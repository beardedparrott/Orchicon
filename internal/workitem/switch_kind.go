package workitem

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Kind-switch resolution (ADR-WIT-1/2): switching a work item's kind can
// break three invariants — the parent invariant (a child must be strictly
// deeper than its parent), the child invariant (children must be strictly
// deeper than the item), and schedulability (only tasks/subtasks may be
// dispatched). ResolveKindSwitch computes the full set of mutations that
// keep the tree well-formed, and is shared by the Connect UpdateWorkItem
// handler and the Ask Orchicon update_work_item tool so the two paths
// cannot drift (AGENTS.md).
//
// The resolution runs inside the caller's tenant transaction, before the
// item UPDATE, so every mutation (the switch itself, child reparents) is
// enqueued through the transactional outbox in the same atomic unit
// (AGENTS.md invariant #3).

var (
	// ErrKindSwitchRunning is returned when the item is currently
	// executing (running/checkpointing/recovering) — it must not be
	// re-typed mid-flight.
	ErrKindSwitchRunning = errors.New("cannot switch the kind of a work item that is currently running, checkpointing, or recovering")
	// ErrKindSwitchActiveRun is returned when an active (non-terminal)
	// workflow run is attached to the item.
	ErrKindSwitchActiveRun = errors.New("cannot switch the kind of a work item with an active workflow run")
)

// ChildReparent is a single child move decided by kind-switch resolution:
// ChildID (with its version for optimistic concurrency) moves under
// NewParentID (nil = top-level, which cannot actually trigger here — see
// ResolveKindSwitch).
type ChildReparent struct {
	ChildID     string
	ChildVersion int
	NewParentID *string
}

// KindSwitchPlan is the set of mutations ResolveKindSwitch decided. The
// caller applies them to the switched item and each reparented child in
// the same transaction, emitting the corresponding outbox events.
type KindSwitchPlan struct {
	// NewParentID is the resolved parent for the switched item:
	// nil = top-level (epic). When the item keeps its current parent it is
	// still set to that parent id so the caller can compare and decide.
	NewParentID *string
	// NewStatus, when non-nil, is the adjusted status (ready/assigned/
	// scheduled → pending when the new kind is not schedulable).
	NewStatus *string
	// ClearWorkerRef clears assigned_worker_ref (non-schedulable kinds).
	ClearWorkerRef bool
	// ClearScheduledStartAt clears scheduled_start_at (non-schedulable).
	ClearScheduledStartAt bool
	// ReparentedChildren are the direct children that can no longer sit
	// under the switched item and must move to its resolved parent.
	ReparentedChildren []ChildReparent
}

// ResolveKindSwitch validates and resolves a kind switch for the work
// item `current`, returning the mutations to apply. `explicitParentID`
// is the caller's requested parent (nil = not provided, or equal to the
// current parent — which is treated as "keep" and left to auto-resolution
// because the current parent may no longer be valid for the new kind).
// `projectID` is the effective project the item will live in (the request's
// project_id when also reassigning, otherwise the current one); explicit
// parents and the walk-up are validated against it.
//
// Preconditions (rejected with sentinel errors the caller maps to
// CodeFailedPrecondition):
//   - the item's status is system-managed (running/checkpointing/
//     recovering);
//   - an active workflow run is attached (workflow_run_id non-empty and
//     the run not in a terminal state — same check pattern as the
//     auto-start guard in service.go).
//
// Resolution (given newDepth = kindOrder[newKind]):
//  1. Parent side:
//     - newKind == epic → parent_id = NULL (epics are top-level).
//     - else keep the current parent when it is strictly shallower than
//       newDepth; otherwise walk up the ancestor chain to the nearest
//       ancestor shallower than newDepth.
//     - no parent + non-epic → error: a parent must be chosen explicitly.
//  2. Child side: each direct child with depth(child) <= newDepth is
//     reparented to the item's resolved parent (they become siblings).
//  3. Schedulability: switching to epic/feature clears the worker
//     binding and scheduled start, and demotes ready/assigned/scheduled
//     to pending so ListReadyTasks can never dispatch a re-typed item.
func ResolveKindSwitch(ctx context.Context, tx pgx.Tx, tenantID string, current db.WorkItemRow, newKind string, explicitParentID *string, projectID string) (*KindSwitchPlan, error) {
	kind, err := domain.NormalizeWorkItemKind(newKind)
	if err != nil {
		return nil, err
	}
	newDepth, ok := kindOrder[kind]
	if !ok {
		return nil, fmt.Errorf("invalid work item kind %q", kind)
	}

	// Preconditions: a running item or an active run must not be re-typed.
	switch current.Status {
	case domain.WorkItemRunning, domain.WorkItemCheckpointing, domain.WorkItemRecovering:
		return nil, ErrKindSwitchRunning
	}
	active, err := workflowRunActive(ctx, tx, tenantID, current.WorkflowRunID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, ErrKindSwitchActiveRun
	}

	plan := &KindSwitchPlan{}

	// 1. Parent side.
	var resolvedParent *string
	switch kind {
	case domain.WorkItemKindEpic:
		resolvedParent = nil
	default:
		explicit := explicitParentID
		if explicit != nil && current.ParentID != nil && *explicit == *current.ParentID {
			// The requested parent is the item's current parent. For the
			// new kind that parent may no longer be shallower (e.g. a
			// Task under a Feature switched to Feature) — auto-resolution
			// is the point of a kind switch, so treat this as "keep".
			explicit = nil
		}
		switch {
		case explicit != nil:
			if *explicit == current.ID {
				return nil, fmt.Errorf("a work item cannot be its own parent")
			}
			if err := ValidateParent(ctx, tx, tenantID, *explicit, kind, projectID); err != nil {
				return nil, err
			}
			resolvedParent = explicit
		case current.ParentID != nil:
			resolvedParent, err = nearestShallowerAncestor(ctx, tx, tenantID, *current.ParentID, newDepth)
			if err != nil {
				return nil, err
			}
		}
		if resolvedParent == nil {
			return nil, fmt.Errorf("a %s must have a parent; choose one explicitly", kind)
		}
	}
	plan.NewParentID = resolvedParent

	// 2. Child side: direct children that would no longer be strictly
	// deeper than the item move under the item's resolved parent. This is
	// defensive for well-formed trees but reachable (e.g. a Task with a
	// Subtask child switched to Subtask).
	children, err := db.ListDirectChildren(ctx, tx, tenantID, current.ID)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		if kindOrder[child.Kind] <= newDepth {
			plan.ReparentedChildren = append(plan.ReparentedChildren, ChildReparent{
				ChildID:      child.ID,
				ChildVersion: child.Version,
				NewParentID:  resolvedParent,
			})
		}
	}

	// 3. Schedulability cleanup for non-schedulable kinds.
	if !isSchedulableKind(kind) {
		plan.ClearWorkerRef = true
		plan.ClearScheduledStartAt = true
		switch current.Status {
		case domain.WorkItemReady, domain.WorkItemAssigned, domain.WorkItemScheduled:
			s := domain.WorkItemPending
			plan.NewStatus = &s
		}
	}

	return plan, nil
}

// isSchedulableKind reports whether a kind can be dispatched by the
// TaskReconciler (only tasks and subtasks — docs/02 §2.2).
func isSchedulableKind(kind string) bool {
	return kind == domain.WorkItemKindTask || kind == domain.WorkItemKindSubtask
}

// nearestShallowerAncestor walks up the ancestor chain starting from
// parentID and returns the nearest ancestor whose depth is strictly less
// than newDepth. The chain is strictly shallower, so an ancestor always
// exists while the item has any parent (the topmost epic is depth 1 and
// newDepth >= 2 here — newDepth == 1 is handled by the epic branch).
func nearestShallowerAncestor(ctx context.Context, tx pgx.Tx, tenantID, parentID string, newDepth int) (*string, error) {
	cur := parentID
	for {
		parent, err := db.GetWorkItem(ctx, tx, tenantID, cur)
		if err != nil {
			return nil, err
		}
		if kindOrder[parent.Kind] < newDepth {
			return &parent.ID, nil
		}
		if parent.ParentID == nil {
			return nil, fmt.Errorf("no ancestor shallower than depth %d found", newDepth)
		}
		cur = *parent.ParentID
	}
}

// workflowRunActive reports whether the workflow run exists and is not in
// a terminal state (completed/failed/aborted — same check pattern as the
// auto-start guard in service.go). A missing run is treated as inactive.
func workflowRunActive(ctx context.Context, tx pgx.Tx, tenantID, runID string) (bool, error) {
	if runID == "" {
		return false, nil
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1 AND tenant_id = $2`, runID, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: read workflow run status: %w", err)
	}
	return status != domain.WorkflowRunCompleted && status != domain.WorkflowRunFailed && status != domain.WorkflowRunAborted, nil
}
