// Package scheduler implements the TaskReconciler — the control loop
// that reconciles desired work-item state with observed runtime state
// (docs/03_Scheduler_and_Runtime_Design.md §2–4). It is the only
// component permitted to create WorkerExecutions and call
// adapter.Start (docs/03 §8 invariant #1).
//
// The dispatch flow (docs/03 §4):
//  1. Filter tasks in "ready" state.
//  2. For each, check dependencies are satisfied (docs/02 §4 #1).
//  3. Select a Worker (rule-based: runtime/model compatibility, health,
//     concurrency — docs/03 §4.1).
//  4. Select an Adapter (matching kind, healthy heartbeat, free capacity
//     — docs/03 §4.2).
//  5. Create a WorkerExecution (status=dispatching).
//  6. Call the adapter bridge to start the execution.
//  7. Transition the task to "assigned" and requeue.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/jackc/pgx/v5"
)

// heartbeatTTL is how long an adapter heartbeat remains valid for
// selection (docs/03 §5: heartbeat age > 60s → unhealthy).
const heartbeatTTL = 60 * time.Second

// TaskReconciler implements the reconciler.Reconciler interface for the
// "task" kind. It polls the work_items table for ready tasks and
// dispatches them via the AdapterBridge.
type TaskReconciler struct {
	pool             *db.Pool
	log              *slog.Logger
	bridge           AdapterBridge
	eventPub         eventbus.Publisher // direct NATS publisher for low-latency streaming (bypasses outbox relay)
	workflowNotifier func(ctx context.Context, runID string) // enqueues run for WorkflowReconciler on task completion
}

// NewTaskReconciler creates a TaskReconciler.
func NewTaskReconciler(pool *db.Pool, log *slog.Logger, bridge AdapterBridge) *TaskReconciler {
	return &TaskReconciler{pool: pool, log: log, bridge: bridge}
}

// SetRecoveryTrigger is deprecated. Recovery is triggered exclusively by
// explicit `recover` steps on the workflow canvas (docs/06 §1).
func (r *TaskReconciler) SetRecoveryTrigger(rt RecoveryTrigger) {}

// SetEventPublisher injects a direct NATS publisher for streaming
// execution events. When set, the reconciler publishes events directly
// to NATS after each callback commits, bypassing the outbox relay's
// 500ms poll interval. The outbox continues to be written for durability
// and catch-up on reconnect; the direct publish provides near-zero
// latency for the live frontend event stream.
func (r *TaskReconciler) SetEventPublisher(pub eventbus.Publisher) { r.eventPub = pub }

// SetWorkflowNotifier injects a callback that is called when a work
// item transitions to a terminal state (succeeded/failed). The callback
// should enqueue the workflow run ID so the WorkflowReconciler picks it
// up immediately instead of waiting for the next scan pass.
func (r *TaskReconciler) SetWorkflowNotifier(fn func(ctx context.Context, runID string)) {
	r.workflowNotifier = fn
}

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *TaskReconciler) Kind() string { return "task" }

// DispatchTask implements scheduler.TaskDispatcher. It dispatches a
// single ready task synchronously. The WorkflowReconciler calls this
// after its own transaction commits so the work item is visible to the
// TaskReconciler's internal dispatch transaction (docs/03 §8 invariant
// #1: only the TaskReconciler creates WorkerExecutions).
func (r *TaskReconciler) DispatchTask(ctx context.Context, taskID string) error {
	return r.reconcileOne(ctx, taskID)
}

// Reconcile processes a single task key. The key is the task (work item)
// ID. It is idempotent: re-running a pass for a task converges to the
// same state (docs/03 §1).
//
// The reconciler is driven by the manager's work queue, which enqueues
// ready tasks. When called with an empty key, it scans for ready tasks
// and dispatches them (docs/03 §4) — this is the scan pass the manager
// invokes when the queue is empty, which lets workflow task steps (and
// any other ready task) get dispatched without an explicit enqueue path.
func (r *TaskReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	if key == "" {
		// Scan pass: find ready tasks and dispatch each (docs/03 §4).
		// Limited to a batch per tick so one scan doesn't monopolize the
		// reconciler goroutine. v0.1: single dev tenant.
		tenantID := "tnt_dev"
		ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
		if err != nil {
			return reconciler.Result{Error: err}
		}
		ready, err := db.ListReadyTasks(ctx, ttx.Tx, tenantID)
		ttx.Rollback(ctx)
		if err != nil {
			return reconciler.Result{Error: fmt.Errorf("scan ready tasks: %w", err)}
		}
		for i, task := range ready {
			if i >= 16 {
				break
			}
			if err := r.reconcileOne(ctx, task.ID); err != nil {
				r.log.Warn("scan: dispatch ready task failed", "task", task.ID, "error", err)
			}
		}
		return reconciler.Result{}
	}
	if err := r.reconcileOne(ctx, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// reconcileOne dispatches a single ready task (docs/03 §4).
func (r *TaskReconciler) reconcileOne(ctx context.Context, taskID string) error {
	// We need the tenant to scope the transaction. The task carries it.
	// First, read the task without a tenant tx (RLS will block us), so
	// we resolve the tenant from the work item row via a query that
	// sets a temporary tenant context. In practice, the poll loop that
	// enqueues tasks knows the tenant; for v0.1 we scan all tenants
	// via the dev tenant. This is acceptable because v0.1 has a single
	// dev tenant; multi-tenant scheduling arrives with auth (Phase 9).
	tenantID := "tnt_dev"
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, taskID)
	if err != nil {
		if err == db.ErrNotFound {
			return nil // task deleted; nothing to reconcile
		}
		return fmt.Errorf("get task: %w", err)
	}

	// Only reconcile tasks in "ready" state (docs/03 §4: if status !=
	// ready, return).
	if task.Status != domain.WorkItemReady {
		return nil
	}

	// Check dependencies satisfied (docs/02 §4 #1, docs/03 §4).
	satisfied, err := db.CheckDependenciesSatisfied(ctx, ttx.Tx, tenantID, task.ID)
	if err != nil {
		return fmt.Errorf("check deps: %w", err)
	}
	if !satisfied {
		// Requeue: dependencies not yet terminal-success.
		return nil
	}

	// Select a Worker (docs/03 §4.1: rule-based).
	_, version, err := r.selectWorker(ctx, ttx.Tx, tenantID, task)
	if err != nil {
		// No suitable worker — requeue with backoff.
		r.log.Warn("no suitable worker for task", "task", task.ID, "error", err)
		return nil
	}

	// Select an Adapter (docs/03 §4.2).
	adapter, err := r.selectAdapter(ctx, ttx.Tx, tenantID, version.RuntimeRef)
	if err != nil {
		r.log.Warn("no suitable adapter for task", "task", task.ID, "worker", version.WorkerID, "error", err)
		return nil
	}

	// Create WorkerExecution (docs/03 §4: createWorkerExecution).
	// Check if the work item's results indicate this is a follow-up
	// execution (created by CreateFollowUpExecution).
	var isFollowUp bool
	if len(task.Results) > 0 {
		var taskResults map[string]any
		if err := json.Unmarshal(task.Results, &taskResults); err == nil {
			if v, ok := taskResults["_is_follow_up"].(string); ok && v == "true" {
				isFollowUp = true
			}
		}
	}
	now := time.Now().UTC()
	execRow := db.ExecutionRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		ProjectID:      task.ProjectID,
		TaskID:         task.ID,
		WorkerID:       version.WorkerID,
		WorkerVersion:  version.Version,
		AdapterID:      &adapter.ID,
		Status:         domain.ExecutionDispatching,
		HealthState:    domain.HealthHealthy,
		StartedAt:      &now,
		WorkflowRunID:  task.WorkflowRunID,
		WorkflowStepID: task.WorkflowStepID,
		IsFollowUp:     isFollowUp,
	}
	created, err := db.CreateExecution(ctx, ttx.Tx, execRow)
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}

	// Transition task to "assigned" (docs/03 §6: ready → assigned).
	_, err = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemAssigned),
	})
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}

	// Link the workflow step run to the new execution so the run-view
	// UI can show "click step → open execution". Without this link,
	// the run view falls back to "waiting for dispatch…" placeholders
	// even after the dispatch succeeded, and the step run rows aren't
	// clickable (no worker_execution_id to navigate to).
	//
	// Done inside the same transaction so a worker-step run never
	// points at an execution that doesn't exist.
	if task.WorkflowStepID != "" {
		if stepRun, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, task.WorkflowRunID, task.WorkflowStepID); err == nil {
			if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRun.ID, stepRun.Version, db.UpdateWorkflowStepRunFields{
				WorkerExecutionID: &created.ID,
				Status:           strPtr(domain.StepRunRunning),
				StartedAt:        &now,
			}); err != nil {
				return fmt.Errorf("link step run to execution: %w", err)
			}
		} else if !errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("get step run for link: %w", err)
		}
	}

	// Enqueue outbox events for the execution + task.
	if err := enqueueExecEvent(ctx, ttx.Tx, "execution.created", created, nil); err != nil {
		return fmt.Errorf("enqueue exec event: %w", err)
	}
	if err := enqueueWorkItemEvent(ctx, ttx.Tx, "work_item.assigned", task); err != nil {
		return fmt.Errorf("enqueue task event: %w", err)
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Start the execution via the adapter bridge. This happens AFTER
	// the tx commits so the execution row is durable; if the adapter
	// start fails, a later reconcile pass marks the execution
	// failed_to_start (docs/03 §8: adapter unreachable mid-dispatch).
	go r.startExecution(ctx, created, task, version, adapter)

	r.log.Info("task dispatched",
		"task", task.ID, "execution", created.ID,
		"worker", version.WorkerID, "worker_version", version.Version,
		"adapter", adapter.ID)
	return nil
}

// startExecution calls the adapter bridge to start the execution. It
// runs in a goroutine so the reconcile loop is not blocked by the
// adapter call (docs/03 §8: no SELECT FOR UPDATE held across external
// calls). The bridge updates the execution status as telemetry arrives.
func (r *TaskReconciler) startExecution(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, adapter db.AdapterRow) {
	// PR B (context propagation): if the WorkflowReconciler populated
	// the work item's prompt_context, the composite prompt is the
	// Goal — it carries the work item's title + description + AC +
	// ancestor chain + upstream step summaries. Otherwise fall back
	// to task.Title (the legacy direct-dispatch path).
	goal := task.Title
	if len(task.PromptContext) > 0 {
		var pc struct {
			Composite string `json:"composite"`
		}
		if err := json.Unmarshal(task.PromptContext, &pc); err == nil && pc.Composite != "" {
			goal = pc.Composite
		}
	}
	// Resolve the project directory so the adapter runs in the correct
	// working directory (avoids picking up Orchicon's own AGENTS.md etc.).
	var projectDir string
	var projDir string
	if err := r.pool.QueryRow(ctx,
		`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
		exec.ProjectID, "tnt_dev",
	).Scan(&projDir); err == nil {
		projectDir = projDir
	}
	manifest := ExecutionManifest{
		ExecutionID:        exec.ID,
		TaskID:             exec.TaskID,
		ProjectID:          exec.ProjectID,
		WorkerID:           version.WorkerID,
		WorkerVersion:      version.Version,
		SystemPrompt:       composeSystemPrompt(version),
		Goal:               goal,
		AcceptanceCriteria: task.AcceptanceCriteria,
		ModelRef:           version.ModelRef,
		ContextSources:     version.ContextSources,
		Budgets:            version.BudgetOverrides,
		Permissions:        version.Permissions,
		ProjectDir:         projectDir,
	}
	if err := r.bridge.Start(ctx, exec, manifest, r); err != nil {
		r.log.Error("adapter start failed", "execution", exec.ID, "error", err)
		// Mark the execution as failed_to_start.
		r.markFailedToStart(context.Background(), exec, err.Error())
	}
}

// markFailedToStart transitions an execution to failed_to_start
// (docs/03 §8: adapter unreachable mid-dispatch → failed_to_start, task
// requeues with backoff).
func (r *TaskReconciler) markFailedToStart(ctx context.Context, exec db.ExecutionRow, errorMessage string) {
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Error("begin tx for failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	now := time.Now().UTC()
	_, err = db.UpdateExecution(ctx, ttx.Tx, exec.TenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
		Status:       strPtr(domain.ExecutionFailedToStart),
		EndedAt:      &now,
		ErrorMessage: &errorMessage,
	})
	if err != nil {
		r.log.Error("mark failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	// Requeue the task: status back to ready.
	_, err = db.UpdateWorkItem(ctx, ttx.Tx, exec.TenantID, exec.TaskID, 0, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemReady),
	})
	if err != nil {
		r.log.Error("requeue task after failed_to_start", "task", exec.TaskID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit failed_to_start", "execution", exec.ID, "error", err)
	}
}

// selectWorker selects a published Worker for the task using rule-based
// ranking (docs/03 §4.1): filter by health, rank by lowest utilization
// + LRU, deterministic tiebreak by worker id.
func (r *TaskReconciler) selectWorker(ctx context.Context, tx pgx.Tx, tenantID string, task db.WorkItemRow) (db.WorkerRow, db.WorkerVersionRow, error) {
	// v0.1: the task's assigned_worker_ref pins the worker. If empty,
	// there's no worker to dispatch to (the user must assign one).
	if len(task.AssignedWorkerRef) == 0 {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("task has no assigned worker")
	}
	var ref struct {
		WorkerID string `json:"worker_id"`
		Version  int    `json:"version"`
	}
	if err := json.Unmarshal(task.AssignedWorkerRef, &ref); err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("parse assigned_worker_ref: %w", err)
	}
	if ref.WorkerID == "" {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("assigned_worker_ref has no worker_id")
	}
	worker, err := db.GetWorker(ctx, tx, tenantID, ref.WorkerID)
	if err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("get worker: %w", err)
	}
	// Worker must be published or deprecated (dispatchable — docs/05 §4).
	if worker.Status != domain.WorkerPublished && worker.Status != domain.WorkerDeprecated {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("worker %s is not dispatchable (status=%s)", ref.WorkerID, worker.Status)
	}
	// Resolve the version: specific or latest published.
	var version db.WorkerVersionRow
	if ref.Version > 0 {
		versions, err := db.ListWorkerVersions(ctx, tx, tenantID, ref.WorkerID)
		if err != nil {
			return db.WorkerRow{}, db.WorkerVersionRow{}, err
		}
		for _, v := range versions {
			if v.Version == ref.Version {
				version = v
				break
			}
		}
		if version.ID == "" {
			return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("worker version %d not found", ref.Version)
		}
	} else {
		version, err = db.GetLatestWorkerVersion(ctx, tx, tenantID, ref.WorkerID, true)
		if err != nil {
			return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("get latest published version: %w", err)
		}
	}
	return worker, version, nil
}

// selectAdapter selects a registered adapter of the matching kind with
// a recent heartbeat and free capacity (docs/03 §4.2).
func (r *TaskReconciler) selectAdapter(ctx context.Context, tx pgx.Tx, tenantID, kind string) (db.AdapterRow, error) {
	adapters, err := db.ListReadyAdaptersByKind(ctx, tx, tenantID, kind, heartbeatTTL)
	if err != nil {
		return db.AdapterRow{}, fmt.Errorf("list adapters: %w", err)
	}
	if len(adapters) == 0 {
		return db.AdapterRow{}, fmt.Errorf("no ready adapters of kind %q", kind)
	}
	// Filter by free capacity (docs/03 §4.2: prefer adapters with
	// recent healthy heartbeats + free capacity).
	var candidates []db.AdapterRow
	for _, a := range adapters {
		count, err := db.CountActiveExecutionsForAdapter(ctx, tx, tenantID, a.ID)
		if err != nil {
			continue
		}
		if count < a.MaxConcurrentExecutions {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		return db.AdapterRow{}, fmt.Errorf("all adapters of kind %q at capacity", kind)
	}
	// Deterministic: sort by id (docs/03 §4.1: deterministic tiebreak).
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ID < candidates[j].ID
	})
	return candidates[0], nil
}

// --- execution status callbacks (called by the adapter bridge) ---

// OnStarted is called by the adapter bridge when the adapter confirms
// execution has started (docs/03 §6: assigned → running).
func (r *TaskReconciler) OnStarted(ctx context.Context, execID string) {
	r.updateExecStatus(ctx, execID, domain.ExecutionRunning, domain.HealthHealthy, "")
}

// OnResult is called by the adapter bridge when the execution reaches a
// terminal state (docs/03 §6: running → succeeded|failed). It updates
// the execution status and transitions the linked WorkItem to
// succeeded/failed so downstream consumers (the WorkflowReconciler
// polling task steps) observe completion (docs/02 §2.4: tasks are
// reconciled as children of workflows).
//
// `output` is the worker's accumulated text from the adapter (PR B —
// context propagation). When the worker succeeded, the TaskReconciler
// extracts the ORCHICON WORKER SUMMARY block from `output`, persists
// it as the work item's _summary, and copies it onto the linked
// workflow step run so downstream stages can include it as upstream
// context. `output` may be empty for non-opencode adapters or when
// the worker errored before producing any text.
func (r *TaskReconciler) OnResult(ctx context.Context, execID string, succeeded bool, output string, errorMessage string) {
	status := domain.ExecutionSucceeded
	if !succeeded {
		status = domain.ExecutionFailed
	}
	r.updateExecStatus(ctx, execID, status, domain.HealthTerminating, output, errorMessage)
	r.transitionWorkItemOnResult(ctx, execID, succeeded, output)
}

// transitionWorkItemOnResult moves the WorkItem linked to the execution
// to succeeded/failed when the execution terminates. This closes the
// loop so the WorkflowReconciler's task-step polling observes the
// terminal state (docs/02 §2.4, docs/03 §6). On failure, the
// TaskReconciler triggers recovery (Phase 7, docs/06 §2) — recovery is
// opt-out, not opt-in (docs/06 §1).
//
// `output` is the worker's accumulated text (PR B). On success, the
// function extracts the ORCHICON WORKER SUMMARY block, persists the
// full output + extracted summary onto the work item's results, and
// copies the summary onto the linked workflow step run so downstream
// stages can read it as upstream context.
func (r *TaskReconciler) transitionWorkItemOnResult(ctx context.Context, execID string, succeeded bool, output string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("transition work item: begin tx", "execution", execID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	exec, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("transition work item: get execution", "execution", execID, "error", err)
		return
	}
	// Fetch the work item to use its current version for optimistic
	// concurrency (docs/09 §5). Passing 0 would never match.
	wi, err := db.GetWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID)
	if err != nil {
		r.log.Error("transition work item: get work item", "task", exec.TaskID, "error", err)
		return
	}
	// PR B: extract summary from worker output. If the marker is
	// absent, the entire output is treated as the summary (the
	// worker's prompt instructs it to end with the marker; lenient
	// workers that don't follow the contract still get their full
	// output propagated downstream).
	var summary string
	if succeeded && output != "" {
		summary = extractWorkerSummary(output)
	}
	// Check if this work item is a follow-up with a parent execution
	// to write back to. Read _parent_execution_id from the raw results
	// before we overwrite them with the worker output.
	var parentExecID string
	if len(wi.Results) > 0 {
		var rawResults map[string]any
		if err := json.Unmarshal(wi.Results, &rawResults); err == nil {
			if pid, ok := rawResults["_parent_execution_id"].(string); ok {
				parentExecID = pid
			}
		}
	}

	// Persist output + summary on the work item's results JSON so the
	// audit trail shows what the worker produced. The summary is the
	// canonical downstream input.
	results := map[string]any{}
	if len(wi.Results) > 0 {
		_ = json.Unmarshal(wi.Results, &results)
	}
	if output != "" {
		results["_output"] = output
	}
	if summary != "" {
		results["_summary"] = summary
	}
	resultsJSON, _ := json.Marshal(results)
	if succeeded {
		fields := db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemSucceeded),
		}
		if resultsJSON != nil {
			fields.Results = &resultsJSON
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID, wi.Version, fields); err != nil {
			r.log.Error("transition work item: update", "task", exec.TaskID, "error", err)
			return
		}
		// PR B: copy the summary onto the linked workflow step run
		// (results._summary) so the WorkflowReconciler can compose
		// it into the next stage's prompt. Best-effort — a missing
		// step run (e.g. dispatched without a workflow) is logged
		// and skipped, not fatal.
		if summary != "" {
			if err := r.propagateSummaryToStepRun(ctx, ttx.Tx, "tnt_dev", exec.TaskID, summary); err != nil {
				r.log.Warn("propagate summary to step run", "task", exec.TaskID, "error", err)
			}
		}
	} else {
		// Failure: transition to failed so the step run transitions to
		// terminal-failed, allowing a downstream `recover` step to
		// activate and trigger recovery (docs/06 §1).
		fields := db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemFailed),
		}
		if resultsJSON != nil {
			fields.Results = &resultsJSON
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID, wi.Version, fields); err != nil {
			r.log.Error("transition work item: update", "task", exec.TaskID, "error", err)
			return
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("transition work item: commit", "execution", execID, "error", err)
		return
	}

	// Follow-up write-back: if this work item has a parent execution
	// (created via CreateFollowUpExecution), append the assistant's
	// output to the parent execution's conversation so the follow-up
	// feels like a continuation of the same conversation.
	if parentExecID != "" && succeeded && output != "" {
		r.appendToParentConversation(context.Background(), "tnt_dev", parentExecID, output)
	}

	// Notify the WorkflowReconciler that this task completed so it
	// can progress the step DAG immediately (docs/03 §2). Done after
	// commit so the work item status is visible. No-op when the work
	// item has no workflow link (direct dispatch, not from a workflow).
	if r.workflowNotifier != nil && wi.WorkflowRunID != "" {
		r.workflowNotifier(context.Background(), wi.WorkflowRunID)
	}

	// Recovery is NOT triggered automatically — explicit `recover`
	// steps on the workflow canvas handle this (docs/06 §1).
}

// OnHealth is called by the adapter bridge to update the execution's
// health_state (docs/03 §5: HealthMonitor recomputes from signals).
func (r *TaskReconciler) OnHealth(ctx context.Context, execID, healthState string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		return
	}
	_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
		HealthState: &healthState,
	})
	_ = ttx.Commit(ctx)
}

// OnStall is called by the adapter bridge's progress monitor when a stall
// signal trips (docs/06 §2: "stalled health state | no progress within
// stall window"; docs/03 §5). The reason carries which signal fired:
// stalled:no_progress | stalled:no_file_progress | stalled:repetition:<sig>.
//
// It updates the execution's health_state to stalled and triggers recovery
// (opt-out, not opt-in — docs/06 §1; idempotent — §9: an active recovery
// for the task short-circuits a duplicate trigger). This closes the
// "worker stuck looping" gap: a worker that repeats the same tool calls,
// makes no file changes, or makes no token progress is recovered rather
// than running forever.
func (r *TaskReconciler) OnStall(ctx context.Context, execID, reason string) {
	r.log.Warn("execution stalled — triggering recovery",
		"execution", execID, "reason", reason)
	// Update health_state to stalled and persist the stall reason as
	// error_message so the UI surfaces why recovery was triggered.
	r.OnHealth(ctx, execID, domain.HealthStalled)
	ttx, txErr := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if txErr != nil {
		r.log.Error("on stall: begin tx for error_message", "execution", execID, "error", txErr)
	} else {
		current, getErr := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
		if getErr == nil {
			_, _ = db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
				ErrorMessage: &reason,
			})
		}
		_ = ttx.Commit(ctx)
	}
	// Terminate the execution and fail the work item so the downstream
	// recover step (if any) activates on the next reconcile pass.
	r.updateExecStatus(ctx, execID, domain.ExecutionUnhealthy, domain.HealthUnhealthy, "", reason)
	r.transitionWorkItemOnResult(ctx, execID, false, reason)
}

// OnToolCall publishes a tool_call execution event so the frontend live
// session pane can show the tool invocation in real-time.
func (r *TaskReconciler) OnToolCall(ctx context.Context, execID, toolName string, input, output []byte) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on tool call: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.tool_call", current, map[string]any{
		"tool_name": toolName,
		"input":     string(input),
		"output":    string(output),
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.tool_call", current, map[string]any{
		"tool_name": toolName,
		"input":     string(input),
		"output":    string(output),
	})
}

// OnText publishes a text execution event so the frontend live session
// pane can show model output in real-time.
func (r *TaskReconciler) OnText(ctx context.Context, execID string, text string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on text: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.text", current, map[string]any{
		"text": text,
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.text", current, map[string]any{
		"text": text,
	})
}

// OnArtifact publishes an artifact execution event so the frontend live
// session pane can show model output files inline (docs/10 §11). Called
// by the adapter when the model uses the `write` tool (opencode built-in
// file writer). The name is the file path, artifactType is "markdown" /
// "json" / "text", and content is the full artifact body.
func (r *TaskReconciler) OnArtifact(ctx context.Context, execID, name, artifactType, content string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("on artifact: get execution", "execution", execID, "error", err)
		return
	}
	_ = enqueueExecEvent(ctx, ttx.Tx, "execution.artifact", current, map[string]any{
		"artifact_name": name,
		"artifact_type": artifactType,
		"content":       content,
	})
	_ = ttx.Commit(ctx)
	r.publishExecEvent(ctx, "execution.artifact", current, map[string]any{
		"artifact_name": name,
		"artifact_type": artifactType,
		"content":       content,
	})
}

func (r *TaskReconciler) updateExecStatus(ctx context.Context, execID, status, health string, output string, errorMessage ...string) {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("begin tx for status update", "execution", execID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	if err != nil {
		r.log.Error("get execution for status update", "execution", execID, "error", err)
		return
	}
	var endedAt *time.Time
	if status == domain.ExecutionSucceeded || status == domain.ExecutionFailed || status == domain.ExecutionTerminated || status == domain.ExecutionUnhealthy {
		now := time.Now().UTC()
		endedAt = &now
	}
	fields := db.UpdateExecutionFields{
		Status:      &status,
		HealthState: &health,
		EndedAt:     endedAt,
	}
	if len(errorMessage) > 0 && errorMessage[0] != "" {
		fields.ErrorMessage = &errorMessage[0]
	}
	if output != "" {
		fields.Output = &output
	}
	updated, err := db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, fields)
	if err != nil {
		r.log.Error("update execution status", "execution", execID, "error", err)
		return
	}
	// Enqueue event.
	eventType := "execution." + status
	_ = enqueueExecEvent(ctx, ttx.Tx, eventType, updated, nil)
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit status update", "execution", execID, "error", err)
		return
	}
	r.publishExecEvent(ctx, eventType, updated, nil)
}

// appendToParentConversation appends the assistant's output to the parent
// execution's conversation field so follow-up messages and responses appear
// as one continuous thread on the original execution detail page.
func (r *TaskReconciler) appendToParentConversation(ctx context.Context, tenantID, parentExecID, output string) {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Error("append to parent conversation: begin tx", "parent_execution", parentExecID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)

	parent, err := db.GetExecution(ctx, ttx.Tx, tenantID, parentExecID)
	if err != nil {
		r.log.Error("append to parent conversation: get parent", "parent_execution", parentExecID, "error", err)
		return
	}
	conv := parent.Conversation
	if len(conv) == 0 {
		conv = []byte("[]")
	}
	var entries []map[string]any
	if err := json.Unmarshal(conv, &entries); err != nil {
		entries = []map[string]any{}
	}
	truncated := output
	if len(truncated) > 32000 {
		truncated = truncated[:32000]
	}
	entries = append(entries, map[string]any{
		"role":       "assistant",
		"content":    truncated,
		"type":       "follow_up_response",
		"created_at": time.Now().UTC().Format(time.RFC3339),
	})
	updatedConv, err := json.Marshal(entries)
	if err != nil {
		r.log.Error("append to parent conversation: marshal", "parent_execution", parentExecID, "error", err)
		return
	}
	if _, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, parentExecID, parent.Version, db.UpdateExecutionFields{
		Conversation: &updatedConv,
	}); err != nil {
		r.log.Error("append to parent conversation: update", "parent_execution", parentExecID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("append to parent conversation: commit", "parent_execution", parentExecID, "error", err)
		return
	}
	r.log.Info("follow-up response appended to parent conversation",
		"parent_execution", parentExecID, "output_len", len(truncated))
}

// --- helpers ---------------------------------------------------------------

// publishExecEvent builds the same event payload as enqueueExecEvent and
// publishes it directly to NATS via the reconciler's direct publisher.
// This bypasses the outbox relay's 500ms poll interval so the frontend
// event stream receives events in near-real-time (~1ms vs ~500ms).
// Must be called AFTER the outbox transaction commits so the event is
// committed to the DB before being published (the outbox serves as the
// durable fallback for catch-up on reconnect).
func (r *TaskReconciler) publishExecEvent(ctx context.Context, eventType string, e db.ExecutionRow, extra map[string]any) {
	if r.eventPub == nil {
		return
	}
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       e.TenantID,
		"execution_id":    e.ID,
		"task_id":         e.TaskID,
		"project_id":      e.ProjectID,
		"worker_id":       e.WorkerID,
		"worker_version":  e.WorkerVersion,
		"status":          e.Status,
		"health_state":    e.HealthState,
		"aggregate_type":  "execution",
		"aggregate_id":    e.ID,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		evt[k] = v
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		r.log.Error("publish exec event marshal", "execution", e.ID, "error", err)
		return
	}
	subject := eventbus.SubjectFor("execution", eventType)
	// Use the execution ID + event type as the dedup key so the outbox
	// relay's eventual publish with its own MsgID (the outbox row ULID)
	// is a distinct message — the frontend's seenIds dedup catches the
	// duplicate. This is intentional: the direct publish arrives fast,
	// the outbox relay provides the durable fallback.
	dedupID := fmt.Sprintf("direct:%s:%s", e.ID, eventType)
	if err := r.eventPub.Publish(ctx, subject, dedupID, payload); err != nil {
		r.log.Warn("publish exec event", "execution", e.ID, "subject", subject, "error", err)
	}
}

func enqueueExecEvent(ctx context.Context, tx pgx.Tx, eventType string, e db.ExecutionRow, extra map[string]any) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       e.TenantID,
		"execution_id":    e.ID,
		"task_id":         e.TaskID,
		"project_id":      e.ProjectID,
		"worker_id":       e.WorkerID,
		"worker_version":  e.WorkerVersion,
		"status":          e.Status,
		"health_state":    e.HealthState,
		"aggregate_type":  "execution",
		"aggregate_id":    e.ID,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	for k, v := range extra {
		evt[k] = v
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      e.TenantID,
		EventType:     eventType,
		AggregateType: "execution",
		AggregateID:   e.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func enqueueWorkItemEvent(ctx context.Context, tx pgx.Tx, eventType string, w db.WorkItemRow) error {
	evt := map[string]any{
		"event_type":   eventType,
		"tenant_id":    w.TenantID,
		"work_item_id": w.ID,
		"project_id":   w.ProjectID,
		"status":       w.Status,
		"kind":         w.Kind,
		"title":        w.Title,
		"occurred_at":  time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      w.TenantID,
		EventType:     eventType,
		AggregateType: "work_item",
		AggregateID:   w.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func strPtr(s string) *string { return &s }

// composeSystemPrompt assembles the full system prompt from the worker's
// four structured fields (role, skills, behavior, agents_md). Falls back
// to the legacy SystemPrompt field if the new fields are empty.
func composeSystemPrompt(v db.WorkerVersionRow) string {
	if v.Role == "" && v.Skills == "" && v.Behavior == "" && v.AgentsMD == "" {
		return v.SystemPrompt
	}
	var parts []string
	add := func(heading, content string) {
		c := strings.TrimSpace(content)
		if c == "" {
			return
		}
		parts = append(parts, "# "+heading+"\n\n"+c)
	}
	add("Role", v.Role)
	add("Skills", v.Skills)
	add("Behavior", v.Behavior)
	add("AGENTS.md", v.AgentsMD)
	return strings.Join(parts, "\n\n")
}

// summaryMarker is the literal line the worker's prompt instructs it to
// end with. Everything from the marker (inclusive) to the end of the
// worker's output becomes the summary that flows downstream as upstream
// context. If absent, the entire output is treated as the summary so
// lenient workers that don't follow the contract still propagate.
const summaryMarker = "ORCHICON WORKER SUMMARY:"

// extractWorkerSummary parses the ORCHICON WORKER SUMMARY block from
// the worker's text. It takes the LAST occurrence of the marker (in
// case the worker mentions the literal in earlier text) and returns
// everything from the marker to the end of the string, trimmed. If the
// marker is not present, the entire input is returned (best-effort —
// the worker's prompt instructs it to end with the marker, but
// fallbacks keep lenient workers from breaking the workflow).
func extractWorkerSummary(output string) string {
	idx := strings.LastIndex(output, summaryMarker)
	if idx < 0 {
		return strings.TrimSpace(output)
	}
	return strings.TrimSpace(output[idx+len(summaryMarker):])
}

// propagateSummaryToStepRun copies the worker's summary onto the
// workflow step run that is awaiting this task (PR B — context
// propagation). The step run's _work_item_id (set when the run was
// dispatched) points at the work item; we look up the step run by
// that id and append _summary to its results JSON.
//
// Best-effort: a missing step run (e.g. dispatched without a
// workflow) is logged at debug and skipped. An error is returned only
// for genuine database errors.
func (r *TaskReconciler) propagateSummaryToStepRun(ctx context.Context, tx pgx.Tx, tenantID, taskID, summary string) error {
	// Find the step run that references this task.
	const q = `SELECT id, result, version FROM workflow_step_runs
		WHERE tenant_id = $1 AND result::text LIKE $2
		ORDER BY created_at DESC LIMIT 1`
	// We can't pass JSONB -> text via bind, so use a LIKE on the
	// result's text projection. The _work_item_id is a unique key in
	// the result JSON for task steps dispatched by the workflow.
	// Postgres's JSONB text representation has a space after each
	// colon, so the pattern needs a wildcard between the colon and
	// the id: "_work_item_id":<space?>01K...; the leading + trailing
	// `%` cover anything before/after.
	rows, err := tx.Query(ctx, q, tenantID, `%_work_item_id":%`+taskID+`%`)
	if err != nil {
		return fmt.Errorf("find step run for task: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		r.log.Debug("no step run references task", "task", taskID)
		return nil // no step run — task wasn't dispatched by a workflow
	}
	var stepRunID, rawResult string
	var version int
	if err := rows.Scan(&stepRunID, &rawResult, &version); err != nil {
		return fmt.Errorf("scan step run: %w", err)
	}
	rows.Close()
	merged := map[string]any{}
	if rawResult != "" {
		_ = json.Unmarshal([]byte(rawResult), &merged)
	}
	merged["_summary"] = summary
	updated, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, stepRunID, version, db.UpdateWorkflowStepRunFields{
		Result: &updated,
	}); err != nil {
		return fmt.Errorf("update step run result: %w", err)
	}
	return nil
}

// composeSystemPrompt assembles the full system prompt from the worker's
// four structured fields (role, skills, behavior, agents_md). Falls back
// to the legacy SystemPrompt field if the new fields are empty.
func composeSystemPrompt(v db.WorkerVersionRow) string {
	if v.Role == "" && v.Skills == "" && v.Behavior == "" && v.AgentsMD == "" {
		return v.SystemPrompt
	}
	var parts []string
	add := func(heading, content string) {
		c := strings.TrimSpace(content)
		if c == "" {
			return
		}
		parts = append(parts, "# "+heading+"\n\n"+c)
	}
	add("Role", v.Role)
	add("Skills", v.Skills)
	add("Behavior", v.Behavior)
	add("AGENTS.md", v.AgentsMD)
	return strings.Join(parts, "\n\n")
}
