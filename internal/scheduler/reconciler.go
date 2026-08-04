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
	"os"
	"path/filepath"
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
// after its own transaction commits so the step run + prompt are visible
// to the TaskReconciler's internal dispatch transaction (docs/03 §8
// invariant #1: only the TaskReconciler creates WorkerExecutions).
//
// stepRunID scopes the dispatch to a workflow step run ("" for
// standalone dispatch). For a workflow step the work item is a shared
// input reference — the step's execution is keyed by its step run, the
// composite prompt is read from the step run, and the ticket is never
// mutated (no status gate, no assigned/ready flip).
func (r *TaskReconciler) DispatchTask(ctx context.Context, taskID, stepRunID string) error {
	return r.reconcileOne(ctx, taskID, stepRunID)
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
			if err := r.reconcileOne(ctx, task.ID, ""); err != nil {
				r.log.Warn("scan: dispatch ready task failed", "task", task.ID, "error", err)
			}
		}
		return reconciler.Result{}
	}
	if err := r.reconcileOne(ctx, key, ""); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// reconcileOne dispatches a single task. When stepRunID is set it is a
// workflow-step dispatch, scoped to that step run: the ticket is a shared
// input reference, so there is no "ready" gate and no status mutation on
// the ticket, and the worker comes from the step (stored on the step run)
// rather than the ticket's assigned_worker_ref. With an empty stepRunID it
// is a standalone dispatch (docs/03 §4).
func (r *TaskReconciler) reconcileOne(ctx context.Context, taskID, stepRunID string) error {
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

	// Workflow-step dispatch: keyed by the step run, not the ticket.
	// The step run carries the worker + composite prompt written by the
	// WorkflowReconciler; the ticket is never gated on "ready" or
	// mutated here (parallel steps bound to the same ticket each get
	// their own execution).
	var stepRun *db.WorkflowStepRunRow
	if stepRunID != "" {
		sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunID)
		if err != nil {
			if err == db.ErrNotFound {
				return nil // step run gone; nothing to dispatch
			}
			return fmt.Errorf("get step run: %w", err)
		}
		// Idempotency: already linked to an execution (a prior inline
		// dispatch won) — don't double-dispatch this step.
		if sr.WorkerExecutionID != "" {
			return nil
		}
		stepRun = &sr
	} else {
		// Only reconcile tasks in "ready" state (docs/03 §4: if status !=
		// ready, return).
		if task.Status != domain.WorkItemReady {
			return nil
		}
		// Skip items that belong to a WORKFLOW RUN: they are dispatched
		// exclusively by the WorkflowReconciler's inline (step-run)
		// dispatch, never by the standalone scan. This covers both the
		// shared ticket and per-step artifacts like the worker-backed
		// approval ticket (created "ready" with an assigned worker). Two
		// failure modes without it:
		//   (a) during the run — the scan dispatches the approval ticket
		//       in parallel with the inline dispatch → TWO executions for
		//       one step;
		//   (b) after the run reaches terminal — the orphaned "ready"
		//       approval ticket is no longer bound to an *active* run, so
		//       a boundToActiveRun guard wouldn't catch it, and the scan
		//       dispatches a ghost execution into the void.
		if task.WorkflowRunID != "" {
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
	}

	// Select a Worker (docs/03 §4.1: rule-based). For a workflow step the
	// worker is pinned by the STEP (stored on the step run by the
	// WorkflowReconciler), not the ticket.
	var version db.WorkerVersionRow
	if stepRun != nil {
		version, err = r.workerVersionForStepRun(ctx, ttx.Tx, tenantID, *stepRun)
		if err != nil {
			r.log.Warn("no suitable worker for step run", "task", task.ID, "step_run", stepRun.ID, "error", err)
			return nil
		}
	} else {
		_, version, err = r.selectWorker(ctx, ttx.Tx, tenantID, task)
		if err != nil {
			// No suitable worker — requeue with backoff.
			r.log.Warn("no suitable worker for task", "task", task.ID, "error", err)
			return nil
		}
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
	// Look up the workflow step run's iteration (loop number) so the
	// execution can display "Work Item Name - Worker Name - Loop #" in
	// the frontend. Defaults to 0 for direct-dispatch (no workflow).
	var iteration int
	workflowRunID := task.WorkflowRunID
	workflowStepID := task.WorkflowStepID
	if stepRun != nil {
		workflowRunID = stepRun.WorkflowRunID
		workflowStepID = stepRun.StepID
		iteration = stepRun.Iteration
	} else if workflowRunID != "" && workflowStepID != "" {
		if sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, workflowRunID, workflowStepID); err == nil {
			iteration = sr.Iteration
		}
	}
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
		WorkflowRunID:  workflowRunID,
		WorkflowStepID: workflowStepID,
		IsFollowUp:     isFollowUp,
		Iteration:      iteration,
	}
	created, err := db.CreateExecution(ctx, ttx.Tx, execRow)
	if err != nil {
		return fmt.Errorf("create execution: %w", err)
	}

	// Transition task to "assigned" (docs/03 §6: ready → assigned) —
	// standalone dispatch only. A workflow-bound ticket stays "running"
	// for the whole run (the workflow reconciler sets its terminal status
	// at run end).
	if stepRun == nil {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, task.ID, task.Version, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemAssigned),
		}); err != nil {
			return fmt.Errorf("update task status: %w", err)
		}
	}

	// Link the workflow step run to the new execution so the run-view
	// UI can show "click step → open execution". Without this link,
	// the run view falls back to "waiting for dispatch…" placeholders
	// even after the dispatch succeeded, and the step run rows aren't
	// clickable (no worker_execution_id to navigate to).
	//
	// Done inside the same transaction so a worker-step run never
	// points at an execution that doesn't exist.
	if stepRun != nil {
		if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRun.ID, stepRun.Version, db.UpdateWorkflowStepRunFields{
			WorkerExecutionID: &created.ID,
			Status:            strPtr(domain.StepRunRunning),
			StartedAt:         &now,
		}); err != nil {
			return fmt.Errorf("link step run to execution: %w", err)
		}
	} else if workflowStepID != "" {
		if stepRunRow, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, tenantID, workflowRunID, workflowStepID); err == nil {
			if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, stepRunRow.ID, stepRunRow.Version, db.UpdateWorkflowStepRunFields{
				WorkerExecutionID: &created.ID,
				Status:            strPtr(domain.StepRunRunning),
				StartedAt:         &now,
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
		"task", task.ID, "step_run", stepRunID, "execution", created.ID,
		"worker", version.WorkerID, "worker_version", version.Version,
		"adapter", adapter.ID)
	return nil
}

// workerVersionForStepRun resolves the worker version pinned by a
// workflow step. The WorkflowReconciler stores _worker_id +
// _worker_version on the step run's result when it dispatches the step;
// it falls back to the worker assigned on the ticket (worker-backed
// approval steps) when the step run carries none.
func (r *TaskReconciler) workerVersionForStepRun(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow) (db.WorkerVersionRow, error) {
	var meta struct {
		WorkerID     string `json:"_worker_id"`
		WorkerVer    int    `json:"_worker_version"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	if meta.WorkerID != "" {
		if meta.WorkerVer > 0 {
			if v, err := db.GetWorkerVersionByID(ctx, tx, tenantID, meta.WorkerID, fmt.Sprintf("v%d", meta.WorkerVer)); err == nil {
				return v, nil
			}
		}
		if v, err := db.GetLatestWorkerVersion(ctx, tx, tenantID, meta.WorkerID, true); err == nil {
			return v, nil
		}
		return db.WorkerVersionRow{}, fmt.Errorf("no dispatchable version for step worker %s", meta.WorkerID)
	}
	// No worker pinned on the step run (e.g. worker-backed approval):
	// fall back to the ticket's assigned worker.
	task, err := db.GetWorkItem(ctx, tx, tenantID, mustStepWorkItemID(sr))
	if err != nil {
		return db.WorkerVersionRow{}, fmt.Errorf("get task for worker resolution: %w", err)
	}
	_, version, err := r.selectWorker(ctx, tx, tenantID, task)
	if err != nil {
		return db.WorkerVersionRow{}, err
	}
	return version, nil
}

// mustStepWorkItemID reads _work_item_id from a step run's result. It
// must exist for any dispatched step; empty means the step run is not in
// a dispatchable state.
func mustStepWorkItemID(sr db.WorkflowStepRunRow) string {
	var meta struct {
		WorkItemID string `json:"_work_item_id"`
	}
	_ = json.Unmarshal(sr.Result, &meta)
	return meta.WorkItemID
}

// startExecution calls the adapter bridge to start the execution. It
// runs in a goroutine so the reconcile loop is not blocked by the
// adapter call (docs/03 §8: no SELECT FOR UPDATE held across external
// calls). The bridge updates the execution status as telemetry arrives.
func (r *TaskReconciler) startExecution(ctx context.Context, exec db.ExecutionRow, task db.WorkItemRow, version db.WorkerVersionRow, adapter db.AdapterRow) {
	// Resolve the project directory so the adapter runs in the correct
	// working directory (avoids picking up Orchicon's own AGENTS.md etc.).
	// Use a background context (the reconciler's ctx may expire before
	// this goroutine gets a chance to query).
	var projectDir string
	{
		var p db.ProjectRow
		qCtx := context.Background()
		if err := r.pool.QueryRow(qCtx,
			`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
			exec.ProjectID, exec.TenantID,
		).Scan(&p.ProjectDir); err == nil {
			projectDir = p.ProjectDir
		}
	}
	// The system prompt is the full context the model sees on every
	// turn. The WorkflowReconciler builds the composite per step and
	// stores it on the STEP RUN's result (_prompt) — the ticket is a
	// shared input reference and is no longer mutated with per-step
	// context. For worker-backed approval steps (and legacy direct
	// dispatch) the composite still lives on the work item's
	// prompt_context. The composite carries the worker identity, the
	// project directory + context files, the task, the ancestor chain,
	// the recovery narrative, and the worker's contract (ORCHICON
	// WORKER SUMMARY marker). The opencode adapter delivers this as the
	// agent's `prompt` via OPENCODE_CONFIG_CONTENT so every
	// conversation turn carries the same context.
	composite, _ := extractComposite(task.PromptContext)
	if exec.WorkflowRunID != "" && exec.WorkflowStepID != "" {
		if stx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
			if sr, err := db.GetWorkflowStepRunByStep(context.Background(), stx.Tx, exec.TenantID, exec.WorkflowRunID, exec.WorkflowStepID); err == nil {
				var srMeta struct {
					Prompt string `json:"_prompt"`
				}
				_ = json.Unmarshal(sr.Result, &srMeta)
				if srMeta.Prompt != "" {
					composite = srMeta.Prompt
				}
			}
			_ = stx.Rollback(context.Background())
		}
	}
	systemPrompt := composite
	// Fall back to a minimal worker-prompt if no composite was set
	// (legacy direct-dispatch path: work item dispatched outside a
	// workflow, so the workflow reconciler never built a composite).
	if systemPrompt == "" {
		systemPrompt = composeSystemPrompt(version)
		if strings.TrimSpace(systemPrompt) == "" {
			systemPrompt = "You are a worker in the Orchicon orchestration system. " +
				"Complete the work item described in the user message and report back."
		}
	}
	// User message (Goal): just the work item title. The composite
	// (with the full task + project + recovery context) is the
	// system message, not the user message, so the worker
	// instruction to end with ORCHICON WORKER SUMMARY is consistent
	// across the first turn and every subsequent turn.
	// Fetch tenant settings for default model and stall thresholds.
	var defaultModelRef string
	var stallNoProgress, stallNoFileDiff, stallTextLoop int64
	var stallRepCount int32
	var stallRepWindow int64
	var reconnectAttempts int32
	var reconnectGrace int64
	{
		settingsCtx := context.Background()
		stx, err := r.pool.BeginTenantTx(settingsCtx, exec.TenantID)
		if err == nil {
			s, err := db.GetTenantSettings(settingsCtx, stx.Tx, exec.TenantID)
			if err == nil {
				defaultModelRef = s.DefaultWorkerModel
				stallNoProgress = s.StallNoProgressWindowSeconds
				stallNoFileDiff = s.StallNoFileDiffWindowSeconds
				stallTextLoop = s.StallTextLoopWindowSeconds
				stallRepCount = s.StallRepetitionCount
				stallRepWindow = s.StallRepetitionWindowSeconds
				reconnectAttempts = s.ExecutionReconnectAttempts
				reconnectGrace = s.ExecutionReconnectGraceSeconds
			}
			stx.Rollback(settingsCtx)
		}
	}

	// Resolve the workflow run's runtime container image so the adapter's
	// self-heal recreates the container with the identical image the
	// WorkflowReconciler used at run start.
	runtimeImage := ""
	if task.WorkflowRunID != "" {
		if rtx, err := r.pool.BeginTenantTx(context.Background(), exec.TenantID); err == nil {
			if run, gerr := db.GetWorkflowRun(context.Background(), rtx.Tx, exec.TenantID, task.WorkflowRunID); gerr == nil {
				runtimeImage = run.RuntimeImage
			}
			_ = rtx.Rollback(context.Background())
		}
	}

	manifest := ExecutionManifest{
		ExecutionID:                 exec.ID,
		TaskID:                      exec.TaskID,
		ProjectID:                   exec.ProjectID,
		WorkerID:                    version.WorkerID,
		WorkerVersion:               version.Version,
		SystemPrompt:                systemPrompt,
		Goal:                        task.Title,
		AcceptanceCriteria:          task.AcceptanceCriteria,
		ModelRef:                    version.ModelRef,
		DefaultModelRef:             defaultModelRef,
		ContextSources:              version.ContextSources,
		Budgets:                     version.BudgetOverrides,
		Permissions:                 version.Permissions,
		ProjectDir:                  projectDir,
		RuntimeWorkflowID:           task.WorkflowRunID,
		RuntimeImage:                runtimeImage,
		StallNoProgressWindowSeconds:  stallNoProgress,
		StallNoFileDiffWindowSeconds:  stallNoFileDiff,
		StallTextLoopWindowSeconds:    stallTextLoop,
		StallRepetitionCount:          stallRepCount,
		StallRepetitionWindowSeconds:  stallRepWindow,
		ReconnectAttempts:             reconnectAttempts,
		ReconnectGraceSeconds:         reconnectGrace,
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
	// Requeue the task: status back to ready (standalone only). A
	// workflow-bound ticket stays "running" for the whole run — its
	// status is set by the workflow reconciler at run end; the step run
	// is left to the workflow's poll/recovery path (dispatchLinkGrace →
	// recovery) instead of being requeued here.
	if exec.WorkflowRunID == "" {
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, exec.TenantID, exec.TaskID, 0, db.UpdateWorkItemFields{
			Status: strPtr(domain.WorkItemReady),
		}); err != nil {
			r.log.Error("requeue task after failed_to_start", "task", exec.TaskID, "error", err)
			return
		}
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

// FailLostExecution marks an execution failed with a clear reason and
// transitions its work item to failed, so the WorkflowReconciler's
// task-step poll observes the terminal state and the workflow's recover
// step re-dispatches in a fresh runtime. Used by the execution-liveness
// reaper (docs/03 §6) after a control-plane restart or a lost runtime
// container; it mirrors the failure path of a normal OnResult(false).
func (r *TaskReconciler) FailLostExecution(ctx context.Context, execID, errorMessage string) {
	r.updateExecStatus(ctx, execID, domain.ExecutionFailed, domain.HealthUnhealthy, "", errorMessage)
	r.transitionWorkItemOnResult(ctx, execID, false, "")
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
	//
	// We start fresh — previous results from an earlier execution
	// (e.g. SSE) carry stale fields that must NOT survive into this
	// execution. _parent_execution_id and _recovery_summary are
	// carried forward for traceability. _issues is NOT preserved —
	// the execution history in the prompt already shows prior issues.
	results := map[string]any{}
	if len(wi.Results) > 0 {
		var existing map[string]any
		if err := json.Unmarshal(wi.Results, &existing); err == nil {
			for _, k := range []string{"_parent_execution_id", "_recovery_summary"} {
				if v, ok := existing[k]; ok {
					results[k] = v
				}
			}
		}
	}
	if output != "" {
		results["_output"] = output
	}
	if summary != "" {
		results["_summary"] = summary
	}
	results["_worker"] = exec.WorkerID
	// Extract structured fields from worker output for the
	// loop_decision step: _decision (success/failure) and _issues.
	// The worker's AGENTS.md instructs it to emit these on their
	// own line at the end of the response. Without this extraction
	// the _decision signal stays buried in _output text and the
	// loop decision can't route to success/failure — it falls
	// through to re-ask every time.
	extractStructuredResult(output, results)

	// Fallback: if no explicit _decision marker was found, parse it
	// from the ORCHICON WORKER SUMMARY: <decision> — <text> format.
	if _, ok := results["_decision"]; !ok {
		if d := extractSummaryDecision(output); d != "" {
			results["_decision"] = d
		}
	}
	// Hard rule: if the worker explicitly wrote _issues: in its output,
	// the work is not accepted regardless of what _decision the model
	// chose. _issues is no longer auto-preserved from prior iterations,
	// so any _issues present must come from the current worker's output.
	if issues, ok := results["_issues"].(string); ok && strings.TrimSpace(issues) != "" {
		results["_decision"] = "failure"
	}
	// Extract list of modified files from diff markers in the output.
	if output != "" {
		if files := extractTouchedFiles(output); len(files) > 0 {
			results["_touched_files"] = files
		}
	}
	resultsJSON, _ := json.Marshal(results)
	// A work item bound to an ACTIVE workflow run tracks the RUN, not any
	// single step execution: the ticket is a shared input reference and
	// stays "running" for the whole run — its per-step results/status are
	// NOT written here. The workflow reconciler aggregates the run-level
	// narrative (step outputs + recovery episodes) and sets the terminal
	// status when the run completes/fails. Standalone (unbound) items keep
	// the old per-execution terminal transition + results write.
	runActive := r.boundToActiveRun(ctx, ttx.Tx, wi)
	if runActive {
		// Step outputs go on the step run (propagateStepRunResults) so
		// the run view, loop decisions, and composite prompts see them.
		r.propagateStepRunResults(ctx, ttx.Tx, exec.ID, results)
	} else if succeeded {
		status := domain.WorkItemSucceeded
		fields := db.UpdateWorkItemFields{
			Status: strPtr(status),
		}
		if resultsJSON != nil {
			fields.Results = &resultsJSON
		}
		if _, err := db.UpdateWorkItem(ctx, ttx.Tx, "tnt_dev", exec.TaskID, wi.Version, fields); err != nil {
			r.log.Error("transition work item: update", "task", exec.TaskID, "error", err)
			return
		}
		// Copy the execution results onto the linked workflow step run
		// so the run view can display decision/summary/issues/files
		// without opening each execution. Best-effort.
		r.propagateStepRunResults(ctx, ttx.Tx, exec.ID, results)
	} else {
		// Failure: transition to failed so the step run transitions to
		// terminal-failed, allowing a downstream `recover` step to
		// activate and trigger recovery (docs/06 §1).
		status := domain.WorkItemFailed
		fields := db.UpdateWorkItemFields{
			Status: strPtr(status),
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

	// Write .orchicon/ files for the next worker to read.
	r.writeOrchiconFiles(ctx, exec, wi, succeeded, results)

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

// boundToActiveRun reports whether the work item is linked to a workflow
// run that is still in flight (not yet terminal). A bound item's status
// tracks the run as a whole — individual step executions succeed/fail
// while the run continues, so they must not flip the item to a terminal
// state until the run itself ends.
func (r *TaskReconciler) boundToActiveRun(ctx context.Context, tx pgx.Tx, wi db.WorkItemRow) bool {
	if wi.WorkflowRunID == "" {
		return false
	}
	run, err := db.GetWorkflowRun(ctx, tx, wi.TenantID, wi.WorkflowRunID)
	if err != nil {
		return false
	}
	switch run.Status {
	case domain.WorkflowRunPending, domain.WorkflowRunRunning, domain.WorkflowRunPaused:
		return true
	}
	return false
}

// writeOrchiconFiles writes the execution results to .orchicon/ files
// in the project directory so the next worker can read previous step
// results from disk instead of receiving them inline in the prompt.
// Files are overwritten on every execution so disk always reflects
// the latest state. Best-effort — failures are logged but not fatal.
func (r *TaskReconciler) writeOrchiconFiles(ctx context.Context, exec db.ExecutionRow, wi db.WorkItemRow, succeeded bool, results map[string]any) {
	projectDir := r.lookupProjectDir(ctx, wi.ProjectID)
	if projectDir == "" {
		return
	}
	orchDir := filepath.Join(projectDir, ".orchicon", exec.WorkflowRunID)
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		r.log.Warn("write .orchicon files: mkdir", "dir", orchDir, "error", err)
		return
	}

	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(orchDir, name), []byte(content), 0644); err != nil {
			r.log.Warn("write .orchicon file", "file", name, "error", err)
		}
	}

	write("status", map[bool]string{true: "success", false: "failure"}[succeeded])
	write("worker", exec.WorkerID)

	if summary, ok := results["_summary"].(string); ok && summary != "" {
		write("summary", summary)
	}
	if issues, ok := results["_issues"].(string); ok && issues != "" {
		write("issues", issues)
	}
	if files, ok := results["_touched_files"].([]any); ok && len(files) > 0 {
		var sb strings.Builder
		for _, f := range files {
			if s, ok := f.(string); ok {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
		write("touched_files", strings.TrimSpace(sb.String()))
	}
}

// lookupProjectDir returns the project directory for a project id.
// Returns "" if the project is not found.
func (r *TaskReconciler) lookupProjectDir(ctx context.Context, projectID string) string {
	if projectID == "" {
		return ""
	}
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Warn("lookup project dir: begin tx", "error", err)
		return ""
	}
	defer ttx.Rollback(ctx)
	var dir string
	err = ttx.Tx.QueryRow(ctx,
		`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
		projectID, "tnt_dev",
	).Scan(&dir)
	if err != nil || dir == "" {
		return ""
	}
	ttx.Commit(ctx)
	return dir
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

// extractStructuredResult scans the worker's text output for structured
// fields that the loop_decision step reads from the work item's top-level
// results JSON. Recognised fields:
//
//	_decision: success  — the work was accepted (forward)
//	_decision: failure  — the work was rejected (loop back)
//	_issues: <text>      — details about what needs fixing
//
// Only fields at the start of a line (with optional code-fence markers)
// are extracted — inline references like "The team decided: failure" are
// ignored to avoid false matches. Each extracted field is written into
// `results` under its key so the loop_decision's code path at
// workflow_reconciler.go:838 finds it via wiResult[cfg.DecisionField].
func extractStructuredResult(output string, results map[string]any) {
	if output == "" {
		return
	}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Strip markdown code-fence markers the worker might have
		// wrapped the signal in (e.g. `_decision: success`).
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)

		// Extract _decision: handles any delimiter (":" alone or
		// ":space") and concatenated tokens like
		// "_decision:failuresomething" by prefix-matching the first
		// word for "success" or "failure".
		if idx := strings.Index(trimmed, "_decision:"); idx >= 0 {
			after := strings.TrimSpace(trimmed[idx+len("_decision:"):])
			parts := strings.Fields(after)
			if len(parts) > 0 {
				first := parts[0]
				var decision string
				switch {
				case strings.HasPrefix(first, "success"):
					decision = "success"
				case strings.HasPrefix(first, "failure"):
					decision = "failure"
				}
				if decision != "" {
					results["_decision"] = decision
					// Extract _issues whether it follows as a
					// separate token or is concatenated with the
					// decision token (e.g. "failure_issues:...").
					rest := strings.TrimSpace(strings.TrimPrefix(after, first))
					if i := strings.Index(rest, "_issues:"); i >= 0 {
						issues := strings.TrimSpace(rest[i+len("_issues:"):])
						if issues != "" {
							results["_issues"] = issues
						}
					} else if i := strings.Index(first, "_issues:"); i >= 0 {
						// Concatenated: "failure_issues:..."
						issues := strings.TrimSpace(first[i+len("_issues:"):])
						if issues != "" {
							results["_issues"] = issues
						}
					}
				}
			}
		}

		// Extract _issues: when it's on its own line (not already
		// captured from the _decision line above).
		if i := strings.Index(trimmed, "_issues:"); i >= 0 {
			if _, ok := results["_issues"]; !ok {
				issues := strings.TrimSpace(trimmed[i+len("_issues:"):])
				if issues != "" {
					results["_issues"] = issues
				}
			}
		}
	}
}

// extractComposite pulls the "composite" string out of a work item's
// prompt_context JSON (set by the WorkflowReconciler's buildCompositePrompt).
// Returns "" if the field is absent or unparseable.
func extractComposite(pc []byte) (string, error) {
	if len(pc) == 0 {
		return "", nil
	}
	var parsed struct {
		Composite string `json:"composite"`
	}
	if err := json.Unmarshal(pc, &parsed); err != nil {
		return "", err
	}
	return parsed.Composite, nil
}

// composeSystemPrompt assembles the worker-only system prompt from the
// four structured fields (role, skills, behavior, agents_md). Used as
// a fallback when the WorkflowReconciler did not build a composite
// (i.e. a work item dispatched outside a workflow, on the legacy
// direct path). The full system prompt the model sees on every turn
// is the composite — which itself contains composeSystemPrompt's
// output prepended under a "# Worker" section.
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
//
// The marker is followed by an optional decision prefix (first word,
// "success" or "failure") and the summary text:
//
//	ORCHICON WORKER SUMMARY: success — Implemented the feature.
//	ORCHICON WORKER SUMMARY: failure — Found 3 bugs.
//
// The `—` separator is optional; anything after the first word is the
// summary. If no decision prefix is present, the full text is treated
// as the summary (backward compatible).
const summaryMarker = "ORCHICON WORKER SUMMARY:"

// extractWorkerSummary parses the ORCHICON WORKER SUMMARY block from
// the worker's text. It takes the LAST occurrence of the marker and
// returns everything after it, trimmed, minus the decision prefix.
// If the marker is not present, the entire input is returned.
func extractWorkerSummary(output string) string {
	idx := strings.LastIndex(output, summaryMarker)
	if idx < 0 {
		return strings.TrimSpace(output)
	}
	rest := strings.TrimSpace(output[idx+len(summaryMarker):])
	return trimSummaryDecision(rest)
}

// extractSummaryDecision reads the first word of the summary block
// (the text after ORCHICON WORKER SUMMARY:) and returns "success",
// "failure", or "" if neither is found. The first word and any
// separator (—, :, whitespace) are consumed.
func extractSummaryDecision(output string) string {
	idx := strings.LastIndex(output, summaryMarker)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(output[idx+len(summaryMarker):])
	return firstWordAsDecision(rest)
}

// trimSummaryDecision removes the leading decision word (if present)
// and any separator from the summary text.
func trimSummaryDecision(s string) string {
	first := firstWordAsDecision(s)
	if first == "" {
		return s
	}
	// Remove the decision word and any separator that follows.
	rest := strings.TrimSpace(strings.TrimPrefix(s, first))
	rest = strings.TrimLeft(rest, "—:- ")
	return strings.TrimSpace(rest)
}

// firstWordAsDecision returns "success", "failure", or "" depending
// on the first whitespace-delimited word of s.
func firstWordAsDecision(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return ""
	}
	first := strings.ToLower(parts[0])
	switch {
	case strings.HasPrefix(first, "success"):
		return "success"
	case strings.HasPrefix(first, "failure"):
		return "failure"
	}
	return ""
}

// extractTouchedFiles parses `diff --git` lines from the worker's
// output text to determine which files were modified. Returns the
// target paths (the b/ side of the diff).
func extractTouchedFiles(output string) []string {
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "diff --git") {
			continue
		}
		// diff --git a/path b/path
		parts := strings.Fields(trimmed)
		if len(parts) < 4 {
			continue
		}
		p := strings.TrimPrefix(parts[3], "b/")
		if p != "" && !seen[p] {
			seen[p] = true
			files = append(files, p)
		}
	}
	return files
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
// propagateStepRunResults copies execution fields onto the workflow step
// run that dispatched THIS execution, so the run-view UI can show them
// without opening each execution. The step run is located by its
// worker_execution_id — NOT by searching for the work item id in the result
// JSON: many steps share the same bound work item, and all step runs are
// seeded with the same created_at, so a `_work_item_id` LIKE search can
// return an arbitrary matching step run (e.g. a previous step's or a
// different step sharing the item) and mislabel it with this execution's
// results. worker_execution_id is set on the step run in the same
// transaction that creates the execution, so it is always unambiguous.
func (r *TaskReconciler) propagateStepRunResults(ctx context.Context, tx pgx.Tx, execID string, results map[string]any) {
	const q = `SELECT id, result, version FROM workflow_step_runs
		WHERE tenant_id = $1 AND worker_execution_id = $2 LIMIT 1`
	rows, err := tx.Query(ctx, q, "tnt_dev", execID)
	if err != nil {
		r.log.Warn("propagate step run results: query", "execution", execID, "error", err)
		return
	}
	defer rows.Close()
	if !rows.Next() {
		return
	}
	var stepRunID, rawResult string
	var version int
	if err := rows.Scan(&stepRunID, &rawResult, &version); err != nil {
		r.log.Warn("propagate step run results: scan", "execution", execID, "error", err)
		return
	}
	rows.Close()
	merged := map[string]any{}
	if rawResult != "" {
		_ = json.Unmarshal([]byte(rawResult), &merged)
	}
	// Propagate execution fields onto the step run so the run-view
	// UI can show them without opening each execution.
	for _, k := range []string{"_summary", "_decision", "_issues", "_touched_files", "_worker"} {
		if v, ok := results[k]; ok {
			merged[k] = v
		}
	}
	updated, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, tx, "tnt_dev", stepRunID, version, db.UpdateWorkflowStepRunFields{
		Result: &updated,
	}); err != nil {
		r.log.Warn("propagate step run results: update", "execution", execID, "error", err)
	}
}