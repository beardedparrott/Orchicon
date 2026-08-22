package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// toolLogger is the package-level logger used for post-commit side effects
// (auto-start workflow) that need a logger but receive none via the ToolFn
// signature. Initialized in NewToolRegistry(pool, log); falls back to
// slog.Default() when a nil logger is passed.
var toolLogger *slog.Logger

var numberRefRe = regexp.MustCompile(`(?i)(?:number|item|#)\s*(\d+)`)

func toolListWorkItems(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
		Search    string `json:"search"`
	}
	if len(args) > 0 && string(args) != "null" {
		json.Unmarshal(args, &params)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	items, err := db.ListWorkItems(ctx, ttx.Tx, db.ListWorkItemsFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
		Status:    params.Status,
		Search:    params.Search,
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(items)
}

func toolGetWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	item, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(item)
}

func toolCreateWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID          string   `json:"project_id"`
		Title              string   `json:"title"`
		Kind               string   `json:"kind"`
		ParentID           string   `json:"parent_id"`
		Priority           int      `json:"priority"`
		Description        string   `json:"description"`
		AcceptanceCriteria string   `json:"acceptance_criteria"`
		Budgets            string   `json:"budgets"`
		ContextWindow      int      `json:"context_window"`
		WorkflowID         string   `json:"workflow_id"`
		ScheduledStartAt   string   `json:"scheduled_start_at"`
		AutoStartWorkflow  bool     `json:"auto_start_workflow"`
		RuntimeImage       string   `json:"runtime_image"`
		ContextFiles       []string `json:"context_files"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	// Shared validators (AGENTS.md: the tool and the service cannot drift) —
	// trim + size bounds + JSON validation at the MCP boundary.
	title, err := workitem.ValidateTitle(params.Title)
	if err != nil {
		return nil, err
	}
	description, err := workitem.ValidateDescription(params.Description)
	if err != nil {
		return nil, err
	}
	acceptanceCriteria, err := workitem.ValidateAcceptanceCriteria(params.AcceptanceCriteria)
	if err != nil {
		return nil, err
	}
	budgets, err := workitem.ValidateBudgets(params.Budgets)
	if err != nil {
		return nil, err
	}
	if err := contextfiles.Validate(params.ContextFiles); err != nil {
		return nil, err
	}
	contextFiles, err := contextfiles.ToJSON(params.ContextFiles)
	if err != nil {
		return nil, err
	}
	kind := params.Kind
	if kind == "" {
		kind = domain.WorkItemKindTask
	} else {
		normalized, err := domain.NormalizeWorkItemKind(kind)
		if err != nil {
			return nil, err
		}
		kind = normalized
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	// Only active projects may host work items (same gate as the Connect
	// Create handler).
	if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, params.ProjectID); err != nil {
		return nil, fmt.Errorf("project not active: %w", err)
	}
	// Context files must live inside the project directory (the only path
	// guaranteed mounted where workers run) — same rule as the Connect
	// Create handler.
	if len(params.ContextFiles) > 0 {
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, params.ProjectID); err == nil {
			if err := contextfiles.ValidateWithin(params.ContextFiles, proj.ProjectDir); err != nil {
				return nil, err
			}
		}
	}
	// Enforce hierarchy depth, shared with the Create path.
	if err := workitem.ValidateParent(ctx, ttx.Tx, tenantID, params.ParentID, kind, params.ProjectID); err != nil {
		return nil, err
	}
	var parentID *string
	if params.ParentID != "" {
		parentID = &params.ParentID
	}
	// Parse scheduled start + workflow binding (docs/11 §5.1).
	var scheduledStartAt *time.Time
	if params.ScheduledStartAt != "" {
		t, parseErr := parseScheduledTime(params.ScheduledStartAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse scheduled_start_at: %w", parseErr)
		}
		scheduledStartAt = &t
	}
	// auto_start_workflow is opt-in, never the default (matches the API:
	// "Start immediately on save" must be requested explicitly).
	autoStart := params.AutoStartWorkflow
	workflowID := strings.TrimSpace(params.WorkflowID)
	status := domain.WorkItemPending
	if scheduledStartAt != nil {
		status = domain.WorkItemScheduled
	}
	// Trim the runtime image; the MCP stdio process has no daemon resolver,
	// so empty stays empty and the WorkflowReconciler resolves base at run
	// start (functionally identical to the API's eager stamping).
	runtimeImage := strings.TrimSpace(params.RuntimeImage)
	row := db.WorkItemRow{
		ID:                 db.NewID(),
		TenantID:           tenantID,
		ProjectID:          params.ProjectID,
		ParentID:           parentID,
		Kind:               kind,
		Title:              title,
		Description:        description,
		AcceptanceCriteria: acceptanceCriteria,
		Status:             status,
		Priority:           params.Priority,
		Budgets:            budgets,
		ContextWindow:      params.ContextWindow,
		WorkflowID:         &workflowID,
		ScheduledStartAt:   scheduledStartAt,
		AutoStartWorkflow:  autoStart,
		RuntimeImage:       runtimeImage,
		ContextFiles:       contextFiles,
	}
	if workflowID == "" {
		row.WorkflowID = nil
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, row)
	if err != nil {
		return nil, err
	}
	// Outbox: the MCP path honors invariant #3 for work item mutations.
	if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.created", created); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	// Post-commit auto-start, same guard and call as the Connect Create
	// handler: bound to a workflow, no scheduled time, and auto-start
	// requested — start the run immediately.
	if workflowID != "" && scheduledStartAt == nil && autoStart {
		if err := workflow.StartWorkflowDirect(ctx, pool, toolLogger, tenantID, workflowID, params.ProjectID, created.ID); err != nil {
			toolLogger.Warn("auto-start workflow failed", "work_item", created.ID, "workflow", workflowID, "error", err)
		}
	}
	return json.Marshal(created)
}

func toolUpdateWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	// Pointer-based params: an unset field (nil) is distinct from an
	// explicit zero/empty value, exactly like the proto optional fields
	// the Connect Update handler reads.
	var params struct {
		ID                 string  `json:"id"`
		Title              *string `json:"title"`
		Description        *string `json:"description"`
		AcceptanceCriteria *string `json:"acceptance_criteria"`
		AcceptanceReview   *string `json:"acceptance_review"`
		Status             *string `json:"status"`
		Priority           *int    `json:"priority"`
		Budgets            *string `json:"budgets"`
		ContextWindow      *int    `json:"context_window"`
		ProjectID          *string `json:"project_id"`
		WorkflowID         *string `json:"workflow_id"`
		ParentID           *string `json:"parent_id"`
		ScheduledStartAt   *string `json:"scheduled_start_at"`
		AutoStartWorkflow  *bool   `json:"auto_start_workflow"`
		WorkflowRunID      *string   `json:"workflow_run_id"`
		RuntimeImage       *string   `json:"runtime_image"`
		Kind               *string   `json:"kind"`
		ContextFiles       *[]string `json:"context_files"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	// Context files must live inside the effective project's directory (the
	// only path guaranteed mounted where workers run) — same rule as the
	// Connect Update handler.
	if params.ContextFiles != nil {
		effProject := current.ProjectID
		if params.ProjectID != nil && *params.ProjectID != "" {
			effProject = *params.ProjectID
		}
		if proj, err := db.GetProject(ctx, ttx.Tx, tenantID, effProject); err == nil {
			if err := contextfiles.ValidateWithin(*params.ContextFiles, proj.ProjectDir); err != nil {
				return nil, err
			}
		}
	}
	update := db.UpdateWorkItemFields{}
	if params.Title != nil {
		title, err := workitem.ValidateTitle(*params.Title)
		if err != nil {
			return nil, err
		}
		update.Title = &title
	}
	if params.Description != nil {
		desc, err := workitem.ValidateDescription(*params.Description)
		if err != nil {
			return nil, err
		}
		update.Description = &desc
	}
	if params.AcceptanceCriteria != nil {
		ac, err := workitem.ValidateAcceptanceCriteria(*params.AcceptanceCriteria)
		if err != nil {
			return nil, err
		}
		update.AcceptanceCriteria = &ac
	}
	if params.AcceptanceReview != nil {
		ar, err := workitem.ValidateAcceptanceReview(*params.AcceptanceReview)
		if err != nil {
			return nil, err
		}
		update.AcceptanceReview = &ar
	}
	if params.Status != nil {
		status, err := workitem.ValidateStatus(*params.Status)
		if err != nil {
			return nil, err
		}
		update.Status = &status
	}
	if params.Priority != nil {
		update.Priority = params.Priority
	}
	if params.Budgets != nil {
		budgets, err := workitem.ValidateBudgets(*params.Budgets)
		if err != nil {
			return nil, err
		}
		update.Budgets = &budgets
	}
	if params.ContextWindow != nil {
		update.ContextWindow = params.ContextWindow
	}
	if params.ProjectID != nil {
		update.ProjectID = params.ProjectID
	}
	if params.WorkflowID != nil {
		wfid := *params.WorkflowID
		update.WorkflowID = &wfid
	}
	if params.ParentID != nil {
		update.ParentID = params.ParentID
	}
	// scheduled_start_at parses the same user-friendly strings as the
	// schedule_work_item convenience tool (ISO 8601 or "N minutes from now").
	var scheduledStartAt *time.Time
	if params.ScheduledStartAt != nil {
		t, parseErr := parseScheduledTime(*params.ScheduledStartAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse scheduled_start_at: %w", parseErr)
		}
		scheduledStartAt = &t
		update.ScheduledStartAt = &t
	}
	if params.AutoStartWorkflow != nil {
		v := *params.AutoStartWorkflow
		update.AutoStartWorkflow = &v
		if v && params.ScheduledStartAt == nil {
			update.ClearScheduledStartAt = true
		}
	}
	if params.WorkflowRunID != nil {
		v := *params.WorkflowRunID
		update.WorkflowRunID = &v
	}
	if params.RuntimeImage != nil {
		v := strings.TrimSpace(*params.RuntimeImage)
		update.RuntimeImage = &v
	}
	if params.ContextFiles != nil {
		if err := contextfiles.Validate(*params.ContextFiles); err != nil {
			return nil, err
		}
		cfJSON, err := contextfiles.ToJSON(*params.ContextFiles)
		if err != nil {
			return nil, err
		}
		update.ContextFiles = &cfJSON
	}
	// Kind switch uses the SAME shared resolution as the Connect Update
	// path (AGENTS.md: the tool and the service cannot drift) — parent
	// walk-up, child reparenting, and schedulability cleanup are decided
	// in one place, inside this transaction.
	var kindSwitchPlan *workitem.KindSwitchPlan
	if params.Kind != nil {
		kind, err := domain.NormalizeWorkItemKind(*params.Kind)
		if err != nil {
			return nil, err
		}
		if kind != current.Kind {
			effectiveProject := current.ProjectID
			if update.ProjectID != nil && *update.ProjectID != "" {
				effectiveProject = *update.ProjectID
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
				keepCurrent := params.ParentID != nil && current.ParentID != nil && *params.ParentID == *current.ParentID
				explicitForTarget := params.ParentID != nil && !keepCurrent
				if !explicitForTarget {
					carryOK := false
					if keepCurrent {
						ok, err := workitem.WorkItemInProject(ctx, ttx.Tx, tenantID, *current.ParentID, effectiveProject)
						if err != nil {
							return nil, err
						}
						carryOK = ok
					}
					if !carryOK {
						return nil, fmt.Errorf("when switching kind and project together, choose the new parent explicitly")
					}
				}
			}
			// Explicit parent passed through verbatim (including an empty
			// string = "clear the parent"), exactly like the Connect Update.
			plan, err := workitem.ResolveKindSwitch(ctx, ttx.Tx, tenantID, current, kind, params.ParentID, effectiveProject)
			if err != nil {
				return nil, err
			}
			kindSwitchPlan = plan
			update.Kind = &kind
		}
	}
	// If reassigning to a different project, the target must be active.
	if update.ProjectID != nil && *update.ProjectID != current.ProjectID {
		if err := db.RequireProjectActive(ctx, ttx.Tx, tenantID, *update.ProjectID); err != nil {
			return nil, fmt.Errorf("target project not active: %w", err)
		}
	}
	// Reparenting is validated against the *effective* project — the
	// request's project_id if also being changed, otherwise the current
	// one — using the same rules as Create (shared helper). Clearing the
	// parent (empty string) is only valid for epics; a non-epic must keep
	// a parent. When a kind switch is in flight, the explicit parent was
	// already validated against the NEW kind inside ResolveKindSwitch, so
	// this block is skipped (the old kind's rules no longer apply).
	if params.ParentID != nil && kindSwitchPlan == nil {
		effectiveProject := current.ProjectID
		if update.ProjectID != nil && *update.ProjectID != "" {
			effectiveProject = *update.ProjectID
		}
		if err := workitem.ValidateParent(ctx, ttx.Tx, tenantID, *params.ParentID, current.Kind, effectiveProject); err != nil {
			return nil, err
		}
	} else if params.ParentID == nil && update.ProjectID != nil && *update.ProjectID != "" && *update.ProjectID != current.ProjectID && current.ParentID != nil {
		// The item is moving to another project WITHOUT an explicit
		// reparent, so its current parent is carried over. That only
		// stays consistent if the parent already lives in the target
		// project (e.g. the parent was moved first). Otherwise the
		// request must reparent explicitly — reject rather than leave
		// the hierarchy cross-project (AGENTS.md: fix the whole class).
		if err := workitem.ValidateParent(ctx, ttx.Tx, tenantID, *current.ParentID, current.Kind, *update.ProjectID); err != nil {
			return nil, err
		}
	}
	// Apply the kind-switch plan (ADR-WIT-2): resolved parent, status
	// demotion, and schedulability clears.
	if kindSwitchPlan != nil {
		if kindSwitchPlan.NewParentID != nil && (current.ParentID == nil || *current.ParentID != *kindSwitchPlan.NewParentID) {
			update.ParentID = kindSwitchPlan.NewParentID
		} else if kindSwitchPlan.NewParentID == nil && current.ParentID != nil {
			empty := ""
			update.ParentID = &empty
		}
		if kindSwitchPlan.NewStatus != nil {
			update.Status = kindSwitchPlan.NewStatus
		}
		if kindSwitchPlan.ClearWorkerRef {
			update.ClearAssignedWorkerRef = true
		}
		if kindSwitchPlan.ClearScheduledStartAt {
			update.ClearScheduledStartAt = true
		}
		if kindSwitchPlan.ClearRecurringSchedule {
			update.ClearRecurringSchedule = true
		}
	}
	// Saving a scheduled start flips the item to "scheduled" (ADR-001 in
	// architecture-notes/running-workflows-not-showing-in-schedules.md).
	// Applied AFTER the kind-switch plan so a switch to a non-schedulable
	// kind (which clears the schedule and demotes status to pending) always
	// wins. Guarded against in-flight runs: flipping a running item would
	// let ScheduledRunReconciler fire a duplicate run and would misstate an
	// in-flight run as waiting. Clearing a schedule never triggers the flip.
	if scheduledStartAt != nil &&
		!workitem.IsActiveRunStatus(current.Status) &&
		!(kindSwitchPlan != nil && kindSwitchPlan.ClearScheduledStartAt) {
		status := domain.WorkItemScheduled
		update.Status = &status
	}
	// Final-state invariant mirror of the Connect Update handler: switching
	// status to anything other than "recurring" clears the schedule, and a
	// resulting "recurring" status without a schedule is impossible — demote
	// to pending (an empty-but-present recurring_schedule from the edit form
	// would otherwise leave the row recurring with a NULL schedule). The
	// tool cannot SET a schedule, so only the clear/demote direction applies.
	if update.Status != nil && *update.Status != domain.WorkItemRecurring && current.RecurringSchedule != nil {
		update.ClearRecurringSchedule = true
	}
	if update.Status != nil && *update.Status == domain.WorkItemRecurring &&
		!(current.RecurringSchedule != nil && !update.ClearRecurringSchedule) {
		status := domain.WorkItemPending
		update.Status = &status
	}
	// Schedule-time validation (architecture-notes §3): scheduling or
	// auto-starting a workflow-less parent WITH children runs a sequence —
	// the full subtree must be executable (every leaf bound to a runnable
	// workflow, no worker-assigned one-shots). Runs before commit so a
	// rejection leaves nothing scheduled or started. Shared with the
	// Connect path so the two surfaces cannot drift.
	effWorkflow := ""
	if current.WorkflowID != nil {
		effWorkflow = *current.WorkflowID
	}
	if update.WorkflowID != nil {
		effWorkflow = *update.WorkflowID
	}
	if effWorkflow == "" && (scheduledStartAt != nil || (params.AutoStartWorkflow != nil && *params.AutoStartWorkflow)) {
		// Validate against the POST-update workflow binding: an update that
		// unbinds a previously-bound workflow turns a parent-with-children
		// into a sequence, which must be validated too.
		effItem := current
		effItem.WorkflowID = nil
		if err := workitem.ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, effItem); err != nil {
			return nil, err
		}
	}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, update)
	if err != nil {
		return nil, err
	}
	// A kind switch must never trigger the post-commit auto-start on its
	// own — the user re-typed the item, they did not ask to start it now
	// (ADR-WIT-2). Only an explicit autoStartWorkflow=true in the same
	// request still wins.
	kindSwitchInFlight := kindSwitchPlan != nil
	userExplicitlyAutoStarts := params.AutoStartWorkflow != nil && *params.AutoStartWorkflow
	// Outbox (invariant #3): a kind switch emits work_item.kind_changed +
	// work_item.updated for the item and work_item.updated for every
	// reparented child; otherwise a single work_item.updated. All in the
	// same transaction as the mutation.
	if kindSwitchPlan != nil {
		if err := workitem.EnqueueKindChangedEvent(ctx, ttx.Tx, current, updated); err != nil {
			return nil, err
		}
		if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
			return nil, err
		}
		for _, cr := range kindSwitchPlan.ReparentedChildren {
			childFields := db.UpdateWorkItemFields{ParentID: cr.NewParentID}
			child, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, cr.ChildID, cr.ChildVersion, childFields)
			if err != nil {
				return nil, err
			}
			if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", child); err != nil {
				return nil, err
			}
		}
	} else if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.updated", updated); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	// Auto-start workflow after commit, with the same guard the Connect
	// Update handler applies: bound, no scheduled time, auto-start true,
	// not a kind-switch (unless the user explicitly asked), and any prior
	// run is terminal.
	if updated.WorkflowID != nil && *updated.WorkflowID != "" && updated.ScheduledStartAt == nil && updated.AutoStartWorkflow && !(kindSwitchInFlight && !userExplicitlyAutoStarts) {
		shouldStart := true
		if updated.WorkflowRunID != "" {
			var runStatus string
			if err := pool.Pool.QueryRow(ctx, `SELECT status FROM workflow_runs WHERE id = $1 AND tenant_id = $2`, updated.WorkflowRunID, tenantID).Scan(&runStatus); err == nil {
				if runStatus != domain.WorkflowRunCompleted && runStatus != domain.WorkflowRunFailed && runStatus != domain.WorkflowRunAborted {
					shouldStart = false
				}
			}
		}
		if shouldStart {
			if err := workflow.StartWorkflowDirect(ctx, pool, toolLogger, tenantID, *updated.WorkflowID, updated.ProjectID, updated.ID); err != nil {
				toolLogger.Warn("auto-start workflow after update failed", "work_item", updated.ID, "error", err)
			}
		}
	}
	return json.Marshal(updated)
}

// toolAssignWorker assigns a worker to a work item, mirroring the
// AssignWorker RPC: the ref {worker_id, version} is stored in
// assigned_worker_ref and a work_item.worker_assigned outbox event is
// emitted in the same transaction.
func toolAssignWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID       string `json:"id"`
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if params.WorkerID == "" {
		return nil, fmt.Errorf("worker_id is required")
	}
	refJSON, err := json.Marshal(map[string]any{"worker_id": params.WorkerID, "version": params.Version})
	if err != nil {
		return nil, fmt.Errorf("marshal worker ref: %w", err)
	}
	workerRef, err := workitem.ValidateWorkerRef(string(refJSON))
	if err != nil {
		return nil, err
	}
	if workerRef == nil {
		return nil, fmt.Errorf("worker_ref must not be empty for assignment")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	updated, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateWorkItemFields{
		AssignedWorkerRef: &workerRef,
	})
	if err != nil {
		return nil, err
	}
	if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_assigned", updated); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

// toolUnassignWorker clears the worker binding from a work item, mirroring
// the UnassignWorker RPC (status CAS bumping the version, worker
// work_item.worker_unassigned outbox event in the same transaction).
func toolUnassignWorker(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	// Clear assigned_worker_ref with a version CAS (the bytea column
	// cannot encode NULL through the *[]byte pointer — the direct-query
	// approach mirrors UnassignWorker in internal/workitem/service.go).
	const q = `UPDATE work_items
		SET assigned_worker_ref = NULL, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING ` + db.WorkItemSelectCols
	var updated db.WorkItemRow
	err = ttx.Tx.QueryRow(ctx, q, tenantID, params.ID, current.Version).Scan(
		db.WorkItemScanPtrs(&updated)...,
	)
	if err != nil {
		return nil, fmt.Errorf("db: unassign worker: %w", err)
	}
	if err := workitem.EnqueueWorkItemEvent(ctx, ttx.Tx, "work_item.worker_unassigned", updated); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(updated)
}

// toolDeleteWorkItem soft-deletes a work item (status → cancelled), matching
// the DeleteWorkItem RPC the UI uses.
func toolDeleteWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	status := domain.WorkItemCancelled
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateWorkItemFields{
		Status: &status,
	}); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"id": params.ID, "status": domain.WorkItemCancelled})
}

// toolArchiveWorkItem hides a terminal work item from every normal view,
// matching the ArchiveWorkItem RPC the UI uses. Only allowed from a terminal
// status and blocked when the item has children.
func toolArchiveWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if !domain.WorkItemIsTerminalArchivable(current.Status) {
		return nil, fmt.Errorf("work item must be in a terminal state (succeeded, failed, cancelled, or skipped) to be archived; finish or cancel it first")
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, current.ID)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		return nil, fmt.Errorf("cannot archive a work item that has %d child work item(s); archive the children first", len(children))
	}
	archived, err := db.ArchiveWorkItem(ctx, ttx.Tx, tenantID, current.ID, current.Version, current.Status)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(archived)
}

// toolRestoreWorkItem returns an archived work item to the active views,
// back to the terminal status it was archived from, matching the
// RestoreWorkItem RPC the UI uses.
func toolRestoreWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.WorkItemArchived {
		return nil, fmt.Errorf("work item is not archived; only archived work items can be restored")
	}
	fromStatus := domain.WorkItemCancelled
	if current.ArchivedFromStatus != nil && *current.ArchivedFromStatus != "" {
		fromStatus = *current.ArchivedFromStatus
	}
	restored, err := db.RestoreWorkItem(ctx, ttx.Tx, tenantID, current.ID, current.Version, fromStatus)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(restored)
}

// toolScheduleWorkItem sets a work item's status to "scheduled" and
// optionally sets its scheduled_start_at. Accepts a work_item_id and an
// optional scheduled_time (ISO 8601 or "N [minutes|hours] from now").
func toolScheduleWorkItem(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		ScheduledTime string `json:"scheduled_time"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	// A workflow-less parent with children is a sequence — validate the
	// subtree before scheduling it (architecture-notes §3). Shared with
	// the Connect path so the two surfaces cannot drift.
	if current.WorkflowID == nil || *current.WorkflowID == "" {
		if err := workitem.ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
			return nil, err
		}
	}
	update := db.UpdateWorkItemFields{}
	status := params.Status
	if status == "" {
		status = "scheduled"
	}
	update.Status = &status
	// Scheduling contradicts "start immediately" — disable auto-start.
	autoStart := false
	update.AutoStartWorkflow = &autoStart
	if params.ScheduledTime != "" {
		t, parseErr := parseScheduledTime(params.ScheduledTime)
		if parseErr != nil {
			return nil, fmt.Errorf("parse scheduled_time: %w", parseErr)
		}
		update.ScheduledStartAt = &t
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, params.ID, current.Version, update); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"status":           "scheduled",
		"work_item_id":     params.ID,
		"work_item_title":  current.Title,
		"scheduled_start":  params.ScheduledTime,
	})
}

// toolReorderWorkItems renumbers sort_order for the siblings under a
// parent (empty = top level) to the given order, in one transaction —
// mirrors the ReorderWorkItems RPC (architecture-notes/
// sequential-multi-workflow-runs.md §1). Only explicit drags change
// sort_order; display sort never does. Safe while a sequence is running:
// the derived cursor reads sort_order at arm time.
func toolReorderWorkItems(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string   `json:"project_id"`
		ParentID  string   `json:"parent_id"`
		ChildIDs  []string `json:"child_ids"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ProjectID == "" {
		return nil, fmt.Errorf("project_id is required")
	}
	if len(params.ChildIDs) == 0 {
		return nil, fmt.Errorf("child_ids must not be empty")
	}
	seen := make(map[string]bool, len(params.ChildIDs))
	for _, id := range params.ChildIDs {
		if id == "" {
			return nil, fmt.Errorf("child_ids must not contain empty ids")
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate child id %q", id)
		}
		seen[id] = true
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	if params.ParentID != "" {
		parent, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ParentID)
		if err != nil {
			return nil, err
		}
		if parent.ProjectID != params.ProjectID {
			return nil, fmt.Errorf("parent_id work item is not in the specified project")
		}
	}
	siblings, err := db.ListSiblingsForReorder(ctx, ttx.Tx, tenantID, params.ProjectID, params.ParentID)
	if err != nil {
		return nil, err
	}
	if len(siblings) == 0 {
		return nil, fmt.Errorf("no siblings under the given parent")
	}
	byID := make(map[string]db.WorkItemRow, len(siblings))
	for _, sib := range siblings {
		byID[sib.ID] = sib
	}
	for _, id := range params.ChildIDs {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("child %q is not a direct child of the given parent in this project", id)
		}
	}
	ordered := make([]db.WorkItemRow, 0, len(siblings))
	placed := make(map[string]bool, len(siblings))
	for _, id := range params.ChildIDs {
		ordered = append(ordered, byID[id])
		placed[id] = true
	}
	for _, sib := range siblings {
		if !placed[sib.ID] {
			ordered = append(ordered, sib)
		}
	}
	for i, sib := range ordered {
		order := float64(i + 1)
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, sib.ID, sib.Version, db.UpdateWorkItemFields{
			SortOrder: &order,
		}); err != nil {
			return nil, err
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	type reorderedItem struct {
		ID    string  `json:"id"`
		Title string  `json:"title"`
		Order float64 `json:"sort_order"`
	}
	out := make([]reorderedItem, 0, len(ordered))
	for i, sib := range ordered {
		out = append(out, reorderedItem{ID: sib.ID, Title: sib.Title, Order: float64(i + 1)})
	}
	return json.Marshal(map[string]any{"reordered": out, "parent_id": params.ParentID})
}

// toolControlSequence drives a sequence parent manually (start / resume /
// stop) — mirrors the ControlSequence RPC (AGENTS.md Ask-Orchicon-sync
// rule). A parent with children IS a sequence run; these explicit gestures
// are what the engine's derived cursor cannot infer on its own:
//   - start re-fires the chain from child #1 (destructive — every
//     descendant resets to pending).
//   - resume continues from the first non-succeeded child (keeps state).
//   - stop parks the chain (parent → pending, schedule cleared) so
//     children can be run standalone.
func toolControlSequence(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	action := strings.ToLower(strings.TrimSpace(params.Action))
	if action == "" {
		return nil, fmt.Errorf("action is required: start, resume, or stop")
	}
	tenantID := tenant.FromContext(ctx)

	// Sequence-parent guard shared with the RPC: must have children and no
	// bound workflow run. Load inside a read tx to validate.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	current, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		ttx.Rollback(ctx)
		return nil, err
	}
	if current.WorkflowRunID != "" {
		ttx.Rollback(ctx)
		return nil, fmt.Errorf("work item %q is bound to a workflow run — not a sequence parent", params.ID)
	}
	children, err := db.ListDirectChildren(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		ttx.Rollback(ctx)
		return nil, err
	}
	if len(children) == 0 {
		ttx.Rollback(ctx)
		return nil, fmt.Errorf("work item %q has no children — only sequence parents (work items with children) can be started/resumed/stopped", params.ID)
	}

	var outcome string
	switch action {
	case "start":
		if workitem.IsActiveRunStatus(current.Status) {
			ttx.Rollback(ctx)
			return nil, fmt.Errorf("cannot START a sequence that is already running — STOP it first")
		}
		if err := workitem.ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
			ttx.Rollback(ctx)
			return nil, err
		}
		if err := ttx.Commit(ctx); err != nil {
			return nil, err
		}
		if err := scheduler.StartSequence(ctx, pool, toolLogger, tenantID, params.ID,
			func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
				return workflow.StartWorkflowDirect(ctx, pool, toolLogger, tenantID, workflowID, projectID, workItemID)
			}); err != nil {
			return nil, err
		}
		outcome = "started"
	case "resume":
		if current.Status != domain.WorkItemFailed && current.Status != domain.WorkItemPending {
			ttx.Rollback(ctx)
			return nil, fmt.Errorf("cannot RESUME a sequence that is not halted (failed) or parked (pending)")
		}
		if err := workitem.ValidateSequenceSchedule(ctx, ttx.Tx, tenantID, current); err != nil {
			ttx.Rollback(ctx)
			return nil, err
		}
		if err := ttx.Commit(ctx); err != nil {
			return nil, err
		}
		if err := scheduler.ResumeSequence(ctx, pool, toolLogger, tenantID, params.ID,
			func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error {
				return workflow.StartWorkflowDirect(ctx, pool, toolLogger, tenantID, workflowID, projectID, workItemID)
			}); err != nil {
			return nil, err
		}
		outcome = "resumed"
	case "stop":
		if current.Status != domain.WorkItemRunning && current.Status != domain.WorkItemFailed {
			ttx.Rollback(ctx)
			return nil, fmt.Errorf("cannot STOP a sequence that is not running or failed")
		}
		if err := ttx.Commit(ctx); err != nil {
			return nil, err
		}
		if err := scheduler.StopSequence(ctx, pool, toolLogger, tenantID, params.ID); err != nil {
			return nil, err
		}
		outcome = "stopped"
	default:
		ttx.Rollback(ctx)
		return nil, fmt.Errorf("action must be one of start, resume, stop")
	}

	// Re-read the parent after the action for server-confirmed state.
	ttx2, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx2.Rollback(ctx)
	updated, err := db.GetWorkItem(ctx, ttx2.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"action":          outcome,
		"work_item_id":    updated.ID,
		"work_item_title": updated.Title,
		"status":          updated.Status,
	})
}

func parseScheduledTime(s string) (time.Time, error) {
	// Try ISO 8601 first.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Normalize word numbers: "five minutes" → "5 minutes".
	normalized := replaceWordNumbers(s)
	// Try "N [minutes|min|m|hours|hour|h] from now".
	re := regexp.MustCompile(`(?i)(\d+)\s*(minute|min|m|hour|hr|h)\s*(?:from\s*now)?`)
	matches := re.FindStringSubmatch(normalized)
	if len(matches) >= 3 {
		n, _ := strconv.Atoi(matches[1])
		unit := strings.ToLower(matches[2])
		var dur time.Duration
		switch unit {
		case "minute", "min", "m":
			dur = time.Duration(n) * time.Minute
		case "hour", "hr", "h":
			dur = time.Duration(n) * time.Hour
		default:
			return time.Time{}, fmt.Errorf("unknown time unit: %q", unit)
		}
		return time.Now().UTC().Add(dur), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q (use ISO 8601 or 'N minutes from now')", s)
}

// wordNumberMap maps spelled-out numbers to digits for the schedule tool's
// "N minutes from now" parsing ("five minutes" → "5 minutes").
var wordNumberMap = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12",
}

func wordNumberPattern() string {
	words := make([]string, 0, len(wordNumberMap))
	for w := range wordNumberMap {
		words = append(words, w)
	}
	sort.Strings(words)
	return strings.Join(words, "|")
}

func replaceWordNumbers(s string) string {
	// Replace word numbers that precede a time unit, e.g. "five minutes" → "5 minutes".
	re := regexp.MustCompile(`(?i)(` + wordNumberPattern() + `)\s*(minutes?|min|m|hours?|hr|h)`)
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if len(parts) >= 3 {
			if digit, ok := wordNumberMap[strings.ToLower(parts[1])]; ok {
				return digit + " " + parts[2]
			}
		}
		return match
	})
}
