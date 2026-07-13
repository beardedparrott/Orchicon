// Package recovery implements the Recovery Workflow Engine
// (docs/06_Recovery_Workflow_Engine.md, docs/02 §2.6).
//
// Recovery is a workflow, not a function. When a WorkerExecution becomes
// unhealthy, stalls, exhausts its context, or breaches a retry budget,
// the engine creates a RecoveryExecution and progresses it through the
// default 6-step workflow (capture → summarize → preserve → review →
// plan → resume) to preserve progress, minimize context loss, and resume
// forward motion (docs/06 §1).
//
// Recovery is opt-out, not opt-in (docs/06 §1). The TaskReconciler calls
// TriggerOnFailure when an execution fails; operators call
// RecoveryService.TriggerRecovery for manual triggers. The
// RecoveryReconciler (registered with the reconciler manager) scans
// pending recoveries and progresses them.
//
// Resumption path (docs/06 §4): the engine attempts direct checkpoint
// replay when a compatible adapter accepts the prior checkpoint;
// otherwise it runs the full summarize-resume path. The choice and
// outcome are recorded as telemetry.
//
// Bounded auto-relax (docs/06 §11): recovery may increase a Task's
// budget by up to 25% of the original automatically (with an audit
// event); beyond 150% of the original, human approval is required.
//
// Task completion (docs/06 §11, docs/02 §4 #2): a Task may be marked
// succeeded by the completion Policy, by the Reviewer Worker during
// recovery, or by a human (MarkTaskSucceeded). All three paths produce
// an audit event with the actor recorded.
//
// Recovery never bypasses the Scheduler (docs/06 §10 invariant #1): the
// resume step transitions the Task to ready and the TaskReconciler
// dispatches the replacement. Recovery never bypasses Policy (invariant
// #2): the resumed execution is subject to the same dispatch/budget/
// completion Policies. The original execution's history is immutable
// once recovering (invariant #4): recovery writes a new execution, it
// does not rewrite the old one.
package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/jackc/pgx/v5"
)

// Engine is the Recovery Workflow Engine. It triggers recoveries and
// progresses them through the default 6-step workflow. The
// RecoveryReconciler embeds it and is registered with the reconciler
// manager so pending recoveries are scanned each tick.
type Engine struct {
	pool *db.Pool
	log  *slog.Logger
}

// New constructs a Recovery Engine.
func New(pool *db.Pool, log *slog.Logger) *Engine {
	return &Engine{pool: pool, log: log}
}

// TriggerOnFailure is called by the TaskReconciler when a WorkerExecution
// fails (docs/06 §2). It creates a RecoveryExecution (idempotent — if an
// active recovery already exists for the task, it is a no-op) and seeds
// the default 6-step workflow. The affected Task transitions to
// recovering. Recovery is opt-out, not opt-in (docs/06 §1).
func (e *Engine) TriggerOnFailure(ctx context.Context, tenantID, taskID, failedExecID, triggerReason string) error {
	return e.trigger(ctx, tenantID, taskID, failedExecID, triggerReason, domain.RecoveryLevelL1)
}

// trigger creates the RecoveryExecution + step runs (idempotent) and
// transitions the task to recovering. Runs in its own transaction.
func (e *Engine) trigger(ctx context.Context, tenantID, taskID, failedExecID, triggerReason string, level int32) error {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("recovery trigger: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	// Idempotency: if an active recovery already exists for this task,
	// do nothing (docs/06 §9).
	if existing, err := db.GetActiveRecoveryForTask(ctx, ttx.Tx, tenantID, taskID); err == nil {
		e.log.Info("recovery already active for task", "task", taskID, "recovery", existing.ID)
		return nil
	}

	// Resolve the task + failed execution to scope the recovery.
	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, taskID)
	if err != nil {
		return fmt.Errorf("recovery trigger: get task: %w", err)
	}
	exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, failedExecID)
	if err != nil {
		return fmt.Errorf("recovery trigger: get execution: %w", err)
	}

	// Determine the resumption path (docs/06 §4): direct checkpoint
	// replay when a checkpoint exists + a compatible adapter is
	// available; otherwise summarize-resume.
	resumptionPath := domain.ResumptionPathSummarizeResume
	if exec.CheckpointRef != nil && *exec.CheckpointRef != "" {
		// A checkpoint exists — attempt direct replay. (Full adapter
		// compatibility check is a v0.2 refinement; v0.1 selects
		// checkpoint replay when a checkpoint is present, recording the
		// choice as telemetry.)
		resumptionPath = domain.ResumptionPathCheckpoint
	}

	// Derive the recovery budget (docs/06 §6): 25% of the task's token
	// budget, capped. The task's budgets are JSON; extract token limit.
	budgetTokensLimit, budgetCostLimit := deriveRecoveryBudget(task.Budgets)

	recoveryID := db.NewID()
	now := time.Now().UTC()
	row := db.RecoveryExecutionRow{
		ID:                 recoveryID,
		TenantID:           tenantID,
		ProjectID:          task.ProjectID,
		TaskID:             taskID,
		FailedExecutionID:  failedExecID,
		TriggerReason:      triggerReason,
		Level:              level,
		Status:             domain.RecoveryPending,
		ResumptionPath:     resumptionPath,
		BudgetTokensLimit:  budgetTokensLimit,
		BudgetCostLimitUSD: budgetCostLimit,
		TriggeredAt:        now,
	}
	created, err := db.CreateRecoveryExecution(ctx, ttx.Tx, row)
	if err != nil {
		return fmt.Errorf("recovery trigger: create: %w", err)
	}

	// Seed the default 6-step workflow (docs/06 §3). For checkpoint
	// replay, summarize + plan are skipped (docs/06 §4).
	for _, stepID := range domain.DefaultRecoverySteps {
		if resumptionPath == domain.ResumptionPathCheckpoint &&
			(stepID == domain.RecoveryStepSummarize || stepID == domain.RecoveryStepPlan) {
			// Direct replay skips summarize + plan.
			stepRun := db.RecoveryStepRunRow{
				ID:            db.NewID(),
				TenantID:      tenantID,
				RecoveryID:    recoveryID,
				StepID:        stepID,
				StepName:      stepName(stepID),
				Status:        domain.RecoveryStepSkipped,
				TriggerReason: triggerReason,
				AffectedRef:   failedExecID,
				Action:        "skipped (direct checkpoint replay)",
			}
			if _, err := db.CreateRecoveryStepRun(ctx, ttx.Tx, stepRun); err != nil {
				return fmt.Errorf("recovery trigger: seed skipped step: %w", err)
			}
			continue
		}
		stepRun := db.RecoveryStepRunRow{
			ID:            db.NewID(),
			TenantID:      tenantID,
			RecoveryID:    recoveryID,
			StepID:        stepID,
			StepName:      stepName(stepID),
			Status:        domain.RecoveryStepPending,
			TriggerReason: triggerReason,
			AffectedRef:   failedExecID,
		}
		if _, err := db.CreateRecoveryStepRun(ctx, ttx.Tx, stepRun); err != nil {
			return fmt.Errorf("recovery trigger: seed step: %w", err)
		}
	}

	// Transition the task to recovering (docs/02 §2.2).
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, taskID, task.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemRecovering),
	}); err != nil {
		return fmt.Errorf("recovery trigger: transition task: %w", err)
	}

	// Enqueue the recovery.triggered event (docs/08 §4.3).
	if err := enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventTriggered, created, "", "", triggerReason, "triggered recovery workflow", adapterRef(exec)); err != nil {
		return fmt.Errorf("recovery trigger: enqueue: %w", err)
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("recovery trigger: commit: %w", err)
	}
	e.log.Info("recovery triggered",
		"recovery", recoveryID, "task", taskID, "execution", failedExecID,
		"trigger", triggerReason, "level", level, "resumption", resumptionPath)
	return nil
}

// MarkTaskSucceeded marks a Task succeeded by the Reviewer Worker (during
// recovery) or a human (docs/06 §11, docs/02 §4 #2). Emits an audit
// event with the actor recorded. If an active recovery exists, it is
// marked resumed (the recovery's job is done — the task is complete).
func (e *Engine) MarkTaskSucceeded(ctx context.Context, tenantID, taskID, actorType, actorID, reason string) (string, error) {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("mark task succeeded: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, taskID)
	if err != nil {
		return "", fmt.Errorf("mark task succeeded: get task: %w", err)
	}
	if task.Status == domain.WorkItemSucceeded {
		return "", nil // already succeeded
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, taskID, task.Version, db.UpdateWorkItemFields{
		Status: strPtr(domain.WorkItemSucceeded),
	}); err != nil {
		return "", fmt.Errorf("mark task succeeded: update: %w", err)
	}

	// Audit event with the actor recorded (docs/02 §4 #2).
	auditID := db.NewID()
	auditEvt := map[string]any{
		"event_type":  "task.succeeded",
		"tenant_id":   tenantID,
		"task_id":     taskID,
		"actor_type":  actorType,
		"actor_id":    actorID,
		"reason":      reason,
		"occurred_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, _ := json.Marshal(auditEvt)
	if err := db.EnqueueOutbox(ctx, ttx.Tx, db.OutboxRow{
		ID:            auditID,
		TenantID:      tenantID,
		EventType:     "task.succeeded",
		AggregateType: "work_item",
		AggregateID:   taskID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("mark task succeeded: enqueue audit: %w", err)
	}

	// If an active recovery exists, mark it resumed (the recovery's job
	// is done — the task is complete). docs/06 §10 invariant #3.
	if rec, err := db.GetActiveRecoveryForTask(ctx, ttx.Tx, tenantID, taskID); err == nil {
		now := time.Now().UTC()
		updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
			Status:  strPtr(domain.RecoveryResumed),
			EndedAt: &now,
		})
		if err != nil {
			return "", fmt.Errorf("mark task succeeded: update recovery: %w", err)
		}
		_ = enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventResumed, updated, "", "", rec.TriggerReason, "task marked succeeded by "+actorType+"/"+actorID, "")
	}

	if err := ttx.Commit(ctx); err != nil {
		return "", fmt.Errorf("mark task succeeded: commit: %w", err)
	}
	e.log.Info("task marked succeeded", "task", taskID, "actor_type", actorType, "actor", actorID)
	return auditID, nil
}

// ApproveContinuationPlan approves a pending continuation plan (L3 human
// escalation, docs/06 §7, §8) and resumes the recovery.
func (e *Engine) ApproveContinuationPlan(ctx context.Context, tenantID, recoveryID, actor string) (db.ContinuationPlanRow, db.RecoveryExecutionRow, error) {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.ContinuationPlanRow{}, db.RecoveryExecutionRow{}, fmt.Errorf("approve plan: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	rec, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID)
	if err != nil {
		return db.ContinuationPlanRow{}, db.RecoveryExecutionRow{}, fmt.Errorf("approve plan: get recovery: %w", err)
	}
	if rec.ContinuationPlanID == "" {
		return db.ContinuationPlanRow{}, rec, fmt.Errorf("no continuation plan for recovery %s", recoveryID)
	}
	plan, err := db.UpdateContinuationPlanStatus(ctx, ttx.Tx, tenantID, rec.ContinuationPlanID, domain.PlanApproved, actor)
	if err != nil {
		return db.ContinuationPlanRow{}, rec, fmt.Errorf("approve plan: update: %w", err)
	}
	// Un-block the recovery so the reconciler resumes the step DAG.
	updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID, rec.Version, db.UpdateRecoveryExecutionFields{
		Status:             strPtr(domain.RecoveryRunning),
		NeedsHumanApproval: boolPtr(false),
	})
	if err != nil {
		return plan, rec, fmt.Errorf("approve plan: update recovery: %w", err)
	}
	_ = enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventPlanApproved, updated, "", "", rec.TriggerReason, "continuation plan approved by "+actor, "")
	if err := ttx.Commit(ctx); err != nil {
		return plan, rec, fmt.Errorf("approve plan: commit: %w", err)
	}
	return plan, updated, nil
}

// RejectContinuationPlan rejects a pending plan; the recovery fails /
// escalates (docs/06 §8).
func (e *Engine) RejectContinuationPlan(ctx context.Context, tenantID, recoveryID, actor, reason string) (db.ContinuationPlanRow, db.RecoveryExecutionRow, error) {
	ttx, err := e.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return db.ContinuationPlanRow{}, db.RecoveryExecutionRow{}, fmt.Errorf("reject plan: begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)
	rec, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID)
	if err != nil {
		return db.ContinuationPlanRow{}, db.RecoveryExecutionRow{}, fmt.Errorf("reject plan: get recovery: %w", err)
	}
	if rec.ContinuationPlanID == "" {
		return db.ContinuationPlanRow{}, rec, fmt.Errorf("no continuation plan for recovery %s", recoveryID)
	}
	plan, err := db.UpdateContinuationPlanStatus(ctx, ttx.Tx, tenantID, rec.ContinuationPlanID, domain.PlanRejected, actor)
	if err != nil {
		return db.ContinuationPlanRow{}, rec, fmt.Errorf("reject plan: update: %w", err)
	}
	now := time.Now().UTC()
	updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID, rec.Version, db.UpdateRecoveryExecutionFields{
		Status:  strPtr(domain.RecoveryFailed),
		EndedAt: &now,
	})
	if err != nil {
		return plan, rec, fmt.Errorf("reject plan: update recovery: %w", err)
	}
	_ = enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventPlanRejected, updated, "", "", rec.TriggerReason, "continuation plan rejected by "+actor+": "+reason, "")
	if err := ttx.Commit(ctx); err != nil {
		return plan, rec, fmt.Errorf("reject plan: commit: %w", err)
	}
	return plan, updated, nil
}

// --- RecoveryReconciler ----------------------------------------------------

// Reconciler implements reconciler.Reconciler for the "recovery" kind.
// It scans pending/running recoveries and progresses them through the
// 6-step default workflow (docs/06 §3, §9).
type Reconciler struct {
	*Engine
}

// NewReconciler creates a RecoveryReconciler.
func NewReconciler(e *Engine) *Reconciler { return &Reconciler{Engine: e} }

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *Reconciler) Kind() string { return "recovery" }

// Reconcile processes a single recovery (key = recovery id), or scans
// all pending recoveries when key is empty. Idempotent (docs/06 §9).
func (r *Reconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	tenantID := "tnt_dev" // v0.1: single dev tenant
	if key == "" {
		if err := r.scanRecoveries(ctx, tenantID); err != nil {
			return reconciler.Result{Error: err}
		}
		return reconciler.Result{}
	}
	if err := r.progressRecovery(ctx, tenantID, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

func (r *Reconciler) scanRecoveries(ctx context.Context, tenantID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	pending, err := db.ListPendingRecoveries(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("list pending recoveries: %w", err)
	}
	for _, rec := range pending {
		if err := r.progressRecovery(ctx, tenantID, rec.ID); err != nil {
			r.log.Warn("recovery reconcile failed", "recovery", rec.ID, "error", err)
		}
	}
	return nil
}

// progressRecovery advances a single recovery through its step DAG
// (docs/06 §3, §9). Idempotent: re-running resumes from the last
// completed step.
func (r *Reconciler) progressRecovery(ctx context.Context, tenantID, recoveryID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	rec, err := db.GetRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID)
	if err != nil {
		if err == db.ErrNotFound {
			return nil
		}
		return fmt.Errorf("get recovery: %w", err)
	}
	if rec.Status == domain.RecoveryResumed || rec.Status == domain.RecoveryFailed ||
		rec.Status == domain.RecoveryCancelled || rec.Status == domain.RecoveryEscalated {
		return nil
	}
	if rec.Status == domain.RecoveryBlocked {
		// Awaiting human approval (L3 — docs/06 §7). Do nothing until
		// ApproveContinuationPlan un-blocks.
		return nil
	}

	// Transition pending → running.
	if rec.Status == domain.RecoveryPending {
		updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID, rec.Version, db.UpdateRecoveryExecutionFields{
			Status: strPtr(domain.RecoveryRunning),
		})
		if err != nil {
			return fmt.Errorf("transition to running: %w", err)
		}
		rec = updated
	}

	// Load step runs + progress the DAG.
	stepRuns, err := db.ListRecoveryStepRuns(ctx, ttx.Tx, tenantID, recoveryID)
	if err != nil {
		return fmt.Errorf("list step runs: %w", err)
	}
	runByStep := make(map[string]db.RecoveryStepRunRow, len(stepRuns))
	for _, sr := range stepRuns {
		runByStep[sr.StepID] = sr
	}

	// Progress each non-skipped, non-terminal step in order. A step runs
	// when all prior non-skipped steps are succeeded (linear DAG —
	// docs/06 §3 default workflow is sequential).
	progressed := false
	for _, stepID := range domain.DefaultRecoverySteps {
		sr, ok := runByStep[stepID]
		if !ok {
			continue
		}
		if sr.Status == domain.RecoveryStepSkipped ||
			sr.Status == domain.RecoveryStepSucceeded ||
			sr.Status == domain.RecoveryStepFailed {
			continue
		}
		// Check all prior steps succeeded.
		if !priorStepsSucceeded(stepID, runByStep) {
			break
		}
		// Run the step.
		if err := r.runStep(ctx, ttx.Tx, tenantID, rec, &sr); err != nil {
			return err
		}
		runByStep[stepID] = sr
		progressed = true
	}

	// Determine terminal state: all non-skipped steps succeeded → resumed;
	// any failed → escalate / fail.
	allDone := true
	anyFailed := false
	for _, stepID := range domain.DefaultRecoverySteps {
		sr, ok := runByStep[stepID]
		if !ok {
			continue
		}
		switch sr.Status {
		case domain.RecoveryStepSucceeded, domain.RecoveryStepSkipped:
		case domain.RecoveryStepFailed:
			anyFailed = true
		default:
			allDone = false
		}
	}
	if allDone && !anyFailed {
		// Resume: transition the task back to ready so the
		// TaskReconciler dispatches the replacement (docs/06 §10
		// invariant #1: recovery never bypasses the Scheduler).
		now := time.Now().UTC()
		updated, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, tenantID, recoveryID, rec.Version, db.UpdateRecoveryExecutionFields{
			Status:  strPtr(domain.RecoveryResumed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark resumed: %w", err)
		}
		rec = updated
		// Transition the task recovering → ready (scheduler dispatches).
		task, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, rec.TaskID)
		if err == nil {
			_, _ = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, rec.TaskID, task.Version, db.UpdateWorkItemFields{
				Status: strPtr(domain.WorkItemReady),
			})
		}
		_ = enqueueRecoveryEvent(ctx, ttx.Tx, domain.RecoveryEventResumed, rec, "", "", rec.TriggerReason, "recovery completed; task resumed to ready", "")
		progressed = true
	} else if anyFailed {
		// Escalate (docs/06 §7). L1 → L2 → L3.
		if err := r.escalate(ctx, ttx.Tx, tenantID, rec); err != nil {
			return err
		}
		progressed = true
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if progressed {
		r.log.Info("recovery progressed", "recovery", recoveryID, "status", rec.Status)
	}
	return nil
}

// runStep executes a single recovery step (docs/06 §3). v0.1: the engine
// drives each step directly (the recovery workflow driver is the engine
// itself), recording the full narrative (why/what/how/where/when —
// docs/06 §11). Real Reviewer Worker dispatch for steps 2/4/5 is the
// v0.2 path; v0.1 validates the end-to-end recovery arc.
func (r *Reconciler) runStep(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow, sr *db.RecoveryStepRunRow) error {
	now := time.Now().UTC()
	// Mark running.
	updated, err := db.UpdateRecoveryStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateRecoveryStepRunFields{
		Status:    strPtr(domain.RecoveryStepRunning),
		StartedAt: &now,
		Action:    strPtr(stepAction(sr.StepID)),
	})
	if err != nil {
		return fmt.Errorf("mark step running: %w", err)
	}
	*sr = updated
	_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventStepStarted, rec, sr.StepID, sr.ID, rec.TriggerReason, stepAction(sr.StepID), "")

	// Execute the step logic.
	var stepResult []byte
	var stepErr error
	switch sr.StepID {
	case domain.RecoveryStepCapture:
		stepResult, stepErr = r.stepCapture(ctx, tx, tenantID, rec)
	case domain.RecoveryStepSummarize:
		stepResult, stepErr = r.stepSummarize(ctx, tx, tenantID, rec)
	case domain.RecoveryStepPreserve:
		stepResult, stepErr = r.stepPreserve(ctx, tx, tenantID, rec)
	case domain.RecoveryStepReview:
		stepResult, stepErr = r.stepReview(ctx, tx, tenantID, rec)
	case domain.RecoveryStepPlan:
		stepResult, stepErr = r.stepPlan(ctx, tx, tenantID, rec)
	case domain.RecoveryStepResume:
		stepResult, stepErr = r.stepResume(ctx, tx, tenantID, rec)
	}

	endNow := time.Now().UTC()
	if stepErr != nil {
		failed, err := db.UpdateRecoveryStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateRecoveryStepRunFields{
			Status:  strPtr(domain.RecoveryStepFailed),
			EndedAt: &endNow,
			Result:  &stepResult,
		})
		if err != nil {
			return fmt.Errorf("mark step failed: %w", err)
		}
		*sr = failed
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventStepFailed, rec, sr.StepID, sr.ID, rec.TriggerReason, "step failed: "+stepErr.Error(), "")
		return nil
	}
	succeeded, err := db.UpdateRecoveryStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateRecoveryStepRunFields{
		Status:  strPtr(domain.RecoveryStepSucceeded),
		EndedAt: &endNow,
		Result:  &stepResult,
	})
	if err != nil {
		return fmt.Errorf("mark step succeeded: %w", err)
	}
	*sr = succeeded
	_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventStepCompleted, rec, sr.StepID, sr.ID, rec.TriggerReason, stepAction(sr.StepID), "")
	return nil
}

// stepCapture snapshots the failed execution's state + recent telemetry
// and marks the original execution recovering (docs/06 §3 step 1).
func (r *Reconciler) stepCapture(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	exec, err := db.GetExecution(ctx, tx, tenantID, rec.FailedExecutionID)
	if err != nil {
		return nil, fmt.Errorf("get failed execution: %w", err)
	}
	snapshot := map[string]any{
		"execution_id":  exec.ID,
		"status":        exec.Status,
		"health_state":  exec.HealthState,
		"token_usage":   exec.TokenUsage,
		"cost_usd":      exec.CostUSD,
		"worker_id":     exec.WorkerID,
		"worker_version": exec.WorkerVersion,
		"started_at":    exec.StartedAt,
		"ended_at":      exec.EndedAt,
	}
	result, _ := json.Marshal(snapshot)
	r.log.Info("recovery capture", "recovery", rec.ID, "execution", exec.ID, "tokens", exec.TokenUsage, "cost", exec.CostUSD)
	return result, nil
}

// stepSummarize produces a concise summary of completed work (docs/06 §3
// step 2). v0.1: the engine produces a textual summary from the
// execution snapshot; v0.2 dispatches a Reviewer-class Worker.
func (r *Reconciler) stepSummarize(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	exec, err := db.GetExecution(ctx, tx, tenantID, rec.FailedExecutionID)
	if err != nil {
		return nil, fmt.Errorf("get execution: %w", err)
	}
	summary := fmt.Sprintf("Execution %s failed after %d tokens ($%.4f). Worker %s v%d. Resuming from captured state.",
		exec.ID, exec.TokenUsage, exec.CostUSD, exec.WorkerID, exec.WorkerVersion)
	// Persist the summary on the recovery.
	_, err = db.UpdateRecoveryExecution(ctx, tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
		Summary: &summary,
	})
	if err != nil {
		return nil, fmt.Errorf("update summary: %w", err)
	}
	result, _ := json.Marshal(map[string]string{"summary": summary})
	return result, nil
}

// stepPreserve writes artifacts, traces, file-diff refs, and the summary
// to durable storage (docs/06 §3 step 3). v0.1: the summary is already
// persisted on the recovery row; this step records that preservation
// occurred. v0.2 writes to the object store.
func (r *Reconciler) stepPreserve(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	result, _ := json.Marshal(map[string]string{"preserved": "summary+snapshot persisted on recovery row"})
	return result, nil
}

// stepReview validates completed work against acceptance criteria;
// produces a verdict (accept | reject | needs-human) (docs/06 §3 step 4).
// v0.1: default verdict is accept (the Reviewer Worker dispatch is the
// v0.2 path). A reject verdict would fail the step → escalate.
func (r *Reconciler) stepReview(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	verdict := "accept"
	result, _ := json.Marshal(map[string]string{"verdict": verdict})
	return result, nil
}

// stepPlan produces a continuation_plan describing remaining work and
// corrections (docs/06 §3 step 5, §8). The plan is persisted as a
// ContinuationPlan row and linked to the recovery. If the recovery
// needs human approval (budget > 150% — docs/06 §11), the plan status
// is pending and the recovery blocks at L3.
func (r *Reconciler) stepPlan(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	task, err := db.GetWorkItem(ctx, tx, tenantID, rec.TaskID)
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	// Bounded auto-relax (docs/06 §11): if the task's budget has been
	// exceeded, attempt to relax by up to 25% automatically. Beyond
	// 150% of the original, require human approval.
	relaxFraction := rec.BudgetRelaxFraction
	needsHuman := rec.NeedsHumanApproval
	if rec.BudgetTokensUsed > rec.BudgetTokensLimit && rec.BudgetTokensLimit > 0 {
		overage := float64(rec.BudgetTokensUsed) / float64(rec.BudgetTokensLimit)
		if relaxFraction < domain.BudgetRelaxAutoMaxFraction {
			relaxFraction = domain.BudgetRelaxAutoMaxFraction
		}
		if overage > domain.BudgetRelaxHumanThreshold {
			needsHuman = true
		}
	}

	planID := db.NewID()
	plan := db.ContinuationPlanRow{
		ID:             planID,
		TenantID:       tenantID,
		RecoveryID:     rec.ID,
		Version:        1,
		Completed:      []byte("[]"),
		InProgress:     []byte("[]"),
		Remaining:      []byte(fmt.Sprintf(`[{"task_id":"%s","title":"%s"}]`, task.ID, task.Title)),
		Corrections:    []byte("[]"),
		ContextSummary: rec.Summary,
		Assumptions:    []byte("[]"),
		Status:         domain.PlanPending,
	}
	if !needsHuman {
		plan.Status = domain.PlanApproved
	}
	createdPlan, err := db.CreateContinuationPlan(ctx, tx, plan)
	if err != nil {
		return nil, fmt.Errorf("create plan: %w", err)
	}
	_ = createdPlan

	// Link the plan to the recovery + record relax state.
	status := rec.Status
	if needsHuman {
		status = domain.RecoveryBlocked
	}
	_, err = db.UpdateRecoveryExecution(ctx, tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
		ContinuationPlanID:  &planID,
		BudgetRelaxFraction: &relaxFraction,
		NeedsHumanApproval:  &needsHuman,
		Status:              &status,
	})
	if err != nil {
		return nil, fmt.Errorf("link plan: %w", err)
	}

	result, _ := json.Marshal(map[string]any{
		"plan_id":            planID,
		"needs_human":        needsHuman,
		"budget_relax":       relaxFraction,
		"remaining_work":     task.Title,
	})
	if needsHuman {
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventBlocked, rec, domain.RecoveryStepPlan, "", rec.TriggerReason, "budget exceeds 150% — human approval required", "")
	} else {
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventPlanProduced, rec, domain.RecoveryStepPlan, "", rec.TriggerReason, "continuation plan produced", "")
	}
	return result, nil
}

// stepResume launches the replacement (docs/06 §3 step 6). The
// continuation plan is handed to the replacement via the task's
// run_context. Recovery never bypasses the Scheduler (docs/06 §10
// invariant #1): this step records the handoff; the actual dispatch
// happens when the task transitions to ready (in progressRecovery's
// allDone branch). v0.1: the resume step records the handoff.
func (r *Reconciler) stepResume(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) ([]byte, error) {
	result, _ := json.Marshal(map[string]string{
		"handoff":   "task transitioned to ready; TaskReconciler will dispatch the replacement",
		"plan_id":   rec.ContinuationPlanID,
		"summary":   rec.Summary,
	})
	return result, nil
}

// escalate handles L1 → L2 → L3 (docs/06 §7). L1 stalls or the Reviewer
// produced needs-human for the recovery → L2 (tighter budget,
// summary-only resume). L2 stalls or budget exhausted → L3 (human
// escalation: pause task, await approval).
func (r *Reconciler) escalate(ctx context.Context, tx pgx.Tx, tenantID string, rec db.RecoveryExecutionRow) error {
	now := time.Now().UTC()
	switch rec.Level {
	case domain.RecoveryLevelL1:
		// L1 → L2: escalate with tighter budget + summary-only resume.
		updated, err := db.UpdateRecoveryExecution(ctx, tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
			Level:  int32Ptr(domain.RecoveryLevelL2),
			Status: strPtr(domain.RecoveryEscalated),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("escalate L1→L2: %w", err)
		}
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventEscalated, updated, "", "", rec.TriggerReason, "escalated L1→L2 (recovery stalled)", "")
		// Trigger an L2 recovery for the same task.
		r.log.Info("recovery escalated L1→L2", "recovery", rec.ID, "task", rec.TaskID)
	case domain.RecoveryLevelL2:
		// L2 → L3: human escalation (pause task, await approval).
		updated, err := db.UpdateRecoveryExecution(ctx, tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
			Level:              int32Ptr(domain.RecoveryLevelL3),
			Status:             strPtr(domain.RecoveryBlocked),
			NeedsHumanApproval: boolPtr(true),
		})
		if err != nil {
			return fmt.Errorf("escalate L2→L3: %w", err)
		}
		// Pause the task (docs/06 §7: L3 always pauses the Task).
		task, err := db.GetWorkItem(ctx, tx, tenantID, rec.TaskID)
		if err == nil {
			_, _ = db.UpdateWorkItem(ctx, tx, tenantID, rec.TaskID, task.Version, db.UpdateWorkItemFields{
				Status: strPtr(domain.WorkItemRecovering),
			})
		}
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventEscalated, updated, "", "", rec.TriggerReason, "escalated L2→L3 (human approval required; task paused)", "")
		r.log.Warn("recovery escalated L2→L3 (human approval required)", "recovery", rec.ID, "task", rec.TaskID)
	default:
		// L3 already — cannot escalate further. Mark failed.
		updated, err := db.UpdateRecoveryExecution(ctx, tx, tenantID, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
			Status:  strPtr(domain.RecoveryFailed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("escalate L3 fail: %w", err)
		}
		_ = enqueueRecoveryEvent(ctx, tx, domain.RecoveryEventFailed, updated, "", "", rec.TriggerReason, "recovery failed at L3", "")
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func priorStepsSucceeded(stepID string, runs map[string]db.RecoveryStepRunRow) bool {
	for _, sid := range domain.DefaultRecoverySteps {
		if sid == stepID {
			break
		}
		sr, ok := runs[sid]
		if !ok {
			continue
		}
		if sr.Status != domain.RecoveryStepSucceeded && sr.Status != domain.RecoveryStepSkipped {
			return false
		}
	}
	return true
}

func stepName(stepID string) string {
	switch stepID {
	case domain.RecoveryStepCapture:
		return "Capture state"
	case domain.RecoveryStepSummarize:
		return "Summarize completed work"
	case domain.RecoveryStepPreserve:
		return "Preserve artifacts"
	case domain.RecoveryStepReview:
		return "Review against acceptance criteria"
	case domain.RecoveryStepPlan:
		return "Produce continuation plan"
	case domain.RecoveryStepResume:
		return "Resume execution"
	}
	return stepID
}

func stepAction(stepID string) string {
	switch stepID {
	case domain.RecoveryStepCapture:
		return "snapshot execution state + telemetry"
	case domain.RecoveryStepSummarize:
		return "produce concise summary of completed work"
	case domain.RecoveryStepPreserve:
		return "write artifacts + traces + summary to durable storage"
	case domain.RecoveryStepReview:
		return "validate completed work against acceptance criteria"
	case domain.RecoveryStepPlan:
		return "produce continuation plan + bounded auto-relax"
	case domain.RecoveryStepResume:
		return "hand off to scheduler for replacement dispatch"
	}
	return stepID
}

func adapterRef(exec db.ExecutionRow) string {
	if exec.AdapterID != nil {
		return *exec.AdapterID
	}
	return ""
}

// deriveRecoveryBudget computes the recovery budget from the task's
// budgets JSON (docs/06 §6: 25% of the Task's token/cost budget,
// capped). Returns (tokenLimit, costLimit).
func deriveRecoveryBudget(taskBudgets []byte) (int64, float64) {
	if len(taskBudgets) == 0 {
		return 0, 0
	}
	var b struct {
		TokenLimit int64   `json:"token_limit"`
		CostLimit  float64 `json:"cost_limit_usd"`
	}
	if err := json.Unmarshal(taskBudgets, &b); err != nil {
		return 0, 0
	}
	tokenLimit := int64(float64(b.TokenLimit) * domain.BudgetRelaxAutoMaxFraction)
	costLimit := b.CostLimit * domain.BudgetRelaxAutoMaxFraction
	return tokenLimit, costLimit
}

// enqueueRecoveryEvent enqueues a recovery.* outbox event with the full
// narrative (why/what/how/where/when — docs/06 §11).
func enqueueRecoveryEvent(ctx context.Context, tx pgx.Tx, eventType string, rec db.RecoveryExecutionRow, stepID, stepRunID, triggerReason, action, adapterRef string) error {
	evt := map[string]any{
		"event_type":          eventType,
		"tenant_id":           rec.TenantID,
		"project_id":          rec.ProjectID,
		"recovery_id":         rec.ID,
		"task_id":             rec.TaskID,
		"failed_execution_id": rec.FailedExecutionID,
		"recovery_status":     rec.Status,
		"trigger_reason":      triggerReason, // why
		"action":              action,        // how
		"adapter_ref":         adapterRef,    // where
		"level":               rec.Level,
		"occurred_at":         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if stepID != "" {
		evt["step_id"] = stepID
	}
	if stepRunID != "" {
		evt["step_run_id"] = stepRunID
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal recovery event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      rec.TenantID,
		EventType:     eventType,
		AggregateType: "recovery",
		AggregateID:   rec.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
