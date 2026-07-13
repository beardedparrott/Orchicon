// WorkflowReconciler — the control loop that progresses workflow runs
// through their step DAG (docs/03_Scheduler_and_Runtime_Design.md §2,
// docs/02_Domain_Model.md §2.4).
//
// A workflow run is the top-level reconcilable object for execution;
// tasks are reconciled as children (docs/02 §2.4). The
// WorkflowReconciler:
//  1. Scans pending/running workflow runs.
//  2. Transitions a pending run to running.
//  3. Progresses the step DAG: for each step whose depends_on are all
//     satisfied (succeeded), mark it ready; for each ready step,
//     evaluate its gate (gate_policy_ref) then dispatch by kind:
//       - task: create a WorkItem (kind=task) with the step's Worker ref
//         and hand it to the TaskReconciler for dispatch (only the
//         TaskReconciler creates WorkerExecutions — docs/03 §8
//         invariant #1). The step run polls the WorkItem to completion.
//       - decision: evaluate the branch (v0.1: default-true) and mark
//         succeeded; downstream branches that don't match are skipped.
//       - approval: block at approval_pending (human approval wiring
//         arrives with the Policy engine, Phase 7).
//       - parallel: mark succeeded; downstream fan-out steps become
//         ready as their deps complete.
//       - recover: invoke the recovery workflow (v0.1: mark succeeded;
//         full recovery arrives Phase 7).
//  4. When all steps are terminal-success, mark the run completed. If
//     any step failed with no recovery path, mark the run failed.
//
// Gate evaluation (docs/02 §2.5 Tier 1): the gate_policy_ref is
// evaluated before a ready step runs. The Rego Policy Engine arrives in
// Phase 7; for v0.1 the gate is a pass-through that logs the decision
// (allow) so the DAG progresses end-to-end for dev verification.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/workflow"
	"github.com/jackc/pgx/v5"
)

// WorkflowReconciler implements the reconciler.Reconciler interface for
// the "workflow" kind. It polls the workflow_runs table for pending/
// running runs and progresses their step DAGs.
type WorkflowReconciler struct {
	pool *db.Pool
	log  *slog.Logger
}

// NewWorkflowReconciler creates a WorkflowReconciler.
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger) *WorkflowReconciler {
	return &WorkflowReconciler{pool: pool, log: log}
}

// Kind returns the reconciler kind (docs/03 §2.1).
func (r *WorkflowReconciler) Kind() string { return "workflow" }

// Reconcile processes a single workflow run (key = run id), or scans
// all pending runs when key is empty. It is idempotent: re-running a
// pass for a run converges to the same state (docs/03 §1).
func (r *WorkflowReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// v0.1: single dev tenant. Multi-tenant scheduling arrives with
	// auth (Phase 9). Same assumption as TaskReconciler.reconcileOne.
	tenantID := "tnt_dev"
	if key == "" {
		// Scan pass: progress all pending/running runs.
		if err := r.scanRuns(ctx, tenantID); err != nil {
			return reconciler.Result{Error: err}
		}
		return reconciler.Result{}
	}
	if err := r.reconcileRun(ctx, tenantID, key); err != nil {
		return reconciler.Result{Error: err}
	}
	return reconciler.Result{}
}

// scanRuns lists pending/running runs and reconciles each.
func (r *WorkflowReconciler) scanRuns(ctx context.Context, tenantID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	runs, err := db.ListPendingWorkflowRuns(ctx, ttx.Tx, tenantID)
	ttx.Rollback(ctx)
	if err != nil {
		return fmt.Errorf("list pending runs: %w", err)
	}
	for _, run := range runs {
		if err := r.reconcileRun(ctx, tenantID, run.ID); err != nil {
			r.log.Warn("workflow run reconcile failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

// reconcileRun progresses a single workflow run through its step DAG.
func (r *WorkflowReconciler) reconcileRun(ctx context.Context, tenantID, runID string) error {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer ttx.Rollback(ctx)

	run, err := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		if err == db.ErrNotFound {
			return nil
		}
		return fmt.Errorf("get run: %w", err)
	}
	// Only progress non-terminal runs.
	if run.Status == domain.WorkflowRunCompleted || run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		return nil
	}

	// Transition pending → running (docs/02 §2.4).
	if run.Status == domain.WorkflowRunPending {
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status: strPtr(domain.WorkflowRunRunning),
		})
		if err != nil {
			return fmt.Errorf("transition run to running: %w", err)
		}
		run = updated
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunStarted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_started: %w", err)
		}
	}

	// Load the published version's steps to drive DAG progression.
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		return fmt.Errorf("get workflow version: %w", err)
	}
	steps, err := workflow.ParseSteps(version.Steps)
	if err != nil {
		return fmt.Errorf("parse steps: %w", err)
	}
	stepByID := make(map[string]workflow.StepWire, len(steps))
	for _, s := range steps {
		stepByID[s.ID] = s
	}

	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		return fmt.Errorf("list step runs: %w", err)
	}
	runByID := make(map[string]db.WorkflowStepRunRow, len(stepRuns))
	for _, sr := range stepRuns {
		runByID[sr.StepID] = sr
	}

	// Progress pending steps whose deps are satisfied → ready.
	progressed := false
	for _, sr := range stepRuns {
		if sr.Status != domain.StepRunPending {
			continue
		}
		step, ok := stepByID[sr.StepID]
		if !ok {
			continue
		}
		if r.depsSatisfied(step, runByID) {
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
				Status: strPtr(domain.StepRunReady),
			})
			if err != nil {
				return fmt.Errorf("mark step ready: %w", err)
			}
			runByID[sr.StepID] = updated
			progressed = true
			if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepReady, run, updated); err != nil {
				return fmt.Errorf("enqueue step_ready: %w", err)
			}
		}
	}

	// Dispatch ready steps by kind, evaluating gates first.
	for _, sr := range stepRuns {
		if sr.Status != domain.StepRunReady {
			// Re-read from the map to catch steps we just promoted.
			if r2, ok := runByID[sr.StepID]; ok && r2.Status == domain.StepRunReady {
				sr = r2
			} else {
				continue
			}
		}
		step, ok := stepByID[sr.StepID]
		if !ok {
			continue
		}
		// Gate evaluation (docs/02 §2.5 Tier 1). The Rego Policy Engine
		// arrives in Phase 7; for v0.1 the gate is a pass-through that
		// logs the decision (allow) so the DAG progresses end-to-end.
		allowed := r.evaluateGate(ctx, step, run)
		if !allowed {
			now := time.Now().UTC()
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunBlocked),
				EndedAt: &now,
			})
			if err != nil {
				return fmt.Errorf("mark step blocked: %w", err)
			}
			runByID[sr.StepID] = updated
			progressed = true
			if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepBlocked, run, updated); err != nil {
				return fmt.Errorf("enqueue step_blocked: %w", err)
			}
			continue
		}
		if err := r.dispatchStep(ctx, ttx.Tx, tenantID, run, step, sr, runByID); err != nil {
			return err
		}
		progressed = true
	}

	// Poll running task steps: check their linked WorkItem status.
	for i, sr := range stepRuns {
		if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindTask {
			continue
		}
		terminal, failed, err := r.pollTaskStep(ctx, ttx.Tx, tenantID, run, sr)
		if err != nil {
			return err
		}
		if terminal {
			endNow := time.Now().UTC()
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunSucceeded),
				EndedAt: &endNow,
			})
			if err != nil {
				return fmt.Errorf("mark task step succeeded: %w", err)
			}
			if failed {
				_, err = db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
					Status: strPtr(domain.StepRunFailed),
				})
				if err != nil {
					return fmt.Errorf("mark task step failed: %w", err)
				}
			}
			stepRuns[i] = updated
			runByID[sr.StepID] = updated
			progressed = true
			evt := domain.WorkflowEventStepSucceeded
			if failed {
				evt = domain.WorkflowEventStepFailed
			}
			if err := r.enqueueStepEvent(ctx, ttx.Tx, evt, run, updated); err != nil {
				return fmt.Errorf("enqueue step result: %w", err)
			}
		}
	}

	// Determine run terminal state: all steps succeeded → completed;
	// any failed → failed.
	allSucceeded := true
	anyFailed := false
	hasSteps := false
	for _, sr := range stepRuns {
		hasSteps = true
		// Re-read the latest status from the map (some rows may have
		// been promoted in this pass).
		if latest, ok := runByID[sr.StepID]; ok {
			sr = latest
		}
		switch sr.Status {
		case domain.StepRunSucceeded, domain.StepRunSkipped:
			// terminal-success
		case domain.StepRunFailed, domain.StepRunBlocked:
			anyFailed = true
		case domain.StepRunApprovalPending:
			allSucceeded = false
		default:
			allSucceeded = false
		}
	}
	if hasSteps && allSucceeded {
		now := time.Now().UTC()
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:  strPtr(domain.WorkflowRunCompleted),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark run completed: %w", err)
		}
		run = updated
		progressed = true
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunCompleted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_completed: %w", err)
		}
	} else if anyFailed {
		now := time.Now().UTC()
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:  strPtr(domain.WorkflowRunFailed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark run failed: %w", err)
		}
		run = updated
		progressed = true
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunFailed, run, ""); err != nil {
			return fmt.Errorf("enqueue run_failed: %w", err)
		}
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if progressed {
		r.log.Info("workflow run progressed", "run", runID, "status", run.Status)
	}
	return nil
}

// depsSatisfied returns true if all depends_on steps of `step` are in a
// terminal-success state (succeeded or skipped).
func (r *WorkflowReconciler) depsSatisfied(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	for _, dep := range step.DependsOn {
		sr, ok := runs[dep]
		if !ok {
			return false
		}
		if sr.Status != domain.StepRunSucceeded && sr.Status != domain.StepRunSkipped {
			return false
		}
	}
	return true
}

// evaluateGate evaluates the step's gate_policy_ref (docs/02 §2.5 Tier
// 1). The Rego Policy Engine arrives in Phase 7; for v0.1 the gate is a
// pass-through that logs the decision (allow) so the DAG progresses
// end-to-end for dev verification.
func (r *WorkflowReconciler) evaluateGate(ctx context.Context, step workflow.StepWire, run db.WorkflowRunRow) bool {
	if step.GatePolicyRef == "" {
		return true
	}
	r.log.Info("workflow gate evaluated (pass-through, Rego engine pending Phase 7)",
		"run", run.ID, "step", step.ID, "gate_policy_ref", step.GatePolicyRef, "decision", "allow")
	return true
}

// dispatchStep dispatches a ready step by kind (docs/02 §2.4).
func (r *WorkflowReconciler) dispatchStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow) error {
	now := time.Now().UTC()
	switch step.Kind {
	case domain.StepKindTask:
		// Create a WorkItem (kind=task) with the step's Worker ref and
		// hand it to the TaskReconciler for dispatch. Only the
		// TaskReconciler creates WorkerExecutions (docs/03 §8 invariant
		// #1). The step run polls the WorkItem to completion.
		workerRef, err := json.Marshal(map[string]any{
			"worker_id": step.Ref,
			"version":   step.WorkerVersion,
		})
		if err != nil {
			return fmt.Errorf("marshal worker ref: %w", err)
		}
		result, _ := json.Marshal(map[string]string{"_workflow_step_run_id": sr.ID, "_workflow_run_id": run.ID})
		workItem := db.WorkItemRow{
			ID:                db.NewID(),
			TenantID:          tenantID,
			ProjectID:         run.ProjectID,
			Kind:              domain.WorkItemKindTask,
			Title:             fmt.Sprintf("Workflow %s step %s", run.WorkflowID, step.Name),
			Status:            domain.WorkItemReady,
			AssignedWorkerRef: workerRef,
			Priority:          0,
			Budgets:           []byte("{}"),
			Results:           result,
		}
		// workflow_id links the WorkItem back to the workflow for
		// traceability (docs/02 §2.2). The reconciler polls via the
		// _workflow_step_run_id stored in results.
		wfID := run.WorkflowID
		workItem.WorkflowID = &wfID
		if _, err := db.CreateWorkItem(ctx, tx, workItem); err != nil {
			return fmt.Errorf("create task work item: %w", err)
		}
		// Record the work_item_id on the step run so we can poll it.
		stepResult, _ := json.Marshal(map[string]string{"_work_item_id": workItem.ID})
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status: strPtr(domain.StepRunRunning),
			Result: &stepResult,
			StartedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark task step running: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
			return fmt.Errorf("enqueue step_started: %w", err)
		}
		r.log.Info("workflow task step dispatched", "run", run.ID, "step", step.ID, "work_item", workItem.ID, "worker", step.Ref)

	case domain.StepKindDecision:
		// v0.1: default branch (true). Branch-condition evaluation from
		// step.config arrives with richer decision config.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark decision step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue decision step_succeeded: %w", err)
		}

	case domain.StepKindParallel:
		// Fan-out: mark succeeded; downstream steps that depend on this
		// one become ready on the next pass (their deps are now
		// satisfied).
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark parallel step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue parallel step_succeeded: %w", err)
		}

	case domain.StepKindRecover:
		// v0.1: recovery workflow invocation arrives with the Recovery
		// Engine (Phase 7). For now mark succeeded so the DAG progresses.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunSucceeded),
			StartedAt: &now,
			EndedAt:   &now,
		})
		if err != nil {
			return fmt.Errorf("mark recover step succeeded: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
			return fmt.Errorf("enqueue recover step_succeeded: %w", err)
		}

	case domain.StepKindApproval:
		// Block at approval_pending (docs/02 §2.4). Human approval
		// wiring (an ApproveStep RPC + Policy-derived decision) arrives
		// with the Policy engine, Phase 7. The run view shows the step
		// waiting.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunApprovalPending),
			StartedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark approval step pending: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepApproval, run, updated); err != nil {
			return fmt.Errorf("enqueue step_approval: %w", err)
		}

	default:
		// Unknown kind → fail the step rather than stall the run.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, 0, db.UpdateWorkflowStepRunFields{
			Status:  strPtr(domain.StepRunFailed),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark unknown step failed: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
			return fmt.Errorf("enqueue step_failed: %w", err)
		}
	}
	return nil
}

// pollTaskStep checks the WorkItem linked to a running task step. Returns
// (terminal, failed, error). The WorkItem id is stored in the step run's
// result JSON under _work_item_id.
func (r *WorkflowReconciler) pollTaskStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) (bool, bool, error) {
	var parsed struct {
		WorkItemID string `json:"_work_item_id"`
	}
	if err := json.Unmarshal(sr.Result, &parsed); err != nil || parsed.WorkItemID == "" {
		// No linked WorkItem yet (dispatch may not have committed in
		// this pass). Not terminal.
		return false, false, nil
	}
	wi, err := db.GetWorkItem(ctx, tx, tenantID, parsed.WorkItemID)
	if err != nil {
		if err == db.ErrNotFound {
			return false, false, nil
		}
		return false, false, fmt.Errorf("get work item: %w", err)
	}
	switch wi.Status {
	case domain.WorkItemSucceeded:
		return true, false, nil
	case domain.WorkItemFailed, domain.WorkItemCancelled:
		return true, true, nil
	default:
		// Still in flight.
		return false, false, nil
	}
}

// --- event helpers ---------------------------------------------------------

func (r *WorkflowReconciler) enqueueRunEvent(ctx context.Context, tx pgx.Tx, eventType string, run db.WorkflowRunRow, stepID string) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       run.TenantID,
		"workflow_id":     run.WorkflowID,
		"workflow_run_id": run.ID,
		"run_status":      run.Status,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if stepID != "" {
		evt["step_id"] = stepID
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal run event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      run.TenantID,
		EventType:     eventType,
		AggregateType: "workflow",
		AggregateID:   run.ID,
		AggregateVer:  run.Version,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

func (r *WorkflowReconciler) enqueueStepEvent(ctx context.Context, tx pgx.Tx, eventType string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) error {
	evt := map[string]any{
		"event_type":      eventType,
		"tenant_id":       sr.TenantID,
		"workflow_id":     run.WorkflowID,
		"workflow_run_id": sr.WorkflowRunID,
		"step_id":         sr.StepID,
		"step_run_id":     sr.ID,
		"step_status":     sr.Status,
		"occurred_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal step event: %w", err)
	}
	return db.EnqueueOutbox(ctx, tx, db.OutboxRow{
		TenantID:      sr.TenantID,
		EventType:     eventType,
		AggregateType: "workflow",
		AggregateID:   sr.ID,
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
}

// nowPtr returns a pointer to t (helper for optional timestamp fields).
func nowPtr(t time.Time) *time.Time { return &t }

// (nowPtr currently unused after refactor; retained for step-run
// timestamp fields as the reconciler grows.)
var _ = nowPtr
