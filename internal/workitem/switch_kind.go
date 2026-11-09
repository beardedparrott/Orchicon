package workitem

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Kind-switch resolution (ADR-WIT-1/2): switching a work item's kind can break three invariants —
// the parent invariant (a child must be strictly deeper than its parent).

var (
	// ErrKindSwitchRunning is returned when the item is currently executing
	// (running/checkpointing/recovering) — it must not be re-typed mid-flight.
	ErrKindSwitchRunning = errors.New("cannot switch the kind of a work item that is currently running, checkpointing, or recovering")
	// ErrKindSwitchActiveRun is returned when an active (non-terminal)
	// workflow run is attached to the item.
	ErrKindSwitchActiveRun = errors.New("cannot switch the kind of a work item with an active workflow run")
)

// ChildReparent is a single child move decided by kind-switch resolution: ChildID (with its version
// for optimistic concurrency) moves under NewParentID (nil = top-level.
type ChildReparent struct {
	ChildID      string
	ChildVersion int
	NewParentID  *string
}

// KindSwitchPlan is the set of mutations ResolveKindSwitch decided.
type KindSwitchPlan struct {
	// NewParentID is the resolved parent for the switched item: nil = top-level (epic).
	NewParentID *string
	// NewStatus, when non-nil, is the adjusted status (ready/assigned/
	// scheduled → pending when the new kind is not schedulable).
	NewStatus *string
	// ClearWorkerRef clears assigned_worker_ref (non-schedulable kinds).
	ClearWorkerRef bool
	// ClearScheduledStartAt clears scheduled_start_at (non-schedulable).
	ClearScheduledStartAt bool
	// ClearRecurringSchedule clears recurring_schedule and next_run_at
	// (non-schedulable kinds).
	ClearRecurringSchedule bool
	// ReparentedChildren are the direct children that can no longer sit
	// under the switched item and must move to its resolved parent.
	ReparentedChildren []ChildReparent
}

// ResolveKindSwitch validates and resolves a kind switch for the work item `current`, returning the
// mutations to apply.
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
			// The requested parent is the item's current parent.
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
			resolvedParent, err = nearestShallowerAncestor(ctx, tx, tenantID, *current.ParentID, newDepth, projectID)
			if err != nil {
				return nil, err
			}
		}
		if resolvedParent == nil {
			// DELIBERATELY NOT exempted for an ephemeral item (unlike the create/update paths' top-level
			// exemption in ValidateParent).
			return nil, fmt.Errorf("a %s must have a parent; choose one explicitly", kind)
		}
	}
	plan.NewParentID = resolvedParent

	// 2.
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
		plan.ClearRecurringSchedule = true
		switch current.Status {
		case domain.WorkItemReady, domain.WorkItemAssigned, domain.WorkItemScheduled,
			domain.WorkItemRecurring:
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

// nearestShallowerAncestor walks up the ancestor chain starting from parentID and returns the
// nearest ancestor whose depth is strictly less than newDepth.
func nearestShallowerAncestor(ctx context.Context, tx pgx.Tx, tenantID, parentID string, newDepth int, projectID string) (*string, error) {
	cur := parentID
	for {
		parent, err := db.GetWorkItem(ctx, tx, tenantID, cur)
		if err != nil {
			return nil, err
		}
		if parent.ProjectID != projectID {
			return nil, fmt.Errorf("ancestor %s is not in project %s; choose the new parent explicitly", parent.ID, projectID)
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

// workflowRunActive reports whether the workflow run exists and is not in a terminal state
// (completed/failed/aborted — same check pattern as the auto-start guard in service.go).
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
