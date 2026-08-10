package workitem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/project"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StartWorkflowStarter starts a workflow run for a bound work item.
// Injected by the server wired to the workflow service.
type StartWorkflowStarter func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error

// StartSequenceStarter starts a sequence run for a parent work item with
// children (architecture-notes/sequential-multi-workflow-runs.md §2.3):
// flips the parent to running, resets every descendant to pending, and
// arms the first child in sort_order. Injected by the server wired to the
// sequence engine; validation of the subtree runs BEFORE it is called.
type StartSequenceStarter func(ctx context.Context, tenantID, parentID string) error

// RuntimeImageResolver resolves the base runtime image tag (or "" when no
// daemon is configured). Injected by the server so the backend can stamp
// the work item's runtime_image default (AGENTS.md: the default is a
// concrete stored value, never a UI-only affordance).
type RuntimeImageResolver func(ctx context.Context) string

// Service implements the WorkItemService Connect handler
// (apiv1connect.WorkItemServiceHandler). Each mutation writes an outbox
// row in the same transaction as the state change (AGENTS.md invariant
// #3). Optimistic concurrency is enforced via the version column
// (docs/09 §5). Dependency cycles are rejected at admission using a
// recursive CTE (docs/09 §11).
type Service struct {
	pool            *db.Pool
	log             *slog.Logger
	startWorkflowFn StartWorkflowStarter
	sequenceStartFn StartSequenceStarter
	runtimeImageFn  RuntimeImageResolver
	apiv1connect.UnimplementedWorkItemServiceHandler
}

// Compile-time assertion.
var _ apiv1connect.WorkItemServiceHandler = (*Service)(nil)

// New constructs a WorkItemService handler.
func New(pool *db.Pool, log *slog.Logger) *Service {
	return &Service{pool: pool, log: log}
}

// SetStartWorkflowStarter injects the function to start a bound workflow run.
// Called by the server before the reconciler starts (docs/11 §5.2).
func (s *Service) SetStartWorkflowStarter(fn StartWorkflowStarter) { s.startWorkflowFn = fn }

// SetStartSequenceStarter injects the function that starts a sequence run
// for a parent work item with children (auto-start / run-instant path).
// Called by the server before the reconciler starts.
func (s *Service) SetStartSequenceStarter(fn StartSequenceStarter) { s.sequenceStartFn = fn }

// SetRuntimeImageResolver injects the function that resolves the base
// runtime image tag, used to stamp the work item's runtime_image default.
func (s *Service) SetRuntimeImageResolver(fn RuntimeImageResolver) { s.runtimeImageFn = fn }

// validateContextFilesInput validates a request's context_files list and
// marshals it to the JSONB column value. A nil/empty request list yields
// the empty JSON array ("[]") so a work item created without context has
// a defined value (mirrors the project service's contextFilesToJSON).
func validateContextFilesInput(paths []string) ([]byte, error) {
	if err := contextfiles.Validate(paths); err != nil {
		return nil, err
	}
	return contextfiles.ToJSON(paths)
}

// CreateWorkItem creates a new work item within a project. Depth is
// constrained to 4 levels (docs/02 §2.2).
func (s *Service) CreateWorkItem(ctx context.Context, req *connect.Request[apiv1.CreateWorkItemRequest]) (*connect.Response[apiv1.CreateWorkItemResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	kind, err := validateKind(msg.Kind)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	title, err := ValidateTitle(msg.Title)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	description, err := ValidateDescription(msg.Description)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	acceptanceCriteria, err := ValidateAcceptanceCriteria(msg.AcceptanceCriteria)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	budgets, err := ValidateBudgets(msg.Budgets)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	contextFiles, err := validateContextFilesInput(msg.ContextFiles)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	var parentID *string
	if msg.ParentId != "" {
		parentID = &msg.ParentId
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Only active projects may host work items (docs/02 §2.1).
	if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, msg.ProjectId); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("project not active: %w", err))
	}

	// Context files must live inside the project's directory — the only path
	// guaranteed to be mounted where workers run. A path outside it is
	// invisible to the worker, so reject it.
	if len(msg.ContextFiles) > 0 {
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, msg.ProjectId); err == nil {
			if err := contextfiles.ValidateWithin(msg.ContextFiles, proj.ProjectDir); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}

	// Enforce hierarchy depth: a subtask's parent must be a task, etc.
	// Shared with the Update path so the two cannot drift.
	if err := ValidateParent(ctx, ttx.Tx, tenantID, msg.ParentId, kind, msg.ProjectId); err != nil {
		return nil, mapParentError(err)
	}

	// Parse scheduled start and workflow binding fields (docs/11 §5.1).
	var scheduledStartAt *time.Time
	if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		scheduledStartAt = &t
	}
	// auto_start_workflow defaults to false when unset (proto optional) —
	// "Start immediately on save" is opt-in, never the default (bug fix:
	// the old true default kicked off runs on every create/save).
	autoStart := false
	if msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow {
		autoStart = true
	}
	workflowID := msg.WorkflowId
	if workflowID == "" {
		workflowID = "" // keep empty for unbound items
	}

	status := domain.WorkItemPending
	if scheduledStartAt != nil {
		status = domain.WorkItemScheduled
	}
	row := db.WorkItemRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		ProjectID:          msg.ProjectId,
		ParentID:           parentID,
		Kind:               kind,
		Title:              title,
		Description:        description,
		AcceptanceCriteria: acceptanceCriteria,
		Status:             status,
		Priority:           int(msg.Priority),
		Budgets:            budgets,
		ContextWindow:      int(msg.ContextWindow),
		WorkflowID:         &workflowID,
		ScheduledStartAt:   scheduledStartAt,
		AutoStartWorkflow:  autoStart,
		ContextFiles:       contextFiles,
	}
	// Stamp the runtime image: the caller's choice wins; empty = the base
	// image (resolved from the daemon). The value is stored concretely so
	// it carries forward to the workflow run regardless of when it fires.
	runtimeImage := strings.TrimSpace(msg.RuntimeImage)
	if runtimeImage == "" && s.runtimeImageFn != nil {
		runtimeImage = s.runtimeImageFn(ctx)
	}
	row.RuntimeImage = runtimeImage
	if workflowID == "" {
		row.WorkflowID = nil
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, row)
	if err != nil {
		return nil, mapDBError(err)
	}
	// No schedule-time validation at create: a freshly created item has no
	// children (so the sequence case can't apply), and auto_start_workflow
	// on a workflow-less item is a stored preference — nothing fires until
	// a workflow is bound (the auto-start fire path below no-ops for a
	// workflow-less leaf). Scheduling/run-immediately on an UPDATE validates
	// and rejects a workflow-less leaf there.
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.created", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}

	// If the work item should auto-start immediately. A parent with
	// children wins: it runs as a SEQUENCE even when it carries a workflow
	// binding (children each run their own workflows); only a workflow-bound
	// LEAF starts its own run.
	if scheduledStartAt == nil && autoStart {
		if s.itemHasChildren(ctx, tenantID, created.ID) {
			s.maybeStartSequence(ctx, tenantID, created)
		} else if workflowID != "" && s.startWorkflowFn != nil {
			if err := s.startWorkflowFn(ctx, tenantID, workflowID, msg.ProjectId, created.ID); err != nil {
				s.log.Warn("auto-start workflow failed", "work_item", created.ID, "workflow", workflowID, "error", err)
			}
		}
	}

	s.log.Info("work item created", "id", created.ID, "kind", kind, "project", msg.ProjectId)
	return connect.NewResponse(&apiv1.CreateWorkItemResponse{WorkItem: rowToProto(created)}), nil
}

// GetWorkItem returns a single work item by id.
func (s *Service) GetWorkItem(ctx context.Context, req *connect.Request[apiv1.GetWorkItemRequest]) (*connect.Response[apiv1.GetWorkItemResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	w, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: rowToProto(w)}), nil
}

// ListWorkItems returns a page of work items for a project, optionally
// filtered by parent (tree) or status (Kanban).
func (s *Service) ListWorkItems(ctx context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Empty project_id = list across all projects (for "All" filter).
	f := db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  int(req.Msg.PageSize),
		AfterID:   req.Msg.PageToken,
		Search:    req.Msg.Search,
		SortBy:    req.Msg.SortBy,
		SortOrder: req.Msg.SortOrder,
	}
	if req.Msg.ParentId != nil {
		pid := *req.Msg.ParentId
		f.ParentID = &pid
	}
	if req.Msg.Status != nil {
		f.Status = validateStatus(*req.Msg.Status)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, f)
	if err != nil {
		return nil, mapDBError(err)
	}
	resp := &apiv1.ListWorkItemsResponse{}
	for _, w := range items {
		resp.WorkItems = append(resp.WorkItems, rowToProto(w))
	}
	if len(items) > 0 {
		resp.NextPageToken = items[len(items)-1].ID
	}
	return connect.NewResponse(resp), nil
}

// UpdateWorkItem applies a partial update with optimistic concurrency
// (docs/09 §5). Only non-nil fields are written (field-mask semantics).
func (s *Service) UpdateWorkItem(ctx context.Context, req *connect.Request[apiv1.UpdateWorkItemRequest]) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	var fields db.UpdateWorkItemFields
	if msg.Title != nil {
		title, err := ValidateTitle(*msg.Title)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Title = &title
	}
	if msg.Description != nil {
		desc, err := ValidateDescription(*msg.Description)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Description = &desc
	}
	if msg.AcceptanceCriteria != nil {
		ac, err := ValidateAcceptanceCriteria(*msg.AcceptanceCriteria)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.AcceptanceCriteria = &ac
	}
	if msg.AcceptanceReview != nil {
		ar, err := ValidateAcceptanceReview(*msg.AcceptanceReview)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.AcceptanceReview = &ar
	}
	if msg.Status != nil {
		fields.Status = strPtr(validateStatus(*msg.Status))
	}
	if msg.Priority != nil {
		fields.Priority = intPtr(int(*msg.Priority))
	}
	if msg.Budgets != nil {
		budgets, err := ValidateBudgets(*msg.Budgets)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.Budgets = &budgets
	}
	if msg.ContextWindow != nil {
		fields.ContextWindow = intPtr(int(*msg.ContextWindow))
	}
	if msg.ProjectId != nil {
		fields.ProjectID = msg.ProjectId
	}
	if msg.WorkflowId != nil {
		wfid := *msg.WorkflowId
		fields.WorkflowID = &wfid
	}
	if msg.ParentId != nil {
		pid := *msg.ParentId
		fields.ParentID = &pid
	}
	if msg.ScheduledStartAt != nil {
		t := msg.ScheduledStartAt.AsTime()
		fields.ScheduledStartAt = &t
	}
	if msg.AutoStartWorkflow != nil {
		v := *msg.AutoStartWorkflow
		fields.AutoStartWorkflow = &v
		if v && msg.ScheduledStartAt == nil {
			fields.ClearScheduledStartAt = true
		}
	}
	if msg.WorkflowRunId != nil {
		v := *msg.WorkflowRunId
		fields.WorkflowRunID = &v
	}
	if msg.RuntimeImage != nil {
		v := strings.TrimSpace(*msg.RuntimeImage)
		if v == "" && s.runtimeImageFn != nil {
			// Empty = reset to the base image.
			v = s.runtimeImageFn(ctx)
		}
		fields.RuntimeImage = &v
	}
	if msg.ContextFiles != nil {
		paths := msg.ContextFiles.Files
		if err := contextfiles.Validate(paths); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		filesJSON, err := contextfiles.ToJSON(paths)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fields.ContextFiles = &filesJSON
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Context files must live inside the effective project's directory — the
	// only path guaranteed to be mounted where workers run. A path outside it
	// is invisible to the worker, so reject it. Uses the NEW project when this
	// request reassigns the item, else the current one.
	if msg.ContextFiles != nil {
		effProject := current.ProjectID
		if fields.ProjectID != nil && *fields.ProjectID != "" {
			effProject = *fields.ProjectID
		}
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, effProject); err == nil {
			if err := contextfiles.ValidateWithin(msg.ContextFiles.Files, proj.ProjectDir); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
	}
	// Kind switch (ADR-WIT-1/2): resolve parent/child + schedulability
	// automatically inside the same transaction. The plan is applied after
	// the shared parent validation below (an explicit parent in the same
	// request is validated against the NEW kind here, not the old one).
	var kindSwitchPlan *KindSwitchPlan
	if msg.Kind != nil {
		newKind, err := validateKind(*msg.Kind)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if newKind != current.Kind {
			effectiveProject := current.ProjectID
			if fields.ProjectID != nil && *fields.ProjectID != "" {
				effectiveProject = *fields.ProjectID
			}
			if effectiveProject != current.ProjectID {
				// Switching kind AND moving to another project: the
				// auto-resolution walk-up follows the current ancestor
				// chain, which must not cross projects. Only a genuine
				// explicit reparent (a parent different from the current
				// one) is validated against the target project by
				// ResolveKindSwitch. Naming no parent, or naming the
				// current parent, leaves the resolution to walk the OLD
				// project's chain — unless the current parent already
				// lives in the target project, the request must choose
				// the new parent explicitly (matches the carried-parent
				// guard below).
				keepCurrent := msg.ParentId != nil && current.ParentID != nil && *msg.ParentId == *current.ParentID
				explicitForTarget := msg.ParentId != nil && !keepCurrent
				if !explicitForTarget {
					carryOK := false
					if keepCurrent {
						ok, err := workItemInProject(ctx, ttx.Tx, tenantID, *current.ParentID, effectiveProject)
						if err != nil {
							return nil, connect.NewError(connect.CodeInternal, err)
						}
						carryOK = ok
					}
					if !carryOK {
						return nil, connect.NewError(connect.CodeInvalidArgument,
							errors.New("when switching kind and project together, choose the new parent explicitly"))
					}
				}
			}
			plan, err := ResolveKindSwitch(ctx, ttx.Tx, tenantID, current, newKind, msg.ParentId, effectiveProject)
			if err != nil {
				return nil, mapKindSwitchError(err)
			}
			kindSwitchPlan = plan
			fields.Kind = &newKind
		}
	}
	// If reassigning to a different project, the target must be active.
	if fields.ProjectID != nil && *fields.ProjectID != current.ProjectID {
		if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, *fields.ProjectID); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target project not active: %w", err))
		}
	}
	// Reparenting is validated against the *effective* project — the
	// request's project_id if also being changed, otherwise the current
	// one — using the same rules as Create (shared helper). Clearing the
	// parent (empty string) is only valid for epics; a non-epic must keep
	// a parent. When a kind switch is in flight, the explicit parent was
	// already validated against the NEW kind inside ResolveKindSwitch, so
	// this block is skipped (the old kind's rules no longer apply).
	if msg.ParentId != nil && kindSwitchPlan == nil {
		effectiveProject := current.ProjectID
		if fields.ProjectID != nil && *fields.ProjectID != "" {
			effectiveProject = *fields.ProjectID
		}
		if err := ValidateParent(ctx, ttx.Tx, tenantID, *msg.ParentId, current.Kind, effectiveProject); err != nil {
			return nil, mapParentError(err)
		}
	} else if msg.ParentId == nil && fields.ProjectID != nil && *fields.ProjectID != "" && *fields.ProjectID != current.ProjectID && current.ParentID != nil {
		// The item is moving to another project WITHOUT an explicit
		// reparent, so its current parent is carried over. That only
		// stays consistent if the parent already lives in the target
		// project (e.g. the parent was moved first). Otherwise the
		// request must reparent explicitly — reject rather than leave
		// the hierarchy cross-project (AGENTS.md: fix the whole class).
		if err := ValidateParent(ctx, ttx.Tx, tenantID, *current.ParentID, current.Kind, *fields.ProjectID); err != nil {
			return nil, mapParentError(err)
		}
	}
	// Apply the kind-switch plan to the update fields (ADR-WIT-2): the
	// resolved parent, the status demotion, and the schedulability clears.
	if kindSwitchPlan != nil {
		if kindSwitchPlan.NewParentID != nil && (current.ParentID == nil || *current.ParentID != *kindSwitchPlan.NewParentID) {
			fields.ParentID = kindSwitchPlan.NewParentID
		} else if kindSwitchPlan.NewParentID == nil && current.ParentID != nil {
			// epic switch: a top-level item has no parent.
			empty := ""
			fields.ParentID = &empty
		}
		if kindSwitchPlan.NewStatus != nil {
			fields.Status = kindSwitchPlan.NewStatus
		}
		if kindSwitchPlan.ClearWorkerRef {
			fields.ClearAssignedWorkerRef = true
		}
		if kindSwitchPlan.ClearScheduledStartAt {
			fields.ClearScheduledStartAt = true
		}
	}
	// Saving a scheduled start flips the item to "scheduled" (ADR-001 in
	// architecture-notes/running-workflows-not-showing-in-schedules.md).
	// Applied AFTER the kind-switch plan so a switch to a non-schedulable
	// kind (which clears the schedule and demotes status to pending) always
	// wins. Guarded against in-flight runs: flipping a running item would
	// let ScheduledRunReconciler fire a duplicate run and would misstate an
	// in-flight run as waiting. Clearing a schedule (form omits the field)
	// never triggers the flip.
	if msg.ScheduledStartAt != nil &&
		!IsActiveRunStatus(current.Status) &&
		!(kindSwitchPlan != nil && kindSwitchPlan.ClearScheduledStartAt) {
		fields.Status = strPtr(domain.WorkItemScheduled)
	}
	// Schedule-time validation (architecture-notes §3): scheduling or
	// Schedule-time validation (architecture-notes §3): scheduling or
	// auto-starting a parent WITH children runs a sequence — the full
	// subtree must be executable (every leaf bound to a runnable workflow,
	// no worker-assigned one-shots). Runs before commit so a rejection
	// leaves nothing scheduled or started. The validation itself decides
	// sequence-ness by HAS CHILDREN (a parent with children is a sequence
	// regardless of a stale workflow binding on the parent), so it runs
	// whenever the user schedules or starts immediately — a leaf with no
	// children is a no-op inside the validator.
	if msg.ScheduledStartAt != nil || (msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow) {
		if err := s.validateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
			return nil, err
		}
	}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	// A kind switch must never trigger the post-commit auto-start on its
	// own — the user re-typed the item, they did not ask to start it now
	// (ADR-WIT-2). This holds for every switch: to a non-schedulable kind
	// (which clears the schedule) AND between schedulable kinds (e.g.
	// Task → Subtask, where no schedule is cleared so the old guard missed
	// it — the destructive bug). Only an explicit autoStartWorkflow=true in
	// the same request still wins.
	kindSwitchInFlight := kindSwitchPlan != nil
	userExplicitlyAutoStarts := msg.AutoStartWorkflow != nil && *msg.AutoStartWorkflow
	// Kind switch events: work_item.kind_changed for the switched item
	// (the authoritative record of the switch, carrying old_kind +
	// new_kind), work_item.updated for the item itself (covers any other
	// fields changed in the same request — the kind_changed event is the
	// authoritative switch record but consumers of work_item.updated
	// still see the item), and work_item.updated for every reparented
	// child — all in the same transaction (invariant #3).
	if kindSwitchPlan != nil {
		if err := enqueueKindChangedEvent(ctx, ttx.Tx, current, updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, cr := range kindSwitchPlan.ReparentedChildren {
			childFields := db.UpdateWorkItemFields{ParentID: cr.NewParentID}
			child, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, cr.ChildID, cr.ChildVersion, childFields)
			if err != nil {
				return nil, mapDBError(err)
			}
			if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", child); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		}
	} else if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	// Auto-start immediately if requested and no scheduled time. A parent
	// with children wins: it runs as a SEQUENCE even if it still carries a
	// workflow binding (children each run their own workflows — the subtree
	// was validated in-tx). Only a workflow-bound LEAF starts its own run.
	if updated.ScheduledStartAt == nil && updated.AutoStartWorkflow && !(kindSwitchInFlight && !userExplicitlyAutoStarts) {
		if s.itemHasChildren(ctx, tenantID, updated.ID) {
			s.maybeStartSequence(ctx, tenantID, updated)
		} else if updated.WorkflowID != nil && *updated.WorkflowID != "" && s.startWorkflowFn != nil {
			// A previous run must be terminal (completed/failed/aborted) —
			// active runs are not duplicated.
			shouldStart := true
			if updated.WorkflowRunID != "" {
				var runStatus string
				if err := s.pool.Pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1 AND tenant_id = $2`, updated.WorkflowRunID, tenantID).Scan(&runStatus); err == nil {
					if runStatus != domain.WorkflowRunCompleted && runStatus != domain.WorkflowRunFailed && runStatus != domain.WorkflowRunAborted {
						shouldStart = false
					}
				}
			}
			if shouldStart {
				if err := s.startWorkflowFn(ctx, tenantID, *updated.WorkflowID, updated.ProjectID, updated.ID); err != nil {
					s.log.Warn("auto-start workflow after update failed", "work_item", updated.ID, "error", err)
				}
			}
		}
	}
	s.log.Info("work item updated", "id", updated.ID, "version", updated.Version)
	// Context files changed → refresh the container mount manifest now (the
	// work_item.context_files paths are mounted into the single-container
	// instance; waiting for the 30s periodic writer delays new mounts).
	if msg.ContextFiles != nil {
		project.NotifyProjectChanged()
	}
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: rowToProto(updated)}), nil
}

// DeleteWorkItem soft-deletes a work item by setting status to cancelled.
func (s *Service) DeleteWorkItem(ctx context.Context, req *connect.Request[apiv1.DeleteWorkItemRequest]) (*connect.Response[apiv1.DeleteWorkItemResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	fields := db.UpdateWorkItemFields{Status: strPtr(domain.WorkItemCancelled)}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.deleted", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item deleted (cancelled)", "id", updated.ID)
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{WorkItem: rowToProto(updated)}), nil
}

// HardDeleteWorkItem permanently removes a work item. Cascades to its
// dependencies. The outbox emits a work_item.purged event.
func (s *Service) HardDeleteWorkItem(ctx context.Context, req *connect.Request[apiv1.HardDeleteWorkItemRequest]) (*connect.Response[apiv1.HardDeleteWorkItemResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := db.HardDeleteWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.purged", current); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("work item hard-deleted", "id", req.Msg.Id)
	return connect.NewResponse(&apiv1.HardDeleteWorkItemResponse{}), nil
}

// AddDependency adds an edge to the work DAG. Cycles are rejected at
// admission using a recursive CTE (docs/02 §2.2, docs/09 §11).
func (s *Service) AddDependency(ctx context.Context, req *connect.Request[apiv1.AddDependencyRequest]) (*connect.Response[apiv1.AddDependencyResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	if msg.FromId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from_id must not be empty"))
	}
	if msg.ToId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("to_id must not be empty"))
	}
	if msg.FromId == msg.ToId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cannot create a self-dependency"))
	}
	depType, err := validateDependencyType(msg.Type)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// Verify both work items exist and are in the same project.
	fromItem, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.FromId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if fromItem.ProjectID != msg.ProjectId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from_id work item is not in the specified project"))
	}
	toItem, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.ToId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if toItem.ProjectID != msg.ProjectId {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("to_id work item is not in the specified project"))
	}

	// Cycle check: would adding from→to create a cycle? Traverse
	// forward from `to` — if `from` is reachable, the edge closes a
	// cycle (docs/09 §11: recursive CTE).
	createsCycle, err := db.CheckCycleWithRecursiveCTE(ctx, ttx.Tx, tenantID, msg.ProjectId, msg.FromId, msg.ToId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if createsCycle {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("adding this dependency would create a cycle in the work DAG"))
	}

	dep := db.DependencyRow{
		ID:       db.NewID(),
		TenantID: tenantID,
		ProjectID: msg.ProjectId,
		FromID:   msg.FromId,
		ToID:     msg.ToId,
		Type:     depType,
	}
	created, err := db.CreateDependency(ctx, ttx.Tx, dep)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueDependencyEvent(ctx, ttx.Tx, "work_item.dependency_added", created); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("dependency added", "from", created.FromID, "to", created.ToID, "type", depType)
	return connect.NewResponse(&apiv1.AddDependencyResponse{Dependency: depRowToProto(created)}), nil
}

// RemoveDependency removes an edge from the work DAG.
func (s *Service) RemoveDependency(ctx context.Context, req *connect.Request[apiv1.RemoveDependencyRequest]) (*connect.Response[apiv1.RemoveDependencyResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	if err := db.DeleteDependency(ctx, ttx.Tx, tenantID, req.Msg.Id); err != nil {
		return nil, mapDBError(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("dependency removed", "id", req.Msg.Id)
	return connect.NewResponse(&apiv1.RemoveDependencyResponse{}), nil
}

// GetDependencyGraph returns the full DAG (nodes + edges) for a
// project. Used by the frontend's read-only React Flow graph (docs/10).
func (s *Service) GetDependencyGraph(ctx context.Context, req *connect.Request[apiv1.GetDependencyGraphRequest]) (*connect.Response[apiv1.GetDependencyGraphResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: req.Msg.ProjectId,
		PageSize:  1000,
	})
	if err != nil {
		return nil, mapDBError(err)
	}
	deps, err := db.ListDependencies(ctx, ttx.Tx, tenantID, req.Msg.ProjectId)
	if err != nil {
		return nil, mapDBError(err)
	}
	graph := &apiv1.DependencyGraph{}
	for _, w := range items {
		graph.Nodes = append(graph.Nodes, rowToProto(w))
	}
	for _, d := range deps {
		graph.Edges = append(graph.Edges, depRowToProto(d))
	}
	return connect.NewResponse(&apiv1.GetDependencyGraphResponse{Graph: graph}), nil
}

// AssignWorker binds a Worker (id + version) to a Task/Subtask
// (docs/02 §2.2).
func (s *Service) AssignWorker(ctx context.Context, req *connect.Request[apiv1.AssignWorkerRequest]) (*connect.Response[apiv1.AssignWorkerResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	workerRef, err := ValidateWorkerRef(req.Msg.WorkerRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if workerRef == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("worker_ref must not be empty for assignment"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	fields := db.UpdateWorkItemFields{AssignedWorkerRef: &workerRef}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id, current.Version, fields)
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_assigned", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker assigned to work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.AssignWorkerResponse{WorkItem: rowToProto(updated)}), nil
}

// UnassignWorker removes the worker binding from a Task/Subtask.
func (s *Service) UnassignWorker(ctx context.Context, req *connect.Request[apiv1.UnassignWorkerRequest]) (*connect.Response[apiv1.UnassignWorkerResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id must not be empty"))
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, req.Msg.Id)
	if err != nil {
		return nil, mapDBError(err)
	}
	// Clear assigned_worker_ref with a status CAS, bumping the version
	// (docs/09 §5). The data-access layer's UpdateWorkItemFields treats
	// AssignedWorkerRef as a non-nil pointer to a bytea value, which
	// cannot encode NULL. So we use a direct CAS query for unassign.
	const q = `UPDATE work_items
		SET assigned_worker_ref = NULL, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, project_id, parent_id, kind, title, description,
			acceptance_criteria, acceptance_review, status, assigned_worker_ref, workflow_id,
			priority, budgets, context_window, sort_order, results, prompt_context, context_files, version, created_at, updated_at`
	var updated db.WorkItemRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, req.Msg.Id, current.Version).Scan(
		&updated.ID, &updated.TenantID, &updated.ProjectID, &updated.ParentID, &updated.Kind, &updated.Title,
		&updated.Description, &updated.AcceptanceCriteria, &updated.AcceptanceReview, &updated.Status, &updated.AssignedWorkerRef,
		&updated.WorkflowID, &updated.Priority, &updated.Budgets, &updated.ContextWindow, &updated.SortOrder, &updated.Results,
		&updated.PromptContext, &updated.ContextFiles, &updated.Version, &updated.CreatedAt, &updated.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("db: unassign worker: %w", err))
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_unassigned", updated); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	s.log.Info("worker unassigned from work item", "id", updated.ID)
	return connect.NewResponse(&apiv1.UnassignWorkerResponse{WorkItem: rowToProto(updated)}), nil
}

// ReorderWorkItems renumbers sort_order for the siblings under parent_id
// (empty = top level) to the given order, in one transaction. Display sort
// (ListWorkItems sort_by) never mutates sort_order — only this RPC does
// (architecture-notes/sequential-multi-workflow-runs.md §1). Safe to call
// while a sequence is running: the sequence cursor is derived from
// sort_order at reconcile time, so a mid-run drag shifts only future
// arming.
func (s *Service) ReorderWorkItems(ctx context.Context, req *connect.Request[apiv1.ReorderWorkItemsRequest]) (*connect.Response[apiv1.ReorderWorkItemsResponse], error) {
	msg := req.Msg
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if msg.ProjectId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("project_id must not be empty"))
	}
	if len(msg.ChildIds) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_ids must not be empty"))
	}
	seen := make(map[string]bool, len(msg.ChildIds))
	for _, id := range msg.ChildIds {
		if id == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("child_ids must not contain empty ids"))
		}
		if seen[id] {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("duplicate child id %q", id))
		}
		seen[id] = true
	}

	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	// The parent must exist (or be top level) and live in the project.
	if msg.ParentId != "" {
		parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, msg.ParentId)
		if err != nil {
			return nil, mapDBError(err)
		}
		if parent.ProjectID != msg.ProjectId {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("parent_id work item is not in the specified project"))
		}
	}
	// Load the current sibling set (sort_order NULLS LAST, created_at).
	siblings, err := db.ListSiblingsForReorder(ctx, ttx.Tx, tenantID, msg.ProjectId, msg.ParentId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if len(siblings) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("no siblings under the given parent"))
	}
	byID := make(map[string]db.WorkItemRow, len(siblings))
	for _, sib := range siblings {
		byID[sib.ID] = sib
	}
	// Every requested child must be a direct child in the same project.
	for _, id := range msg.ChildIds {
		if _, ok := byID[id]; !ok {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("child %q is not a direct child of the given parent in this project", id))
		}
	}
	// New order = requested order first, then any unlisted siblings keep
	// their relative position (a partial drag appends the rest).
	ordered := make([]db.WorkItemRow, 0, len(siblings))
	placed := make(map[string]bool, len(siblings))
	for _, id := range msg.ChildIds {
		ordered = append(ordered, byID[id])
		placed[id] = true
	}
	for _, sib := range siblings {
		if !placed[sib.ID] {
			ordered = append(ordered, sib)
		}
	}
	// Renumber 1..N in one transaction, emitting an outbox event per child.
	for i, sib := range ordered {
		order := float64(i + 1)
		updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, sib.ID, sib.Version, db.UpdateWorkItemFields{
			SortOrder: &order,
		})
		if err != nil {
			return nil, mapDBError(err)
		}
		if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		ordered[i] = updated
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit: %w", err))
	}
	resp := &apiv1.ReorderWorkItemsResponse{}
	for _, sib := range ordered {
		resp.WorkItems = append(resp.WorkItems, rowToProto(sib))
	}
	return connect.NewResponse(resp), nil
}

// --- helpers ---------------------------------------------------------------

// EnqueueWorkItemEvent emits a work item outbox row (work_item.created /
// work_item.updated / work_item.worker_assigned / ...) in the caller's
// transaction — the same atomic-unit guarantee as the Connect handlers
// (invariant #3). Exported so the Ask Orchicon MCP tools honor the outbox
// pattern for work item mutations instead of bypassing it.
func EnqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	return enqueueWorkItemEvent(ctx, tx, eventType, w)
}

// EnqueueKindChangedEvent emits work_item.kind_changed — the authoritative
// record of a kind switch. Exported for the Ask Orchicon MCP update tool so
// a kind switch through the MCP emits the same events as the Connect path.
func EnqueueKindChangedEvent(ctx context.Context, tx pgx.Tx, before, after db.WorkItemRow) error {
	return enqueueKindChangedEvent(ctx, tx, before, after)
}

func enqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	payload, err := buildWorkItemEventPayload(eventType, w)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      w.TenantID,
		EventType:     eventType,
		AggregateType: "work_item",
		AggregateID:   w.ID,
		AggregateVer:  w.Version,
		Payload:       payload,
		OccurredAt:     time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

// enqueueKindChangedEvent emits work_item.kind_changed — the authoritative
// record of a kind switch (ADR-WIT-2). The payload carries old_kind +
// new_kind so webhooks/live views can react distinctly.
func enqueueKindChangedEvent(ctx context.Context, tx pgx.Tx, before, after db.WorkItemRow) error {
	payload, err := buildKindChangedEventPayload(before, after)
	if err != nil {
		return err
	}
	row := db.OutboxRow{
		TenantID:      after.TenantID,
		EventType:     "work_item.kind_changed",
		AggregateType: "work_item",
		AggregateID:   after.ID,
		AggregateVer:  after.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

func enqueueDependencyEvent(ctx context.Context, tx pgx.Tx, eventType string, d db.DependencyRow) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":        d.TenantID,
		"project_id":       d.ProjectID,
		"aggregate_type":   "work_item_dependency",
		"aggregate_id":     d.ID,
		"dependency_id":    d.ID,
		"from_id":          d.FromID,
		"to_id":            d.ToID,
		"type":             d.Type,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal dependency event payload: %w", err)
	}
	row := db.OutboxRow{
		TenantID:      d.TenantID,
		EventType:     eventType,
		AggregateType: "work_item_dependency",
		AggregateID:   d.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}
	return db.EnqueueOutbox(ctx, tx, row)
}

func buildWorkItemEventPayload(eventType string, w db.WorkItemRow) ([]byte, error) {
	evt := map[string]any{
		"event_type":        eventType,
		"tenant_id":         w.TenantID,
		"project_id":        w.ProjectID,
		"work_item_id":      w.ID,
		"aggregate_type":    "work_item",
		"aggregate_id":      w.ID,
		"aggregate_version": w.Version,
		"kind":              w.Kind,
		"title":             w.Title,
		"status":            w.Status,
		"parent_id":         parentIDString(w.ParentID),
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal work item event payload: %w", err)
	}
	return b, nil
}

// buildKindChangedEventPayload builds the payload for work_item.kind_changed,
// carrying old_kind + new_kind alongside the standard aggregate fields.
func buildKindChangedEventPayload(before, after db.WorkItemRow) ([]byte, error) {
	base := map[string]any{
		"event_type":        "work_item.kind_changed",
		"tenant_id":         after.TenantID,
		"project_id":        after.ProjectID,
		"work_item_id":      after.ID,
		"aggregate_type":    "work_item",
		"aggregate_id":      after.ID,
		"aggregate_version": after.Version,
		"kind":              after.Kind,
		"old_kind":          before.Kind,
		"new_kind":          after.Kind,
		"title":             after.Title,
		"status":            after.Status,
		"parent_id":         parentIDString(after.ParentID),
		"occurred_at":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("marshal kind-changed event payload: %w", err)
	}
	return b, nil
}

// parentIDString returns the parent id or "" when the item is top-level.
func parentIDString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// WorkItemInProject reports whether the work item lives in the given
// project. Exported so the Ask Orchicon MCP update tool can apply the same
// kind-switch + project-move guard as the Connect handler. Used by the
// kind-switch early guard: a keep-parent request
// ("explicit parent == current parent") combined with a project move is
// only safe when the current parent already lives in the target project,
// so the auto-resolution walk-up cannot reach back into the old project.
func WorkItemInProject(ctx context.Context, tx pgx.Tx, tenantID, id, projectID string) (bool, error) {
	return workItemInProject(ctx, tx, tenantID, id, projectID)
}

func workItemInProject(ctx context.Context, tx pgx.Tx, tenantID, id, projectID string) (bool, error) {
	var p string
	err := tx.QueryRow(ctx, `SELECT project_id FROM work_items WHERE id = $1 AND tenant_id = $2`, id, tenantID).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: read work item project: %w", err)
	}
	return p == projectID, nil
}

func mapDBError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// mapParentError maps a ValidateParent error to a Connect error: a
// missing parent is NotFound; every hierarchy violation is
// InvalidArgument.
func mapParentError(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errors.New("parent work item not found"))
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

// mapKindSwitchError maps a ResolveKindSwitch error to a Connect error:
// the system-managed preconditions (running item / active run) are
// FailedPrecondition; a missing parent is NotFound; everything else
// (hierarchy violations, missing parent for a non-epic) is
// InvalidArgument.
func mapKindSwitchError(err error) error {
	switch {
	case errors.Is(err, ErrKindSwitchRunning), errors.Is(err, ErrKindSwitchActiveRun):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, db.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("parent work item not found"))
	default:
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
}

// validateSequenceSchedule runs the schedule-time subtree validation when
// the request schedules or auto-starts a parent work item that has
// children and no bound workflow (a sequence). Returns nil when the item
// is not a sequence (has a workflow, or no children) or the subtree is
// valid; returns a rejection error otherwise (nothing starts). Runs inside
// the caller's transaction BEFORE the mutation commits.
func (s *Service) validateSequenceSchedule(ctx context.Context, tx pgx.Tx, tenantID string, item db.WorkItemRow) error {
	return ValidateSequenceSchedule(ctx, tx, tenantID, item)
}

// ValidateSequenceSchedule is the shared schedule-time validation used by
// the Connect Update path and the Ask Orchicon schedule/update tools so
// the two surfaces cannot drift (AGENTS.md Ask-Orchicon-sync rule). It
// returns a Connect InvalidArgument error listing the offending children,
// or nil when the subtree is schedulable.
func ValidateSequenceSchedule(ctx context.Context, tx pgx.Tx, tenantID string, item db.WorkItemRow) error {
	// "Has children" is the sequence determinant: a parent with children IS
	// a sequence run (the ADR's parent-is-the-container model), regardless
	// of whether the parent itself still carries a workflow binding (a stale
	// binding from before the item became a parent must not route it to the
	// bound-run path and skip validation). Children each run their own
	// workflows; the parent's own workflow_id is ignored at fire time.
	children, err := db.ListDirectChildren(ctx, tx, tenantID, item.ID)
	if err != nil {
		return mapDBError(err)
	}
	if len(children) == 0 {
		// Leaf — not a sequence. Scheduling or auto-starting a leaf with no
		// workflow bound has nothing to run: reject at schedule time instead
		// of silently no-oping (the run would never fire).
		if item.WorkflowID == nil || *item.WorkflowID == "" {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("Cannot schedule %q: no workflow is set, so there is nothing to run.", item.Title))
		}
		return nil // a workflow-bound leaf is a normal bound run
	}
	noWorkflow, oneShot, badWorkflow, err := ValidateSequenceSubtree(ctx, tx, tenantID, item)
	if err != nil {
		return mapDBError(err)
	}
	if len(noWorkflow) == 0 && len(oneShot) == 0 && len(badWorkflow) == 0 {
		return nil
	}
	return connect.NewError(connect.CodeInvalidArgument,
		BuildSequenceValidationError(item.Title, noWorkflow, oneShot, badWorkflow))
}

// maybeStartSequence starts a sequence run for a parent with children
// after the mutation commits (auto-start / run-instant path). "Has
// children" is the sequence determinant — a parent with children is a
// sequence even if it still carries a workflow binding (children each run
// their own workflows). A leaf has nothing to run. Validation of the
// subtree ran in-transaction before the commit.
func (s *Service) maybeStartSequence(ctx context.Context, tenantID string, item db.WorkItemRow) {
	if s.sequenceStartFn == nil {
		return
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, item.ID)
	ttx.Rollback(ctx)
	if err != nil || len(children) == 0 {
		return
	}
	if err := s.sequenceStartFn(ctx, tenantID, item.ID); err != nil {
		s.log.Warn("auto-start sequence failed", "parent", item.ID, "error", err)
	}
}

// itemHasChildren reports whether the work item has any direct children —
// the "has children = sequence parent" determinant used to route the
// auto-start fire path.
func (s *Service) itemHasChildren(ctx context.Context, tenantID, itemID string) bool {
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return false
	}
	defer ttx.Rollback(ctx)
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, itemID)
	return err == nil && len(children) > 0
}

func kindToProto(kind string) apiv1.WorkItemKind {
	switch kind {
	case domain.WorkItemKindEpic:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC
	case domain.WorkItemKindFeature:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE
	case domain.WorkItemKindTask:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_TASK
	case domain.WorkItemKindSubtask:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK
	default:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_UNSPECIFIED
	}
}

func statusToProto(status string) apiv1.WorkItemStatus {
	switch status {
	case domain.WorkItemPending:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING
	case domain.WorkItemReady:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_READY
	case domain.WorkItemAssigned:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_ASSIGNED
	case domain.WorkItemRunning:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING
	case domain.WorkItemCheckpointing:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CHECKPOINTING
	case domain.WorkItemSucceeded:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED
	case domain.WorkItemFailed:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_FAILED
	case domain.WorkItemCancelled:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	case domain.WorkItemRecovering:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECOVERING
	case domain.WorkItemScheduled:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_SCHEDULED
	default:
		return apiv1.WorkItemStatus_WORK_ITEM_STATUS_UNSPECIFIED
	}
}

func depTypeToProto(t string) apiv1.DependencyType {
	switch t {
	case domain.DependencyBlocks:
		return apiv1.DependencyType_DEPENDENCY_TYPE_BLOCKS
	case domain.DependencyDependsOn:
		return apiv1.DependencyType_DEPENDENCY_TYPE_DEPENDS_ON
	case domain.DependencyRelatesTo:
		return apiv1.DependencyType_DEPENDENCY_TYPE_RELATES_TO
	default:
		return apiv1.DependencyType_DEPENDENCY_TYPE_UNSPECIFIED
	}
}

func kindForDepth(depth int) string {
	switch depth {
	case 1:
		return domain.WorkItemKindEpic
	case 2:
		return domain.WorkItemKindFeature
	case 3:
		return domain.WorkItemKindTask
	case 4:
		return domain.WorkItemKindSubtask
	default:
		return "unknown"
	}
}

func rowToProto(w db.WorkItemRow) *apiv1.WorkItem {
	p := &apiv1.WorkItem{
		Id:                  w.ID,
		TenantId:            w.TenantID,
		ProjectId:           w.ProjectID,
		Kind:                kindToProto(w.Kind),
		Title:               w.Title,
		Description:         w.Description,
		AcceptanceCriteria:  w.AcceptanceCriteria,
		AcceptanceReview:    w.AcceptanceReview,
		Status:              statusToProto(w.Status),
		Priority:            int32(w.Priority),
		Budgets:             string(w.Budgets),
		ContextWindow:       int32(w.ContextWindow),
		Results:             string(w.Results),
		ContextFiles:        contextFilesFromJSONOrEmpty(w.ContextFiles),
		// PR B (context propagation): carries the composite prompt.
		// Stored as JSONB {"composite": "# Task\n..."} — extract the
		// inner text so the frontend gets plain markdown.
		PromptContext:       extractCompositePrompt(w.PromptContext),
		Version:             int32(w.Version),
		CreatedAt:           timestamppb.New(w.CreatedAt),
		UpdatedAt:           timestamppb.New(w.UpdatedAt),
	}
	if w.ParentID != nil {
		p.ParentId = *w.ParentID
	}
	if w.AssignedWorkerRef != nil {
		p.AssignedWorkerRef = string(w.AssignedWorkerRef)
	}
	if w.WorkflowID != nil {
		p.WorkflowId = *w.WorkflowID
	}
	p.WorkflowRunId = w.WorkflowRunID
	p.WorkflowStepId = w.WorkflowStepID
	if w.ScheduledStartAt != nil {
		p.ScheduledStartAt = timestamppb.New(*w.ScheduledStartAt)
	}
	av := w.AutoStartWorkflow
	p.AutoStartWorkflow = &av
	p.RuntimeImage = w.RuntimeImage
	if w.SortOrder != nil {
		p.SortOrder = *w.SortOrder
	}
	return p
}

func depRowToProto(d db.DependencyRow) *apiv1.WorkItemDependency {
	return &apiv1.WorkItemDependency{
		Id:        d.ID,
		TenantId:  d.TenantID,
		ProjectId: d.ProjectID,
		FromId:    d.FromID,
		ToId:      d.ToID,
		Type:      depTypeToProto(d.Type),
		CreatedAt: timestamppb.New(d.CreatedAt),
	}
}

func extractCompositePrompt(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var pc struct {
		Composite string `json:"composite"`
	}
	if err := json.Unmarshal(raw, &pc); err == nil && pc.Composite != "" {
		return pc.Composite
	}
	return string(raw)
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// contextFilesFromJSONOrEmpty is a best-effort parser for the
// context_files JSONB column. Returns an empty slice on any error so the
// API never crashes on corrupt data (mirrors the project service).
func contextFilesFromJSONOrEmpty(data []byte) []string {
	paths, err := contextfiles.FromJSON(data)
	if err != nil {
		return nil
	}
	return paths
}
