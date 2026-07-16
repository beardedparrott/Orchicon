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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
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
	pool     *db.Pool
	log      *slog.Logger
	bridge   AdapterBridge
	recovery RecoveryTrigger // Phase 7: trigger recovery on failure (docs/06 §2)
}

// NewTaskReconciler creates a TaskReconciler.
func NewTaskReconciler(pool *db.Pool, log *slog.Logger, bridge AdapterBridge) *TaskReconciler {
	return &TaskReconciler{pool: pool, log: log, bridge: bridge}
}

// SetRecoveryTrigger injects the recovery trigger (Phase 7). Called by
// the server after constructing both the TaskReconciler and the
// RecoveryEngine. When set, the TaskReconciler triggers a recovery
// when an execution fails (docs/06 §2). Recovery is opt-out, not opt-in
// (docs/06 §1).
func (r *TaskReconciler) SetRecoveryTrigger(rt RecoveryTrigger) { r.recovery = rt }

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *TaskReconciler) Kind() string { return "task" }

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
	manifest := ExecutionManifest{
		ExecutionID:        exec.ID,
		TaskID:             exec.TaskID,
		ProjectID:          exec.ProjectID,
		WorkerID:           version.WorkerID,
		WorkerVersion:      version.Version,
		SystemPrompt:       version.SystemPrompt,
		Goal:               goal,
		AcceptanceCriteria: task.AcceptanceCriteria,
		ModelRef:           version.ModelRef,
		ContextSources:     version.ContextSources,
		Budgets:            version.BudgetOverrides,
		Permissions:        version.Permissions,
	}
	if err := r.bridge.Start(ctx, exec, manifest, r); err != nil {
		r.log.Error("adapter start failed", "execution", exec.ID, "error", err)
		// Mark the execution as failed_to_start.
		r.markFailedToStart(context.Background(), exec)
	}
}

// markFailedToStart transitions an execution to failed_to_start
// (docs/03 §8: adapter unreachable mid-dispatch → failed_to_start, task
// requeues with backoff).
func (r *TaskReconciler) markFailedToStart(ctx context.Context, exec db.ExecutionRow) {
	ttx, err := r.pool.BeginTenantTx(ctx, exec.TenantID)
	if err != nil {
		r.log.Error("begin tx for failed_to_start", "execution", exec.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	now := time.Now().UTC()
	_, err = db.UpdateExecution(ctx, ttx.Tx, exec.TenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
		Status:  strPtr(domain.ExecutionFailedToStart),
		EndedAt: &now,
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
	r.updateExecStatus(ctx, execID, domain.ExecutionRunning, domain.HealthHealthy)
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
func (r *TaskReconciler) OnResult(ctx context.Context, execID string, succeeded bool, output string) {
	status := domain.ExecutionSucceeded
	if !succeeded {
		status = domain.ExecutionFailed
	}
	r.updateExecStatus(ctx, execID, status, domain.HealthTerminating)
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
		// Failure: transition to recovering and trigger the recovery
		// workflow (docs/06 §2). Recovery is opt-out, not opt-in
		// (docs/06 §1). The trigger is idempotent (docs/06 §9).
		fields := db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemRecovering),
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
	// Trigger recovery on failure (Phase 7). Done after commit so the
	// recovering state is durable; the recovery trigger is idempotent
	// (docs/06 §9). If no RecoveryTrigger is wired (nil), the task
	// stays in recovering — the operator can trigger manually.
	if !succeeded && r.recovery != nil {
		triggerReason := "execution_failed"
		if err := r.recovery.TriggerOnFailure(ctx, "tnt_dev", exec.TaskID, execID, triggerReason); err != nil {
			r.log.Error("trigger recovery on failure", "task", exec.TaskID, "execution", execID, "error", err)
		}
	}
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
	// Update health_state to stalled so the UI + timeline surface the
	// detected stall.
	r.OnHealth(ctx, execID, domain.HealthStalled)
	if r.recovery == nil {
		return
	}
	// Resolve the task + execution for the trigger (idempotent).
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("on stall: begin tx", "execution", execID, "error", err)
		return
	}
	exec, err := db.GetExecution(ctx, ttx.Tx, "tnt_dev", execID)
	ttx.Rollback(ctx)
	if err != nil {
		r.log.Error("on stall: get execution", "execution", execID, "error", err)
		return
	}
	if err := r.recovery.TriggerOnFailure(ctx, "tnt_dev", exec.TaskID, execID, reason); err != nil {
		r.log.Error("on stall: trigger recovery", "execution", execID, "task", exec.TaskID, "error", err)
	}
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
}

func (r *TaskReconciler) updateExecStatus(ctx context.Context, execID, status, health string) {
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
	updated, err := db.UpdateExecution(ctx, ttx.Tx, "tnt_dev", execID, current.Version, db.UpdateExecutionFields{
		Status:      &status,
		HealthState: &health,
		EndedAt:     endedAt,
	})
	if err != nil {
		r.log.Error("update execution status", "execution", execID, "error", err)
		return
	}
	// Enqueue event.
	eventType := "execution." + status
	_ = enqueueExecEvent(ctx, ttx.Tx, eventType, updated, nil)
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("commit status update", "execution", execID, "error", err)
	}
}

// --- helpers ---------------------------------------------------------------

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
