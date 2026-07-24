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
//         invariant #1). After the workflow transaction commits,
//         the reconciler calls DispatchTask inline so the execution
//         appears immediately (no wait for the TaskReconciler
//         heartbeat). The step run polls the WorkItem to completion.
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
	"os"
	"path/filepath"
	"strings"
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
	pool           *db.Pool
	log            *slog.Logger
	policy         PolicyEvaluator // Phase 7: Rego gate evaluation (docs/02 §2.5)
	taskDispatcher TaskDispatcher  // inline dispatch so executions appear immediately
	recovery       RecoveryTrigger // triggers recovery on explicit `recover` steps
}

// NewWorkflowReconciler creates a WorkflowReconciler. The policy
// evaluator evaluates gate_policy_ref before a ready step runs (Phase 7,
// docs/02 §2.5 Tier 1). May be nil (pass-through allow — v0.1 dev).
// The taskDispatcher is called after the workflow transaction commits
// to dispatch ready work items immediately (not waiting for the next
// TaskReconciler heartbeat). May be nil (fall back to heartbeat).
// recovery is the RecoveryEngine used by explicit `recover` steps.
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger, pe PolicyEvaluator, td TaskDispatcher, rt RecoveryTrigger) *WorkflowReconciler {
	return &WorkflowReconciler{pool: pool, log: log, policy: pe, taskDispatcher: td, recovery: rt}
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
	r.log.Debug("DEBUG: reconcileRun entered", "runID", runID, "tenantID", tenantID)
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
	r.log.Debug("DEBUG: run loaded", "runID", run.ID, "status", run.Status, "version", run.Version)
	r.log.Debug("DEBUG: run workflow", "workflowID", run.WorkflowID, "workflowVersion", run.WorkflowVersion)
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

	// stepRuns + runByID are built inside the outer-progress loop so
	// newly-created step runs (loop_decision iterations, loop-back
	// re-entry runs) are visible on subsequent passes.
	var stepRuns []db.WorkflowStepRunRow
	runByID := map[string]db.WorkflowStepRunRow{}
	r.log.Debug("DEBUG: runByID built", "keys", len(runByID))
	for k, v := range runByID {
		r.log.Debug("DEBUG: runByID entry", "key", k, "id", v.ID, "status", v.Status, "version", v.Version)
	}

	// Collect work items dispatched in this pass for inline TaskReconciler
	// dispatch after the transaction commits.
	var dispatchedWIDs []string

	// DAG progression loop: repeat pending→ready, dispatch, and poll
	// until no step makes progress in a full pass. This ensures that
	// when a task step is polled terminal, downstream pending steps
	// whose deps just became satisfied are progressed and dispatched
	// in the SAME scan pass — no need to wait for the next heartbeat
	// (docs/03 §2, docs/02 §2.4).
	progressed := false
	for {
		madeProgress := false

		// Reload step runs on every outer iteration so newly-created
		// step runs (e.g. loop_decision iterations, loop-back re-entry
		// runs) are visible to the terminal-state check and to the
		// next dispatch phase. Without this, the original stepRuns
		// snapshot is stale — new runs are orphaned and the run can
		// be marked COMPLETED while children are still PENDING.
		stepRuns, err = db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, runID)
		if err != nil {
			return fmt.Errorf("reload step runs: %w", err)
		}
		runByID = make(map[string]db.WorkflowStepRunRow, len(stepRuns))
		for _, sr := range stepRuns {
			runByID[sr.StepID] = sr
		}

		// Progress pending steps whose deps are satisfied → ready.
		// Use runByID (which reflects in-pass updates) for the
		// dependency check so steps progressed by Phase 2/3 in a
		// prior outer iteration are not re-processed. The
		// outer-progress loop iterates with the same stepRuns slice,
		// so once a step has been moved past PENDING in this pass
		// (or any prior pass) the original `sr.Version` is stale —
		// re-applying a "mark step ready" against it produces a
		// version-mismatch "db: not found" that wedges the whole run.
		// Loop re-entry: a step run with iteration > 0 is a fresh
		// re-entry run and must be processed even if runByID has a
		// non-pending entry for the same StepID from a prior iteration.
		// Superseded runs: a step run that has been superseded by a
		// later iteration (e.g. loop_decision re-ask created a fresh
		// run for the same step) is no longer the active run for
		// that step and must be skipped. The original PR Reviewer
		// run, for example, has SupersededBy set once a re-ask is
		// created; trying to re-update it as ready/failed produces
		// a version-mismatch "db: not found" error that wedges the
		// whole run.
		for _, sr := range stepRuns {
			if sr.SupersededBy != "" {
				r.log.Debug("DEBUG: skipping superseded step run", "stepID", sr.StepID, "id", sr.ID, "supersededBy", sr.SupersededBy)
				continue
			}
			// Skip if the active run for this StepID has already
			// been moved past PENDING in a prior pass (avoid
			// version-mismatch on re-update).
			active := sr
			if cur, ok := runByID[sr.StepID]; ok {
				active = cur
			}
			if active.Status != domain.StepRunPending {
				r.log.Debug("DEBUG: skipping step run (already past pending)", "stepID", sr.StepID, "id", sr.ID, "status", active.Status, "iteration", sr.Iteration)
				continue
			}
			r.log.Debug("DEBUG: checking step for ready", "stepID", sr.StepID, "id", sr.ID, "status", sr.Status, "version", sr.Version)
			step, ok := stepByID[sr.StepID]
			if !ok {
				r.log.Debug("DEBUG: step not found in stepByID", "stepID", sr.StepID)
				continue
			}
			r.log.Debug("DEBUG: checking deps satisfied", "stepID", sr.StepID)
			if r.depsSatisfied(step, runByID) {
				r.log.Debug("DEBUG: about to update step run",
					"stepID", sr.StepID,
					"runID", sr.ID,
					"version", sr.Version,
					"tenantID", tenantID,
				)
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status: strPtr(domain.StepRunReady),
				})
				if err != nil {
					return fmt.Errorf("mark step ready: %w", err)
				}
				runByID[sr.StepID] = updated
				madeProgress = true
				if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepReady, run, updated); err != nil {
					return fmt.Errorf("enqueue step_ready: %w", err)
				}
			}
		}

		// Dispatch ready or recovering steps by kind, evaluating gates first.
		// Recovering steps from summarize_restart are skipped here — the
		// recovery engine sets the work item back to "ready" asynchronously
		// and the TaskReconciler dispatches it on its own heartbeat.
		for _, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunReady && sr.Status != domain.StepRunRecovering {
				if r2, ok := runByID[sr.StepID]; ok && (r2.Status == domain.StepRunReady || r2.Status == domain.StepRunRecovering) {
					sr = r2
				} else {
					continue
				}
			}
			// For recovering steps, skip if the work item is still in
			// recovery (recovery engine hasn't completed yet).
			if sr.Status == domain.StepRunRecovering && sr.StepKind == domain.StepKindTask {
				var parsed struct {
					WorkItemID string `json:"_work_item_id"`
				}
				if err := json.Unmarshal(sr.Result, &parsed); err == nil && parsed.WorkItemID != "" {
					wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, parsed.WorkItemID)
					if err == nil && wi.Status == domain.WorkItemRecovering {
						// Recovery still in progress — wait for next pass.
						continue
					}
				}
			}
			step, ok := stepByID[sr.StepID]
			if !ok {
				continue
			}
			allowed := r.evaluateGate(ctx, step, run)
			if !allowed {
				now := time.Now().UTC()
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunBlocked),
					EndedAt: &now,
				})
				if err != nil {
					return fmt.Errorf("mark step blocked: %w", err)
				}
				runByID[sr.StepID] = updated
				madeProgress = true
				if err := r.enqueueStepEvent(ctx, ttx.Tx, domain.WorkflowEventStepBlocked, run, updated); err != nil {
					return fmt.Errorf("enqueue step_blocked: %w", err)
				}
				continue
			}
			var stepWIDs []string
			if err := r.dispatchStep(ctx, ttx.Tx, tenantID, run, step, sr, runByID, steps, &stepWIDs); err != nil {
				return err
			}
			madeProgress = true
			dispatchedWIDs = append(dispatchedWIDs, stepWIDs...)
		}

		// Poll running task steps: check their linked WorkItem status.
		for i, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindTask {
				continue
			}
			stepCfg := ""
			if s, ok := stepByID[sr.StepID]; ok {
				stepCfg = s.Config
			}
			terminal, failed, err := r.pollTaskStep(ctx, ttx.Tx, tenantID, run, sr, stepCfg, runByID)
			if err != nil {
				return err
			}
			if terminal {
				endNow := time.Now().UTC()
				finalStatus := domain.StepRunSucceeded
				if failed {
					finalStatus = domain.StepRunFailed
				}
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(finalStatus),
					EndedAt: &endNow,
				})
				if err != nil {
					return fmt.Errorf("mark task step terminal: %w", err)
				}
				stepRuns[i] = updated
				runByID[sr.StepID] = updated
				madeProgress = true
				evt := domain.WorkflowEventStepSucceeded
				if failed {
					evt = domain.WorkflowEventStepFailed
				}
				if err := r.enqueueStepEvent(ctx, ttx.Tx, evt, run, updated); err != nil {
					return fmt.Errorf("enqueue step result: %w", err)
				}
			} else if cur, ok := runByID[sr.StepID]; ok && cur.Status == domain.StepRunRecovering {
				// pollTaskStep initiated a retry — a new work_item was
				// created and the step was set to recovering. Signal
				// progress so the dispatch section re-dispatches on the
				// next inner loop iteration.
				stepRuns[i] = cur
				madeProgress = true
			}
		}

		// Poll running LOOP_DECISION steps: check if their re-entered
		// chain (SSE → … → upstream reviewer) has all completed.
		// When a loop decision re-enters, it marks itself RUNNING;
		// once the chain terminal-states, the loop decision goes back
		// to READY so it re-evaluates the new decision.
		for i, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindLoopDecision {
				continue
			}
			if ok, _ := r.pollLoopDecisionChain(ctx, ttx.Tx, tenantID, sr, steps); ok {
				endNow := time.Now().UTC()
				// re-check the decision; if it's still failure,
				// dispatchStep will re-enter again (bounded by
				// max_iterations).
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunReady),
					EndedAt: &endNow,
				})
				if err != nil {
					return fmt.Errorf("mark loop_decision step ready: %w", err)
				}
				stepRuns[i] = updated
				runByID[sr.StepID] = updated
				madeProgress = true
				r.log.Info("loop_decision chain complete, re-evaluating",
					"run", run.ID, "step", sr.StepID)
			}
		}

		if !madeProgress {
			break
		}
		progressed = true
	}

	// Determine run terminal state: all steps succeeded → completed;
	// any failed → failed. Also skip any remaining pending steps so
	// they don't incorrectly display as "pending" in a failed run.
	// Superseded step runs (e.g. a PR Reviewer run replaced by a
	// loop_decision re-ask) are ignored — the active run for that
	// step_id is whichever run is not superseded. This prevents a
	// stuck "running" state when a superseded SUCCEEDED run is left
	// in the list alongside a fresh PENDING re-ask run.
	allSucceeded := true
	anyFailed := false
	hasSteps := false
	var toSkip []string
	for _, sr := range stepRuns {
		if sr.SupersededBy != "" {
			continue
		}
		hasSteps = true
		if latest, ok := runByID[sr.StepID]; ok {
			sr = latest
		}
		switch sr.Status {
		case domain.StepRunSucceeded, domain.StepRunSkipped:
		case domain.StepRunFailed, domain.StepRunBlocked:
			anyFailed = true
		case domain.StepRunApprovalPending:
			allSucceeded = false
		case domain.StepRunRecovering:
			// Recovering is not terminal — the step is being retried.
			// Keep the run running (don't set anyFailed).
			allSucceeded = false
		default:
			allSucceeded = false
			if anyFailed {
				toSkip = append(toSkip, sr.StepID)
			}
		}
	}
	// If the run has failed, skip all remaining non-terminal steps so
	// the UI accurately reflects the run state instead of showing them
	// as "pending" forever.
	if anyFailed {
		for _, stepID := range toSkip {
			if cur, ok := runByID[stepID]; ok {
				now2 := time.Now().UTC()
				updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, cur.ID, cur.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunSkipped),
					EndedAt: &now2,
				})
				if err != nil {
					return fmt.Errorf("skip pending step on failed run: %w", err)
				}
				runByID[stepID] = updated
			}
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
		// Update the linked work item to failed.
		if run.WorkItemID != "" {
			if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); err == nil {
				status := domain.WorkItemFailed
				_, _ = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID, wi.Version, db.UpdateWorkItemFields{
					Status: &status,
				})
			}
		}
		// Terminate any running worker executions linked to this run.
		for _, sr := range stepRuns {
			if sr.WorkerExecutionID != "" {
				if exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, sr.WorkerExecutionID); err == nil {
					if exec.Status == domain.ExecutionRunning || exec.Status == domain.ExecutionDispatching {
						termStatus := domain.ExecutionTerminated
						termHealth := domain.HealthUnhealthy
						if _, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, exec.ID, exec.Version, db.UpdateExecutionFields{
							Status:       &termStatus,
							HealthState:  &termHealth,
							EndedAt:      &now,
							ErrorMessage: strPtr("workflow run failed"),
						}); err != nil {
							return fmt.Errorf("terminate execution on run failure: %w", err)
						}
					}
				}
			}
		}
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
	// Inline dispatch: hand dispatched work items to the TaskReconciler
	// immediately so executions appear in the UI without waiting for the
	// next TaskReconciler heartbeat (~1s). The dispatch happens after the
	// workflow transaction commits so the work item (status=ready) is
	// visible to the TaskReconciler's own transaction (docs/03 §8 invariant
	// #1: only the TaskReconciler creates WorkerExecutions).
	if r.taskDispatcher != nil {
		for _, wid := range dispatchedWIDs {
			if err := r.taskDispatcher.DispatchTask(context.Background(), wid); err != nil {
				r.log.Warn("inline dispatch failed", "work_item", wid, "error", err)
			}
		}
	}
	if progressed {
		r.log.Info("workflow run progressed", "run", runID, "status", run.Status)
	}
	return nil
}

// depsSatisfied returns true if all depends_on steps of `step` are in a
// terminal-success state (succeeded or skipped). Loop decision steps
// accept a failed upstream as satisfied so they can evaluate looping.
func (r *WorkflowReconciler) depsSatisfied(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	isLoopDecision := step.Kind == domain.StepKindLoopDecision
	for _, dep := range step.DependsOn {
		sr, ok := runs[dep]
		if !ok {
			r.log.Debug("DEBUG: depsSatisfied dep not in map", "step", step.ID, "dep", dep)
			return false
		}
		r.log.Debug("DEBUG: depsSatisfied check", "step", step.ID, "dep", dep, "depStatus", sr.Status)
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunSkipped {
			r.log.Debug("DEBUG: depsSatisfied dep satisfied", "step", step.ID, "dep", dep, "status", sr.Status)
			continue
		}
		if isLoopDecision && sr.Status == domain.StepRunFailed {
			r.log.Debug("DEBUG: depsSatisfied loop_decision dep satisfied (failed)", "step", step.ID, "dep", dep)
			continue
		}
		r.log.Debug("DEBUG: depsSatisfied not satisfied yet", "step", step.ID, "dep", dep, "status", sr.Status)
		return false
	}
	r.log.Debug("DEBUG: depsSatisfied all satisfied", "step", step.ID)
	return true
}

// evaluateGate evaluates the step's gate_policy_ref (docs/02 §2.5 Tier
// 1). Phase 7: the Rego Policy Engine evaluates the gate; if no policy
// is referenced or no PolicyEvaluator is wired, the gate is a pass-
// through (allow) so the DAG progresses (v0.1 dev fallback).
func (r *WorkflowReconciler) evaluateGate(ctx context.Context, step workflow.StepWire, run db.WorkflowRunRow) bool {
	if step.GatePolicyRef == "" {
		return true
	}
	if r.policy == nil {
		r.log.Info("workflow gate pass-through (no policy engine)",
			"run", run.ID, "step", step.ID, "gate_policy_ref", step.GatePolicyRef, "decision", "allow")
		return true
	}
	allowed, err := r.policy.EvaluateGate(ctx, run.TenantID, step.GatePolicyRef, "step_run", run.ID, map[string]any{
		"workflow_id": run.WorkflowID, "run_id": run.ID, "step_id": step.ID,
		"step_kind": step.Kind, "project_id": run.ProjectID,
	})
	if err != nil {
		r.log.Warn("workflow gate evaluation error (fail-open)",
			"run", run.ID, "step", step.ID, "error", err)
		return true
	}
	r.log.Info("workflow gate evaluated",
		"run", run.ID, "step", step.ID, "gate_policy_ref", step.GatePolicyRef, "allowed", allowed)
	return allowed
}

// dispatchStep dispatches a ready step by kind (docs/02 §2.4, docs/10 §5.1).
//
// The PR A model: the canvas holds three first-class node types —
// PROJECT (entry, sets the project context), WORK_ITEM (a passive
// marker that holds a work item's metadata as context for downstream
// workers), and TASK (a worker that processes the work item(s)
// connected to its input edge). Decision/Approval/Parallel/Recover
// remain for advanced control flow but are not required for the simple
// Work Item → Worker chain.
//
// TASK semantics under PR A:
//   - Find upstream WORK_ITEM steps in step.DependsOn.
//   - For each, load the referenced work item, set its
//     assigned_worker_ref to this step's worker, and dispatch it via
//     the existing TaskReconciler path (which keys on
//     assigned_worker_ref — docs/03 §8 invariant #1).
//   - The step run tracks the primary work item id under
//     _work_item_id in result JSON so pollTaskStep can poll.
func (r *WorkflowReconciler) dispatchStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, allSteps []workflow.StepWire, dispatchedWIDs *[]string) error {
	now := time.Now().UTC()
	switch step.Kind {
	case domain.StepKindProject:
		// Project marker step. The author dragged a project onto the
		// canvas and its id lives in config.project_id. On the first
		// dispatch we write it onto the workflow so downstream work
		// items land in the right project. Idempotent — repeated
		// dispatches (re-reconcile) just no-op.
		pid := readConfigProjectID(step.Config)
		if pid == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("project step %q has no config.project_id", step.Name))
		}
		if run.ProjectID != pid {
			updated, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, db.UpdateWorkflowRunFields{
				ProjectID: &pid,
			})
			if err != nil {
				return fmt.Errorf("set workflow project_id: %w", err)
			}
			run = updated
			r.log.Info("workflow project bound", "run", run.ID, "project", pid)
		}
		return r.succeedStep(ctx, tx, tenantID, run, sr, runs, now, "project bound")

	case domain.StepKindWorkItem:
		// Work item marker step. The author dragged a work item onto
		// the canvas and its id lives in config.work_item_id. The
		// marker is a passive anchor — we verify the work item exists
		// and is reachable, then succeed immediately so downstream
		// workers can pick it up.
		wid := readConfigWorkItemID(step.Config)
		if wid == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("work_item step %q has no config.work_item_id", step.Name))
		}
		if _, err := db.GetWorkItem(ctx, tx, tenantID, wid); err != nil {
			if err == db.ErrNotFound {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("work item %s not found", wid))
			}
			return fmt.Errorf("load work item: %w", err)
		}
		return r.succeedStep(ctx, tx, tenantID, run, sr, runs, now, "work_item marker")

	case domain.StepKindTask:
		// For recovering steps, the retry work item was already created
		// by pollTaskStep and its ID is stored in _work_item_id. Use it
		// directly instead of looking up upstream canvas markers.
		var upstream []string
		if sr.Status == domain.StepRunRecovering {
			var parsed struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if err := json.Unmarshal(sr.Result, &parsed); err == nil && parsed.WorkItemID != "" {
				upstream = []string{parsed.WorkItemID}
			}
		}
		if len(upstream) == 0 {
			// Normal path: look upstream for WORK_ITEM steps or bound item.
			upstream = upstreamWorkItemIDs(step, allSteps)
			if len(upstream) == 0 && run.WorkItemID != "" {
				upstream = []string{run.WorkItemID}
			}
		}
		if len(upstream) == 0 {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("worker step %q has no upstream work_item", step.Name))
		}
		if step.Ref == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("worker step %q has no worker ref", step.Name))
		}
		workerRef, err := json.Marshal(map[string]any{
			"worker_id": step.Ref,
			"version":   step.WorkerVersion,
		})
		if err != nil {
			return fmt.Errorf("marshal worker ref: %w", err)
		}
		wfID := run.WorkflowID
		var primaryWID string
		for _, wid := range upstream {
			wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
			if err != nil {
				if err == db.ErrNotFound {
					return r.failStep(ctx, tx, tenantID, run, sr, runs,
						fmt.Errorf("work item %s not found", wid))
				}
				return fmt.Errorf("load work item: %w", err)
			}
		// PR B (context propagation): build the composite prompt
		// the worker should see. The prompt is the work item
		// itself + ancestor chain + summaries from upstream
		// stages in this run. It is stored on the work item
		// before dispatch; the opencode adapter reads it via
		// the TaskReconciler → manifest Goal.
		//
		// Worker identity (Role / Skills / Behavior / AGENTS.md) is
		// prepended so the visible prompt the operator inspects in
		// the execution detail page is the full context the model
		// actually sees. The runtime delivers this same content as
		// the system prompt via OPENCODE_CONFIG_CONTENT (see the
		// opencode adapter) so the worker identity lands on every
		// conversation turn, not just the first.
		workerVer, err := db.GetWorkerVersionByID(ctx, tx, tenantID, step.Ref, fmt.Sprintf("v%d", step.WorkerVersion))
		if err != nil {
			if err == db.ErrNotFound {
				// Fall back to latest published — supports workflows
				// that don't pin a specific version.
				workerVer, err = db.GetLatestWorkerVersion(ctx, tx, tenantID, step.Ref, true)
				if err != nil {
					return fmt.Errorf("load worker version for %s: %w", step.Ref, err)
				}
			} else {
				return fmt.Errorf("load worker version for %s: %w", step.Ref, err)
			}
		}
		composite, err := r.buildCompositePrompt(ctx, tx, tenantID, wi, workerVer, allSteps, runs)
		if err != nil {
			return fmt.Errorf("build composite prompt for %s: %w", wid, err)
		}
		pcJSON, _ := json.Marshal(map[string]any{
			"composite": composite,
		})
			assignFields := db.UpdateWorkItemFields{
				AssignedWorkerRef: &workerRef,
				WorkflowID:        &wfID,
				WorkflowRunID:     &run.ID,
				WorkflowStepID:    &sr.StepID,
				Status:            strPtr(domain.WorkItemReady),
			}
			if pcJSON != nil {
				assignFields.PromptContext = &pcJSON
			}
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, wi.ID, wi.Version, assignFields); err != nil {
				return fmt.Errorf("assign worker to work item: %w", err)
			}
			primaryWID = wid
		}
		// Record the primary work item id for inline TaskReconciler
		// dispatch after the workflow transaction commits.
		if dispatchedWIDs != nil {
			*dispatchedWIDs = append(*dispatchedWIDs, primaryWID)
		}
		// Record the primary work item id on the step run so
		// pollTaskStep can poll it.
		stepResult, _ := json.Marshal(map[string]string{"_work_item_id": primaryWID})
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:    strPtr(domain.StepRunRunning),
			Result:    &stepResult,
			StartedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark task step running: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
			return fmt.Errorf("enqueue step_started: %w", err)
		}
		r.log.Info("workflow worker dispatched",
			"run", run.ID, "step", step.ID,
			"work_items", upstream, "worker", step.Ref)

	case domain.StepKindDecision:
		// v0.1: default branch (true). Branch-condition evaluation from
		// step.config arrives with richer decision config.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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

	case domain.StepKindLoopDecision:
		// Loop decision step (docs/11 §3): inspect the upstream step's
		// result and the bound work item's decision signal.
		//
		//   Upstream failed       → loop back to loop_branch (if iterations
		//                            < max_iterations) or fail.
		//   Upstream succeeded +
		//     _decision: success  → proceed forward.
		//     _decision: failure  → loop back to loop_branch with full
		//                            context (same as upstream failed).
		//     no _decision field  → re-ask the reviewer (up to max_reask).
		cfg := parseLoopDecisionConfig(step.Config)
		if cfg.LoopBranch == "" || cfg.MaxIterations < 1 {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("loop_decision step %q: missing or invalid config (loop_branch=%q, max_iterations=%d)",
					step.Name, cfg.LoopBranch, cfg.MaxIterations))
		}

		// Find the upstream step run (the one we branch from).
		var upResult struct {
			WorkItemID string `json:"_work_item_id"`
		}
		var upstreamStatus string
		for _, dep := range step.DependsOn {
			if s, ok := runs[dep]; ok {
				upstreamStatus = s.Status
				json.Unmarshal(s.Result, &upResult)
				break
			}
		}
		if upstreamStatus == "" {
			return r.failStep(ctx, tx, tenantID, run, sr, runs,
				fmt.Errorf("loop_decision step %q: no upstream step result found", step.Name))
		}

		// If upstream failed (crash, stall, tool error), trigger recovery
		// and create a new loop decision iteration so downstream steps
		// (e.g. QA Engineer) block until the recovery cycle completes
		// and the re-dispatched reviewer produces a valid decision.
		// The old code marked the loop decision SUCCEEDED here, which
		// satisfied downstream deps and let them run in parallel with
		// the recovery — wrong.
		if upstreamStatus != domain.StepRunSucceeded {
			if r.recovery != nil && upResult.WorkItemID != "" {
				failedExecID := ""
				if latest, err := db.GetLatestExecutionForTask(ctx, tx, tenantID, upResult.WorkItemID); err == nil {
					failedExecID = latest.ID
				}
				if err := r.recovery.TriggerOnFailure(ctx, tenantID, upResult.WorkItemID, failedExecID, "loop_decision:upstream_failed"); err != nil {
					r.log.Warn("loop_decision: trigger recovery on failure", "run", run.ID, "step", step.ID, "work_item", upResult.WorkItemID, "error", err)
				}
			}
			nextIter := currentLoopIteration(runs, step.ID) + 1
			if err := r.createLoopDecisionIteration(ctx, tx, tenantID, run, sr, step, runs, nextIter, now, `{"loop":"recovered"}`); err != nil {
				return err
			}
			r.log.Info("loop_decision: upstream failed, new iteration waiting", "run", run.ID, "step", step.ID)
			break
		}

		// Upstream succeeded. Parse the work item's results for a decision signal.
		var decision string
		if upResult.WorkItemID != "" {
			wi, err := db.GetWorkItem(ctx, tx, tenantID, upResult.WorkItemID)
			if err == nil && len(wi.Results) > 0 {
				var wiResult map[string]any
				if json.Unmarshal(wi.Results, &wiResult) == nil {
					if v, ok := wiResult[cfg.DecisionField]; ok {
						decision, _ = v.(string)
					}
				}
			}
		}

		switch decision {
		case cfg.SuccessValue:
			// Decision is success → proceed forward.
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:    strPtr(domain.StepRunSucceeded),
				StartedAt: &now,
				EndedAt:   &now,
			})
			if err != nil {
				return fmt.Errorf("mark loop_decision step succeeded: %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
				return fmt.Errorf("enqueue loop_decision step_succeeded: %w", err)
			}
			r.log.Info("loop_decision: accepted", "run", run.ID, "step", step.ID)

		case cfg.FailureValue:
			// Decision is failure → loop back to loop_branch with full context.
			currentIter := currentLoopIteration(runs, cfg.LoopBranch)
			if currentIter >= cfg.MaxIterations {
				// Fail even though the step run succeeded — the reviewer
				// determined the work is not acceptable.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: max_iterations (%d) exhausted (rejected by reviewer)", step.Name, cfg.MaxIterations))
			}
			r.log.Info("loop_decision: rejected, looping back",
				"run", run.ID, "step", step.ID, "loop_branch", cfg.LoopBranch, "iteration", currentIter)
			if err := r.loopDecisionReenter(ctx, tx, tenantID, run, sr, step, runs, cfg.LoopBranch, currentIter, now, allSteps); err != nil {
				return err
			}

		default:
			// No decision field found. Re-ask the reviewer.
			reviewerStepID := ""
			for _, dep := range step.DependsOn {
				reviewerStepID = dep
				break
			}
			if reviewerStepID == "" {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: no upstream step to re-ask", step.Name))
			}

			reaskCount := currentLoopIteration(runs, reviewerStepID)
			if reaskCount >= cfg.MaxReask {
				// Re-ask exhausted — fail the loop node even though the step
				// run succeeded, because the reviewer never provided a decision.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: reviewer did not provide decision signal after %d attempts", step.Name, cfg.MaxReask))
			}

			r.log.Info("loop_decision: re-asking reviewer",
				"run", run.ID, "step", step.ID, "reviewer_step", reviewerStepID, "attempt", reaskCount+1)
			if _, err := r.reaskDecisionStep(ctx, tx, tenantID, run, step, sr, runs, reviewerStepID, now); err != nil {
				return fmt.Errorf("loop_decision step %q: re-ask: %w", step.Name, err)
			}

			// Create a new loop decision iteration (supersedes the
			// current one) so downstream steps (e.g. QA Engineer)
			// do NOT start until the re-asked reviewer finishes.
			// The old code marked the loop decision as SUCCEEDED
			// immediately, which satisfied the downstream step's
			// dependency check and let it run in parallel with the
			// re-ask — wrong.
			nextIter := currentLoopIteration(runs, step.ID) + 1
			if err := r.createLoopDecisionIteration(ctx, tx, tenantID, run, sr, step, runs, nextIter, now, `{"loop":"re-ask"}`); err != nil {
				return err
			}
		}

	case domain.StepKindParallel:
		// Fan-out: mark succeeded; downstream steps that depend on this
		// one become ready on the next pass (their deps are now
		// satisfied).
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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

	case domain.StepKindApproval:
		// Block at approval_pending (docs/02 §2.4). Human approval
		// wiring (an ApproveStep RPC + Policy-derived decision) arrives
		// with the Policy engine, Phase 7. The run view shows the step
		// waiting.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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

// succeedStep marks a passive step (project, work_item) as succeeded and
// emits the success event. Used by dispatchStep for non-dispatching kinds.
func (r *WorkflowReconciler) succeedStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, now time.Time, _ string) error {
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:    strPtr(domain.StepRunSucceeded),
		StartedAt: &now,
		EndedAt:   &now,
	})
	if err != nil {
		return fmt.Errorf("mark step succeeded: %w", err)
	}
	runs[sr.StepID] = updated
	return r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated)
}

// failStep marks a step as failed with the given reason and emits the
// failed event. Used by dispatchStep for missing-binding failures.
// Returns nil on success — the failure is persisted in the transaction;
// returning the reason would cause the caller to abort the entire
// reconcileRun and roll back the step failure (pre-existing bug fix).
func (r *WorkflowReconciler) failStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, reason error) error {
	now := time.Now().UTC()
	msg := reason.Error()
	result, _ := json.Marshal(map[string]string{"error": msg})
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:  strPtr(domain.StepRunFailed),
		Result:  &result,
		EndedAt: &now,
	})
	if err != nil {
		return fmt.Errorf("mark step failed: %w", err)
	}
	runs[sr.StepID] = updated
	r.log.Info("step failed", "run", run.ID, "step", sr.StepID, "reason", msg)
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
		return fmt.Errorf("enqueue step_failed: %w", err)
	}
	return nil
}

// buildCompositePrompt assembles the prompt text the worker should see
// when this work item is dispatched (PR B — context propagation). It
// has the following sections:
//
//   0. # Worker — the worker's identity (Role / Skills / Behavior /
//      AGENTS.md). Prepended so the visible prompt the operator
//      inspects in the execution detail page is the full context
//      the model actually sees. The runtime delivers the same
//      content as the system prompt via OPENCODE_CONFIG_CONTENT
//      (see the opencode adapter) so the worker identity lands on
//      every conversation turn, not just the first.
//   1. # Project — the project directory (working dir) + the
//      contents of every file in `context_files` so the model
//      doesn't have to guess at file paths.
//   2. # Task — the work item itself: title, description, acceptance
//      criteria. This is THE task; everything else is context.
//   3. # Project context — the ancestor chain walked via
//      work_items.parent_id (oldest first).
//   4. # Workflow context — a chronological timeline of every step in
//      this run, in DAG order, with each step's status and the
//      execution results it produced. The current step is marked so
//      the worker can see what has come before and what is expected
//      next. Includes:
//        - TASK steps: worker's full output (truncated if huge) and
//          the extracted ORCHICON WORKER SUMMARY.
//        - RECOVER steps: recovery execution summary, status, and
//          strategy. Tells the next worker what went wrong on a
//          prior failure and what was tried.
//        - WORK_ITEM / PROJECT steps: linked work item title + short
//          description (passive context markers).
//        - DECISION / APPROVAL / PARALLEL steps: status only.
//
//   5. # Recovery context (this task) — if THIS work item was
//      recovered from a previous execution failure, the recovery
//      summary is included here verbatim (recovery engine writes it
//      to the work item's results). Distinct from the per-step
//      recovery timeline above: this is the recovery for the task
//      the worker is about to execute, not for prior steps.
//   6. # Instructions — the worker's contract: emit the
//      ORCHICON WORKER SUMMARY marker at the end of the response so
//      the next stage can read it as upstream context.
//
// The composite is the opencode adapter's "message" (passed via the
// manifest Goal). The worker is instructed via the prompt's footer to
// end its response with `ORCHICON WORKER SUMMARY:` followed by a short
// summary that becomes the next stage's upstream context.
func (r *WorkflowReconciler) buildCompositePrompt(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, worker db.WorkerVersionRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, error) {
	var sb strings.Builder
	// 0. Worker identity (prepended so it's visible in the prompt
	// the operator inspects).
	workerIdentity := composeSystemPrompt(worker)
	if workerIdentity != "" {
		sb.WriteString("# Worker\n\n")
		sb.WriteString(workerIdentity)
		sb.WriteString("\n\n")
	}
	// 0b. Project (directory + context file contents). We read
	// contents from disk so the model doesn't have to. Best-effort:
	// a missing file logs a note but doesn't abort dispatch.
	if wi.ProjectID != "" {
		var p db.ProjectRow
		if err := tx.QueryRow(ctx,
			`SELECT project_dir, context_files FROM projects WHERE id = $1 AND tenant_id = $2`,
			wi.ProjectID, tenantID,
		).Scan(&p.ProjectDir, &p.ContextFiles); err == nil {
			hasProject := false
			if p.ProjectDir != "" {
				if !hasProject {
					sb.WriteString("# Project\n\n")
					hasProject = true
				}
				fmt.Fprintf(&sb, "Project directory (working dir for all file operations): `%s`\n\n", p.ProjectDir)
			}
			var files []string
			_ = json.Unmarshal(p.ContextFiles, &files)
			for _, f := range files {
				resolved := f
				if !filepath.IsAbs(resolved) && p.ProjectDir != "" {
					resolved = filepath.Join(p.ProjectDir, resolved)
				}
				if !filepath.IsAbs(resolved) {
					continue
				}
				data, err := os.ReadFile(resolved)
				if err != nil {
					if !hasProject {
						sb.WriteString("# Project\n\n")
						hasProject = true
					}
					fmt.Fprintf(&sb, "**Note:** failed to read context file `%s`: %v\n\n", resolved, err)
					continue
				}
				if !hasProject {
					sb.WriteString("# Project\n\n")
					hasProject = true
				}
				fmt.Fprintf(&sb, "## %s\n\n```\n%s\n```\n\n", resolved, string(data))
			}
		}
	}
	// 1. Task.
	// Show the original work item as the overall goal, then anchor
	// the worker to its purpose so the task title ("Create a bash
	// script") doesn't override the worker's actual role ("Review,
	// don't write code"). The worker's Purpose field is set on the
	// Worker profile by the author and travels with every dispatch.
	var workerPurpose string
	var wkrRow db.WorkerRow
	if err := tx.QueryRow(ctx,
		`SELECT purpose FROM workers WHERE id = $1 AND tenant_id = $2`,
		worker.WorkerID, tenantID,
	).Scan(&wkrRow.Purpose); err == nil {
		workerPurpose = strings.TrimSpace(wkrRow.Purpose)
	}
	sb.WriteString("# Task\n\n")
	fmt.Fprintf(&sb, "Original work item: \"%s\"\n\n", strings.TrimSpace(wi.Title))
	if d := strings.TrimSpace(wi.Description); d != "" {
		fmt.Fprintf(&sb, "Description:\n%s\n\n", d)
	}
	if ac := strings.TrimSpace(wi.AcceptanceCriteria); ac != "" {
		fmt.Fprintf(&sb, "Acceptance criteria:\n%s\n\n", ac)
	}
	if workerPurpose != "" {
		fmt.Fprintf(&sb, "---\n\n**Your purpose on this step:** %s\n\nFocus on the acceptance criteria above — they define what \"done\" looks like. The overall goal is described above; your specific job is to execute your purpose against it, not to reproduce the original task literally.\n\n---\n\n", workerPurpose)
	}
	// 2. Project context — ancestors, oldest first.
	ancestors, err := walkAncestors(ctx, tx, tenantID, wi)
	if err != nil {
		return "", fmt.Errorf("walk ancestors: %w", err)
	}
	if len(ancestors) > 0 {
		sb.WriteString("# Project context\n\n")
		sb.WriteString("The items below are ancestor work items (epic → feature → task). They provide project context; the task above is the actual work to do.\n\n")
		for _, a := range ancestors {
			fmt.Fprintf(&sb, "## %s (%s)\n", strings.TrimSpace(a.Title), workItemKindLabel(a.Kind))
			if d := strings.TrimSpace(a.Description); d != "" {
				fmt.Fprintf(&sb, "%s\n", d)
			}
			sb.WriteString("\n")
		}
	}
	// 3. Workflow context — full timeline of every step in this run.
	// Walks allSteps in DAG order, inlines the result of every step
	// (TASK full output + summary, RECOVER narrative, WORK_ITEM
	// title, etc.) and marks the current step so the worker can
	// orient itself. See upstreamContext for the per-step rendering.
	wctx, err := r.upstreamContext(ctx, tx, tenantID, wi, allSteps, runs)
	if err != nil {
		return "", fmt.Errorf("build workflow context: %w", err)
	}
	if wctx != "" {
		sb.WriteString(wctx)
	}
	// 4a. Recovery context (this task) — this work item may have been
	// recovered from a previous execution failure. The recovery engine
	// writes _recovery_summary into the work item's results when it
	// transitions the task back to ready; inject it here so the
	// replacement execution knows what went wrong and what was
	// recovered.
	if len(wi.Results) > 0 {
		var wiParsed map[string]any
		if err := json.Unmarshal(wi.Results, &wiParsed); err == nil {
			if rSummary, ok := wiParsed["_recovery_summary"].(string); ok && rSummary != "" {
				sb.WriteString("# Recovery context (this task)\n\n")
				sb.WriteString("The previous execution for this task failed and was automatically recovered. The following is a summary of what happened:\n\n")
				sb.WriteString(rSummary)
				sb.WriteString("\n\n")
			}
		}
	}
	// 4b. Previous review feedback — when the loop routes back to
	// SSE after PR Reviewer found issues, the _issues field carries
	// the PR Reviewer's findings. Include them here so the SSE knows
	// what needs fixing even though the previous PR Reviewer step
	// run was superseded and its output is no longer in the active
	// workflow context (the new cycle's PR Reviewer hasn't run yet).
	if len(wi.Results) > 0 {
		var wiParsed map[string]any
		if err := json.Unmarshal(wi.Results, &wiParsed); err == nil {
			if issues, ok := wiParsed["_issues"].(string); ok && issues != "" {
				sb.WriteString("# Previous review feedback\n\n")
				sb.WriteString("The following issues were identified by the reviewer. Address them in your work:\n\n")
				sb.WriteString(issues)
				sb.WriteString("\n\n")
			}
		}
	}
	// 5. File context — kept for backward compatibility (old
	// composite shape). The prepended # Project block above already
	// inlines file contents; this section is a fallback if a caller
	// still has it set without a ProjectID.
	if wi.ProjectID != "" {
		fileCtx, err := r.readProjectContextFiles(ctx, tx, tenantID, wi.ProjectID)
		if err != nil {
			r.log.Warn("failed to read project context files", "project_id", wi.ProjectID, "work_item_id", wi.ID, "error", err)
		} else if fileCtx != "" {
			sb.WriteString(fileCtx)
		}
	}
	// 6. Footer: instruction for the worker to emit the summary marker.
	sb.WriteString("# Instructions\n\n")
	sb.WriteString("Complete the task above. When you have finished, end your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one short paragraph summarizing what you did. Everything from that marker to the end of your output is passed to the next stage of the workflow as upstream context.\n\n")
	sb.WriteString("If you produce an output file (an essay, report, configuration, generated code, or any structured artifact), use the `write` tool to save it instead of `bash` with a heredoc. The `write` tool saves the file and orchicon automatically captures its content as an inline artifact visible in the execution log. Using `write` (not bash heredoc) makes your output visible to the operator without them having to click through tool input.\n")
	return sb.String(), nil
}

// walkAncestors walks the parent_id chain from a work item up to the
// root (epic) and returns the ancestors in root-first order. Stops at
// the first missing parent or after `maxAncestorDepth` hops to defend
// against pathological data. PR B.
func walkAncestors(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow) ([]db.WorkItemRow, error) {
	const maxAncestorDepth = 16
	var out []db.WorkItemRow
	cur := wi
	for i := 0; i < maxAncestorDepth; i++ {
		if cur.ParentID == nil || *cur.ParentID == "" {
			break
		}
		parent, err := db.GetWorkItem(ctx, tx, tenantID, *cur.ParentID)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return nil, err
		}
		out = append(out, parent)
		cur = parent
	}
	// Reverse so the oldest (epic) comes first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// upstreamContext renders a chronological "Workflow context" section
// for the worker: a numbered list of every step in this run, in DAG
// order, with each step's status and the execution results it
// produced. The current step is marked so the worker can see what has
// come before and what is expected next.
//
// Per-step rendering (see renderUpstreamStep):
//
//   - TASK: linked work item (loaded from DB) — its title, the
//     worker's full output (truncated to upstreamOutputMaxChars if
//     huge), the extracted ORCHICON WORKER SUMMARY, and any
//     _recovery_summary on the work item.
//   - RECOVER: linked recovery execution (loaded from DB) — its
//     status, strategy, summary narrative, and trigger reason.
//   - WORK_ITEM: linked work item title + a short description
//     excerpt. These are passive context markers on the canvas, not
//     executed by a worker.
//   - PROJECT: project name only.
//   - DECISION / APPROVAL / PARALLEL: status only (they're branching
//     and gating primitives, not result-bearing).
//
// Returns "" when the run has no step runs yet (first step) so the
// caller can omit the section entirely rather than render an empty
// header. The function walks the DAG by step-id order via allSteps
// (the order the author placed them on the canvas — for a linear
// chain that's left-to-right; for a diamond, the source order
// approximates topological order, which is the best the reconciler
// can do without a full topological sort). Cycles are the caller's
// responsibility to prevent (validated at save time, docs/10 §11).
func (r *WorkflowReconciler) upstreamContext(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, error) {
	// Find the current step (the one being dispatched). The worker
	// step whose result will eventually hold this work item's id is
	// the step we're building the prompt for.
	currentStepID := ""
	if wi.WorkflowStepID != "" {
		currentStepID = wi.WorkflowStepID
	} else {
		// Fallback: scan for the step run that referenced this work
		// item. The reconciler stores _work_item_id on the step run
		// when dispatching; if the work item is being created fresh
		// the field may not be set yet, in which case the prompt
		// builder is being called speculatively — treat as "no
		// current step" so we don't mark the wrong stage.
		for sid, sr := range runs {
			var parsed struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if json.Unmarshal(sr.Result, &parsed) == nil && parsed.WorkItemID == wi.ID {
				currentStepID = sid
				break
			}
		}
	}

	// Count terminal (succeeded/failed/skipped) prior steps to know
	// whether to render the section at all. A run with zero
	// completed steps is a single-step workflow — the task section
	// already conveys what the worker should do.
	hasAnyTerminal := false
	for _, s := range allSteps {
		sr, ok := runs[s.ID]
		if !ok {
			continue
		}
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunFailed || sr.Status == domain.StepRunSkipped {
			hasAnyTerminal = true
			break
		}
	}
	if !hasAnyTerminal {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("# Workflow context\n\n")
	sb.WriteString("This is a chronological view of every step in the workflow run, in DAG order. The current step is marked `→ you are here`. Use prior step results as input to your work and produce the output the next stage will read.\n\n")

	stage := 0
	for _, s := range allSteps {
		sr, ok := runs[s.ID]
		if !ok {
			// Step hasn't been visited yet — skip. (Steps scheduled
			// in the future will appear when they run; the timeline
			// only shows what's already happened.)
			continue
		}
		// Only show steps that have actually started. A pending
		// step is invisible to the worker.
		if sr.Status == domain.StepRunPending || sr.Status == domain.StepRunReady {
			continue
		}
		stage++
		isCurrent := s.ID == currentStepID
		if err := r.renderUpstreamStep(ctx, tx, tenantID, &sb, stage, s, sr, isCurrent); err != nil {
			return "", err
		}
	}

	// If a current step is known, append a brief "next task" reminder
	// so the worker can see at a glance what is expected of them. The
	// "→ you are here" marker lives here rather than in the timeline
	// because the current dispatch is the NEXT step to run — the
	// timeline only contains prior steps that have already started.
	if currentStepID != "" {
		for _, s := range allSteps {
			if s.ID != currentStepID {
				continue
			}
			sb.WriteString("## → Next task (you are here)\n\n")
			fmt.Fprintf(&sb, "You are executing **%s** (%s", strings.TrimSpace(s.Name), stepKindLabel(s.Kind))
			if s.Kind == domain.StepKindTask && s.Ref != "" {
				fmt.Fprintf(&sb, ", worker `%s`", s.Ref)
			}
			sb.WriteString("). Complete the work in the *Task* section above, then end your response with the `ORCHICON WORKER SUMMARY:` marker so the next stage can read your result.\n\n")
			break
		}
	}

	return sb.String(), nil
}

// renderUpstreamStep writes one step's entry to the workflow context
// timeline. See upstreamContext for the per-kind format. Errors are
// returned for genuine DB failures (ErrNotFound is treated as
// "no data available" and the section is rendered with whatever we
// have, so a transient inconsistency doesn't poison the prompt).
func (r *WorkflowReconciler) renderUpstreamStep(ctx context.Context, tx pgx.Tx, tenantID string, sb *strings.Builder, stage int, s workflow.StepWire, sr db.WorkflowStepRunRow, isCurrent bool) error {
	marker := ""
	if isCurrent {
		marker = "  → you are here"
	}
	fmt.Fprintf(sb, "## Stage %d — %s (%s)%s\n", stage, strings.TrimSpace(s.Name), stepKindLabel(s.Kind), marker)

	// Step status. For everything but TASK, this is usually the only
	// information we have; render it inline.
	switch sr.Status {
	case domain.StepRunSucceeded:
		fmt.Fprintf(sb, "Status: succeeded\n")
	case domain.StepRunFailed:
		fmt.Fprintf(sb, "Status: **failed**\n")
	case domain.StepRunSkipped:
		fmt.Fprintf(sb, "Status: skipped\n")
	case domain.StepRunRunning:
		fmt.Fprintf(sb, "Status: running\n")
	case domain.StepRunBlocked:
		fmt.Fprintf(sb, "Status: blocked\n")
	case domain.StepRunApprovalPending:
		fmt.Fprintf(sb, "Status: awaiting approval\n")
	default:
		fmt.Fprintf(sb, "Status: %s\n", sr.Status)
	}

	// Per-kind body. Failures to load referenced rows (e.g. the linked
	// work item for a TASK that was never dispatched) are logged and
	// skipped — the timeline still has the status, which is what the
	// worker most needs.
	switch s.Kind {
	case domain.StepKindTask:
		// Linked work item id is stored in the step run's result
		// JSON when the task was dispatched. We then load the work
		// item to get its full _output + _summary + _recovery_summary
		// from the results JSONB.
		var ref struct {
			WorkItemID string `json:"_work_item_id"`
		}
		if err := json.Unmarshal(sr.Result, &ref); err != nil || ref.WorkItemID == "" {
			break
		}
		wi, err := db.GetWorkItem(ctx, tx, tenantID, ref.WorkItemID)
		if err != nil {
			if err == db.ErrNotFound {
				r.log.Debug("upstream step: work item missing", "step", s.ID, "work_item_id", ref.WorkItemID)
				break
			}
			return fmt.Errorf("load work item for upstream step %s: %w", s.ID, err)
		}
		fmt.Fprintf(sb, "Work item: %s (%s)\n", strings.TrimSpace(wi.Title), workItemKindLabel(wi.Kind))
		// Per-work-item results: _output (worker's full text),
		// _summary (extracted by TaskReconciler), _recovery_summary
		// (set when the recovery engine resumes a failed task).
		var parsed map[string]any
		if len(wi.Results) > 0 {
			_ = json.Unmarshal(wi.Results, &parsed)
		}
		if output, ok := parsed["_output"].(string); ok && output != "" {
			r.writeCappedText(sb, "Output", output, upstreamOutputMaxChars)
		}
		if summary, ok := parsed["_summary"].(string); ok && summary != "" {
			// _summary is the canonical "what the worker did" line
			// the next stage reads; it may already appear in the
			// output block above, but we surface it again as a
			// clear "Summary" field so the worker doesn't have to
			// hunt for the marker.
			fmt.Fprintf(sb, "\nSummary: %s\n", summary)
		}
		if recSummary, ok := parsed["_recovery_summary"].(string); ok && recSummary != "" {
			fmt.Fprintf(sb, "\nRecovery narrative (for this task):\n%s\n", recSummary)
		}

	case domain.StepKindWorkItem:
		// Passive marker. Pull the work item title + a short
		// description snippet so the worker knows what this work
		// item represents (often: the input the downstream TASK is
		// processing).
		wid := readConfigWorkItemID(s.Config)
		if wid == "" {
			break
		}
		wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return fmt.Errorf("load work item for work_item step %s: %w", s.ID, err)
		}
		fmt.Fprintf(sb, "Linked work item: %s (%s)\n", strings.TrimSpace(wi.Title), workItemKindLabel(wi.Kind))
		if d := strings.TrimSpace(wi.Description); d != "" {
			r.writeCappedText(sb, "Description", d, upstreamDescriptionMaxChars)
		}

	case domain.StepKindProject:
		// Passive marker. The project id is in the step config.
		pid := readConfigProjectID(s.Config)
		if pid == "" {
			break
		}
		p, err := db.GetProject(ctx, tx, tenantID, pid)
		if err != nil {
			if err == db.ErrNotFound {
				break
			}
			return fmt.Errorf("load project for project step %s: %w", s.ID, err)
		}
		fmt.Fprintf(sb, "Project: %s\n", strings.TrimSpace(p.Name))
	}

	sb.WriteString("\n")
	return nil
}

// upstreamOutputMaxChars caps the worker's full output inline in the
// workflow context. Beyond this size, the trailing portion is
// truncated with an ellipsis and the worker is told to use the
// ORCHICON WORKER SUMMARY line for the canonical downstream input.
// 16K is roughly 4K tokens — large enough for an essay or chapter,
// small enough that four such stages in a row stay under 64K tokens
// of prompt overhead.
const upstreamOutputMaxChars = 16 * 1024

// upstreamDescriptionMaxChars caps descriptions from passive
// context markers (WORK_ITEM, PROJECT). Smaller than the output cap
// because descriptions are short by design.
const upstreamDescriptionMaxChars = 1024

// writeCappedText writes a label + body to the buffer, truncating
// body to maxChars with a clear "truncated" marker if it would
// otherwise blow the budget. The marker names the marker the worker
// should look at for the canonical downstream input.
func (r *WorkflowReconciler) writeCappedText(sb *strings.Builder, label, body string, maxChars int) {
	fmt.Fprintf(sb, "\n%s:\n", label)
	if len(body) <= maxChars {
		sb.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			sb.WriteString("\n")
		}
		return
	}
	// Truncate at a sensible boundary: find the last newline before
	// the cap so we don't slice a word in half.
	cut := maxChars
	if nl := strings.LastIndex(body[:cut], "\n"); nl > cut/2 {
		cut = nl
	}
	sb.WriteString(body[:cut])
	sb.WriteString("\n…[truncated — see the ORCHICON WORKER SUMMARY below for the canonical downstream input]\n")
}

// stepKindLabel returns a human-readable label for a workflow step's
// kind. Used in the workflow-context timeline header.
func stepKindLabel(kind string) string {
	switch kind {
	case domain.StepKindTask:
		return "task"
	case domain.StepKindDecision:
		return "decision"
	case domain.StepKindApproval:
		return "approval"
	case domain.StepKindParallel:
		return "parallel"
	case domain.StepKindRecover:
		return "recovery"
	case domain.StepKindWorkItem:
		return "work item"
	case domain.StepKindProject:
		return "project"
	default:
		return kind
	}
}

// readProjectContextFiles lists the project's context_files as absolute
// paths so the worker can read them from disk. No file contents are sent
// — only the paths. Directories are listed as-is; the worker is instructed
// to read them.
//
// Paths are expected to be absolute. For backward compatibility, relative
// paths are resolved against project_dir if it is set.
func (r *WorkflowReconciler) readProjectContextFiles(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (string, error) {
	p, err := db.GetProject(ctx, tx, tenantID, projectID)
	if err != nil {
		return "", fmt.Errorf("get project for context files: %w", err)
	}
	if len(p.ContextFiles) == 0 {
		return "", nil
	}
	var files []string
	if err := json.Unmarshal(p.ContextFiles, &files); err != nil {
		return "", fmt.Errorf("parse context_files JSON: %w", err)
	}
	if len(files) == 0 {
		return "", nil
	}
	var sb strings.Builder
	sb.WriteString("# File context\n\n")
	sb.WriteString("The following files and directories are provided as project context. Please fully read the contents of each file, and for directories, read all files within them, before starting your work.\n\n")
	for _, path := range files {
		cleaned := filepath.Clean(path)
		// Backward compat: resolve relative paths against project_dir.
		if !filepath.IsAbs(cleaned) && p.ProjectDir != "" {
			cleaned = filepath.Join(p.ProjectDir, cleaned)
		}
		if !filepath.IsAbs(cleaned) {
			continue
		}
		fmt.Fprintf(&sb, "- `%s`\n", cleaned)
	}
	sb.WriteString("\n")
	return sb.String(), nil
}

// workItemKindLabel returns a human-readable label for a work item's
// kind enum value (1=task, 2=feature, 3=epic, 4=subtask).
func workItemKindLabel(kind string) string {
	switch kind {
	case "task":
		return "task"
	case "feature":
		return "feature"
	case "epic":
		return "epic"
	case "subtask":
		return "subtask"
	default:
		return kind
	}
}

// readConfigWorkItemID extracts work_item_id from a step's config JSON.
// Returns "" for empty / malformed / missing config.
func readConfigWorkItemID(config string) string {
	if config == "" {
		return ""
	}
	var parsed struct {
		WorkItemID string `json:"work_item_id"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return ""
	}
	return parsed.WorkItemID
}

// readConfigProjectID extracts project_id from a step's config JSON.
// Returns "" for empty / malformed / missing config.
func readConfigProjectID(config string) string {
	if config == "" {
		return ""
	}
	var parsed struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return ""
	}
	return parsed.ProjectID
}

// readConfigStrategy extracts the recovery strategy from a step's
// config JSON. PR D — recovery palette tiles (stop, summarize_restart,
// human_escalation, retry_n) write this field. The reconciler logs it
// when a RECOVER step is reached so operators can see what the
// runtime would have done on a real failure. Returns "" for empty /
// missing.
// stepRecoveryConfig is the per-step recovery configuration embedded in
// a task step's config JSON under the "recovery" key. Controls what
// happens when the step's work item fails after blind-retry attempts
// are exhausted.
//
// Strategy options:
//   - "retry" (default): blind retry — create a fresh work item and
//     re-dispatch immediately.
//   - "summarize_restart": trigger the recovery engine to capture,
//     summarize, and resume the failed work item with context.
//   - "human_escalation": set the step to approval_pending; a human
//     must mark the task succeeded to continue.
//   - "stop": permanent failure — no retry, step is marked failed.
type stepRecoveryConfig struct {
	Strategy          string `json:"strategy"`
	MaxAttempts       int    `json:"max_attempts"`
	RetryDelaySeconds int    `json:"retry_delay_seconds"`
}

// readStepRecoveryConfig reads the "recovery" block from the step's
// config JSON. Returns defaults (strategy="retry", max_attempts=3) for
// any missing field.
func readStepRecoveryConfig(config string) stepRecoveryConfig {
	cfg := stepRecoveryConfig{
		Strategy:    "retry",
		MaxAttempts: 3,
	}
	if config == "" {
		return cfg
	}
	var outer struct {
		Recovery stepRecoveryConfig `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(config), &outer); err != nil {
		return cfg
	}
	if outer.Recovery.Strategy != "" {
		cfg.Strategy = outer.Recovery.Strategy
	}
	if outer.Recovery.MaxAttempts > 0 {
		cfg.MaxAttempts = outer.Recovery.MaxAttempts
	}
	if outer.Recovery.RetryDelaySeconds > 0 {
		cfg.RetryDelaySeconds = outer.Recovery.RetryDelaySeconds
	}
	return cfg
}

// upstreamWorkItemIDs walks step.DependsOn looking for WORK_ITEM steps
// and returns the work_item_ids they reference. Order matches
// step.DependsOn so callers can pick a deterministic primary input.
func upstreamWorkItemIDs(step workflow.StepWire, allSteps []workflow.StepWire) []string {
	byID := make(map[string]workflow.StepWire, len(allSteps))
	for _, s := range allSteps {
		byID[s.ID] = s
	}
	var ids []string
	for _, dep := range step.DependsOn {
		ds, ok := byID[dep]
		if !ok {
			continue
		}
		if ds.Kind != domain.StepKindWorkItem {
			continue
		}
		if wid := readConfigWorkItemID(ds.Config); wid != "" {
			ids = append(ids, wid)
		}
	}
	return ids
}

// pollTaskStep checks the WorkItem linked to a running task step. Returns
// (terminal, failed, error). The WorkItem id is stored in the step run's
// result JSON under _work_item_id.
//
// When the work item has failed and the step has remaining retry attempts,
// behaviour depends on the step's recovery strategy (from
// stepConfig.recovery):
//
//   - "retry" (default): creates a fresh work item and re-dispatches
//     immediately. The step transitions to "recovering".
//   - "summarize_restart": triggers the recovery engine to capture,
//     summarize, and resume the failed work item. The step waits in
//     "recovering" until the recovery completes.
//   - "human_escalation": sets the step to "approval_pending"; a human
//     must mark the task succeeded to continue.
//   - "stop": permanent failure — no retry.
//
// Terminal failure only occurs after all attempts are exhausted,
// regardless of strategy.
func (r *WorkflowReconciler) pollTaskStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, stepConfig string, runByID map[string]db.WorkflowStepRunRow) (bool, bool, error) {
	var parsed struct {
		WorkItemID string `json:"_work_item_id"`
	}
	if err := json.Unmarshal(sr.Result, &parsed); err != nil || parsed.WorkItemID == "" {
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
		rc := readStepRecoveryConfig(stepConfig)

		if sr.Attempt >= rc.MaxAttempts-1 {
			return true, true, nil
		}

		newAttempt := sr.Attempt + 1
		r.log.Info("task step failed, running recovery strategy",
			"run", run.ID, "step", sr.StepID,
			"work_item", wi.ID, "attempt", newAttempt, "max_attempts", rc.MaxAttempts,
			"strategy", rc.Strategy)

		switch rc.Strategy {
		case "retry", "":
			freshID := db.NewID()
			fresh := db.WorkItemRow{
				ID:                 freshID,
				TenantID:           wi.TenantID,
				ProjectID:          wi.ProjectID,
				ParentID:           wi.ParentID,
				Kind:               wi.Kind,
				Title:              wi.Title,
				Description:        wi.Description,
				AcceptanceCriteria: wi.AcceptanceCriteria,
				Status:             domain.WorkItemPending,
				Priority:           wi.Priority,
				Budgets:            wi.Budgets,
				ContextWindow:      wi.ContextWindow,
				WorkflowID:         wi.WorkflowID,
				WorkflowRunID:      wi.WorkflowRunID,
				WorkflowStepID:     wi.WorkflowStepID,
				Results:            []byte("{}"),
				PromptContext:      wi.PromptContext,
			}
			if _, err := db.CreateWorkItem(ctx, tx, fresh); err != nil {
				return false, false, fmt.Errorf("create retry work item: %w", err)
			}
			stepResult, _ := json.Marshal(map[string]string{"_work_item_id": freshID})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunRecovering),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for retry: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case "summarize_restart":
			if r.recovery != nil {
				failedExecID := r.getFailedExecID(ctx, tx, tenantID, parsed.WorkItemID)
				if err := r.recovery.TriggerOnFailure(ctx, tenantID, parsed.WorkItemID, failedExecID, "step_recovery"); err != nil {
					r.log.Warn("step recovery: trigger on failure", "work_item", parsed.WorkItemID, "error", err)
				}
			}
			stepResult, _ := json.Marshal(map[string]string{"_work_item_id": parsed.WorkItemID})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunRecovering),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for recovery: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case "human_escalation":
			stepResult, _ := json.Marshal(map[string]string{
				"_work_item_id": parsed.WorkItemID,
				"_decision":     "pending",
			})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunApprovalPending),
				Attempt: &newAttempt,
				Result:  &stepResult,
			})
			if err != nil {
				return false, false, fmt.Errorf("update step run for human escalation: %w", err)
			}
			runByID[sr.StepID] = updated
			return false, false, nil

		case "stop":
			return true, true, nil

		default:
			return true, true, nil
		}

	default:
		return false, false, nil
	}
}

// getFailedExecID returns the latest execution ID for the given work
// item. Best-effort — returns "" on any error so recovery can still
// proceed without a specific execution reference.
func (r *WorkflowReconciler) getFailedExecID(ctx context.Context, tx pgx.Tx, tenantID, workItemID string) string {
	exec, err := db.GetLatestExecutionForTask(ctx, tx, tenantID, workItemID)
	if err != nil {
		return ""
	}
	return exec.ID
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
// --- loop decision helpers (docs/11 §3) -----------------------------------

// loopDecisionConfig is the step config for a loop_decision node.
type loopDecisionConfig struct {
	BranchFrom     string `json:"branch_from"`
	SuccessBranch  string `json:"success_branch"`
	LoopBranch     string `json:"loop_branch"`
	MaxIterations  int    `json:"max_iterations"`
	DecisionField  string `json:"decision_field"`  // field name in work item results to check; default "_decision"
	SuccessValue   string `json:"success_value"`   // value meaning success; default "success"
	FailureValue   string `json:"failure_value"`   // value meaning failure; default "failure"
	MaxReask       int    `json:"max_reask"`        // max re-ask attempts when no decision field found; default 3
}

func parseLoopDecisionConfig(config string) loopDecisionConfig {
	var cfg loopDecisionConfig
	json.Unmarshal([]byte(config), &cfg)
	if cfg.DecisionField == "" {
		cfg.DecisionField = "_decision"
	}
	if cfg.SuccessValue == "" {
		cfg.SuccessValue = "success"
	}
	if cfg.FailureValue == "" {
		cfg.FailureValue = "failure"
	}
	if cfg.MaxReask <= 0 {
		cfg.MaxReask = 3
	}
	return cfg
}

// currentLoopIteration returns the current iteration count for a step
// within a run. This is MAX(iteration) over all step runs for the step
// where superseded_by IS NULL (the active run).
func currentLoopIteration(runs map[string]db.WorkflowStepRunRow, stepID string) int {
	var maxIter int
	for _, sr := range runs {
		if sr.StepID == stepID && sr.SupersededBy == "" && sr.Iteration > maxIter {
			maxIter = sr.Iteration
		}
	}
	return maxIter
}

// listStepRunsByStepID fetches all step runs for a given run+step.
func listStepRunsByStepID(ctx context.Context, tx pgx.Tx, tenantID, runID, stepID string) ([]db.WorkflowStepRunRow, error) {
	all, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	var out []db.WorkflowStepRunRow
	for _, sr := range all {
		if sr.StepID == stepID {
			out = append(out, sr)
		}
	}
	return out, nil
}

// reaskDecisionStep creates a new step run for the reviewer step (the
// loop decision's upstream dependency) to re-ask for a decision signal.
// Returns the work item id so callers can optionally amend the prompt.
func (r *WorkflowReconciler) reaskDecisionStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, reaskStepID string, now time.Time) (string, error) {
	nextIter := currentLoopIteration(runs, reaskStepID) + 1

	// Supersede the previous run of the re-ask target step.
	srList, err := listStepRunsByStepID(ctx, tx, tenantID, run.ID, reaskStepID)
	if err != nil {
		return "", fmt.Errorf("reask: list prior runs: %w", err)
	}
	for _, prior := range srList {
		if prior.SupersededBy == "" {
			if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, prior.ID, prior.Version, db.UpdateWorkflowStepRunFields{
				SupersededBy: &sr.ID,
			}); err != nil {
				return "", fmt.Errorf("reask: supersede prior run: %w", err)
			}
		}
	}

	// Look up the work item from the upstream step's result to amend its
	// prompt_context with the re-ask instruction.
	var upResult struct {
		WorkItemID string `json:"_work_item_id"`
	}
	for _, dep := range step.DependsOn {
		if s, ok := runs[dep]; ok {
			json.Unmarshal(s.Result, &upResult)
			break
		}
	}

	if upResult.WorkItemID != "" {
		wi, err := db.GetWorkItem(ctx, tx, tenantID, upResult.WorkItemID)
		if err == nil {
			reaskMsg := "\n\n# Re-ask\n\n**RE-ASK ATTEMPT " + fmt.Sprint(nextIter) + "**\n" +
				"You MUST include `_decision` in your response with a value of `success` or `failure`.\n" +
				"- `_decision: success` — the work is complete and correct\n" +
				"- `_decision: failure` — there are issues that need fixing (include `_issues` with details)"

			// Preserve the existing composite (worker identity, project,
			// task, ancestors, workflow context, recovery, instructions)
			// and append the re-ask instruction so the worker doesn't lose
			// all context on a re-ask turn. Previously this overwrote the
			// entire promptContext with just the re-ask message — the
			// worker saw only "RE-ASK ATTEMPT 1" and nothing else.
			var pc struct {
				Composite string `json:"composite"`
			}
			existingPC := ""
			_ = json.Unmarshal(wi.PromptContext, &pc)
			if pc.Composite != "" {
				existingPC = pc.Composite
			}
			combined := existingPC + reaskMsg
			pcJSON, _ := json.Marshal(map[string]any{
				"composite": combined,
			})
			if _, err := db.UpdateWorkItem(ctx, tx, tenantID, wi.ID, wi.Version, db.UpdateWorkItemFields{
				PromptContext: &pcJSON,
			}); err != nil {
				r.log.Warn("reask: update work item prompt context", "work_item", wi.ID, "error", err)
			}
		}
	}

	// Create a new step run for the re-ask target.
	newStepRun := db.WorkflowStepRunRow{
		ID:            db.NewID(),
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        reaskStepID,
		StepName:      "Reviewer (re-ask)",
		StepKind:      domain.StepKindTask,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newStepRun); err != nil {
		return "", fmt.Errorf("reask: create re-ask run: %w", err)
	}

	return upResult.WorkItemID, nil
}

// createLoopDecisionIteration creates a new iteration of a loop_decision
// step run with PENDING status (blocking downstream steps) and supersedes
// the current iteration. The new iteration runs when its deps (the
// re-asked reviewer) complete.
func (r *WorkflowReconciler) createLoopDecisionIteration(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, nextIter int, now time.Time, resultJSON string) error {
	// Create the new iteration first so we can set SupersededBy.
	newID := db.NewID()
	resultRaw := []byte(resultJSON)
	newIter := db.WorkflowStepRunRow{
		ID:            newID,
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        step.ID,
		StepName:      step.Name,
		StepKind:      domain.StepKindLoopDecision,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newIter); err != nil {
		return fmt.Errorf("loop_decision step %q: create next iteration: %w", step.Name, err)
	}

	// Supersede the current loop decision run, pointing at the new one.
	superseded, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:       strPtr(domain.StepRunSucceeded),
		StartedAt:    &now,
		EndedAt:      &now,
		SupersededBy: &newID,
		Result:       &resultRaw,
	})
	if err != nil {
		return fmt.Errorf("loop_decision step %q: supersede prior iteration: %w", step.Name, err)
	}
	// put the superseded run in the map for the event; the new pending
	// run will be loaded on the next pass and overwrite in runByID.
	runs[step.ID] = superseded
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, superseded); err != nil {
		return fmt.Errorf("enqueue loop_decision step_succeeded (supersede): %w", err)
	}
	r.log.Info("loop_decision iteration created",
		"run", run.ID, "step", step.ID, "iteration", nextIter)
	return nil
}

// loopDecisionReenter creates a new step run for the loop branch
// target, then marks the loop decision as RUNNING. Downstream steps
// are blocked because the loop decision is no longer terminal.
// Previously it created a new loop decision iteration (PENDING),
// but the new iteration's deps (PR Reviewer) were already SUCCEEDED,
// so it dispatched immediately and re-entered again — infinite loop.
func (r *WorkflowReconciler) loopDecisionReenter(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, loopBranch string, currentIter int, now time.Time, allSteps []workflow.StepWire) error {
	nextIter := currentIter + 1

	// Create new step runs for every step between the loop target
	// and this loop decision (inclusive of target, exclusive of
	// decision) so the whole chain re-executes.
	chainIDs := chainStepIDs(allSteps, loopBranch, step.ID)
	if err := r.createChainRuns(ctx, tx, tenantID, run.ID, chainIDs, nextIter, allSteps); err != nil {
		return fmt.Errorf("loop_decision step %q: create chain runs: %w", step.Name, err)
	}

	// Mark the loop decision as RUNNING so it blocks downstream
	// steps (QA Engineer). The poll phase checks whether the
	// re-entered chain completed.
	updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Status:  strPtr(domain.StepRunRunning),
		Result:  func() *[]byte { r := []byte(`{"loop":"re-entered"}`); return &r }(),
	})
	if err != nil {
		return fmt.Errorf("loop_decision step %q: mark running: %w", step.Name, err)
	}
	runs[step.ID] = updated
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
		return fmt.Errorf("enqueue loop_decision step_started: %w", err)
	}
	r.log.Info("loop_decision re-entered",
		"run", run.ID, "step", step.ID, "loop_branch", loopBranch,
		"iteration", nextIter, "chain", chainIDs)
	return nil
}

// chainStepIDs returns the IDs of every step from `fromID` (inclusive)
// to `toID` (exclusive) in DAG order. allSteps is the workflow
// version's step list in canvas order, which for a linear chain is
// DAG order. Returns nil if either step is not found or the range
// is inverted.
func chainStepIDs(allSteps []workflow.StepWire, fromID, toID string) []string {
	start, end := -1, -1
	for i, s := range allSteps {
		if s.ID == fromID {
			start = i
		}
		if s.ID == toID {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || start >= end {
		return nil
	}
	ids := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		ids = append(ids, allSteps[i].ID)
	}
	return ids
}

// createChainRuns supersedes existing step runs for each step in the
// given list and creates new PENDING iterations. The step kind is
// looked up from allSteps (the workflow version so recover steps
// are correctly typed). Each new run carries the provided iteration.
func (r *WorkflowReconciler) createChainRuns(ctx context.Context, tx pgx.Tx, tenantID, runID string, stepIDs []string, iteration int, allSteps []workflow.StepWire) error {
	// Build a stepKind lookup from the workflow version.
	kindByID := make(map[string]string, len(allSteps))
	for _, s := range allSteps {
		kindByID[s.ID] = s.Kind
	}
	for _, sid := range stepIDs {
		srList, err := listStepRunsByStepID(ctx, tx, tenantID, runID, sid)
		if err != nil {
			return fmt.Errorf("list runs for chain step %s: %w", sid, err)
		}
		var priorID string
		for _, prior := range srList {
			if prior.SupersededBy == "" {
				newID := db.NewID()
				priorID = newID
				if _, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, prior.ID, prior.Version, db.UpdateWorkflowStepRunFields{
					SupersededBy: &newID,
				}); err != nil {
					return fmt.Errorf("supersede chain step %s: %w", sid, err)
				}
				break
			}
		}
		if priorID == "" {
			priorID = db.NewID()
		}
		kind := kindByID[sid]
		if kind == "" {
			kind = domain.StepKindTask
		}
		newRun := db.WorkflowStepRunRow{
			ID:            priorID,
			TenantID:      tenantID,
			WorkflowRunID: runID,
			StepID:        sid,
			StepName:      sid + " (loop)",
			StepKind:      kind,
			Status:        domain.StepRunPending,
			Iteration:     iteration,
		}
		if _, err := db.CreateWorkflowStepRun(ctx, tx, newRun); err != nil {
			return fmt.Errorf("create chain step run %s: %w", sid, err)
		}
	}
	return nil
}

// pollLoopDecisionChain checks whether every step in the chain between
// the loop decision's loop_branch target and its upstream dependency
// has completed (all terminal). Callers re-mark the loop decision as
// READY when true.
func (r *WorkflowReconciler) pollLoopDecisionChain(ctx context.Context, tx pgx.Tx, tenantID string, sr db.WorkflowStepRunRow, allSteps []workflow.StepWire) (bool, error) {
	// Find the loop decision's configuration to get loop_branch.
	var stepDef *workflow.StepWire
	for _, s := range allSteps {
		if s.ID == sr.StepID {
			stepDef = &s
			break
		}
	}
	if stepDef == nil {
		return false, fmt.Errorf("step %s not found in version", sr.StepID)
	}
	cfg := parseLoopDecisionConfig(stepDef.Config)
	if cfg.LoopBranch == "" {
		return false, nil
	}

	chain := chainStepIDs(allSteps, cfg.LoopBranch, sr.StepID)
	if len(chain) == 0 {
		return false, nil
	}

	// Load the latest run for each chain step and check if it is terminal.
	for _, cid := range chain {
		srList, err := listStepRunsByStepID(ctx, tx, tenantID, sr.WorkflowRunID, cid)
		if err != nil {
			return false, fmt.Errorf("list chain %s: %w", cid, err)
		}
		// Find the active (non-superseded) run.
			var active db.WorkflowStepRunRow
		for _, s := range srList {
			if s.SupersededBy == "" {
				active = s
				break
			}
		}
		if active.ID == "" || active.Status != domain.StepRunSucceeded {
			return false, nil
		}
	}
	return true, nil
}

func nowPtr(t time.Time) *time.Time { return &t }

// (nowPtr currently unused after refactor; retained for step-run
// timestamp fields as the reconciler grows.)
var _ = nowPtr
