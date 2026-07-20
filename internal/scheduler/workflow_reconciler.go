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
	"bytes"
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
}

// NewWorkflowReconciler creates a WorkflowReconciler. The policy
// evaluator evaluates gate_policy_ref before a ready step runs (Phase 7,
// docs/02 §2.5 Tier 1). May be nil (pass-through allow — v0.1 dev).
// The taskDispatcher is called after the workflow transaction commits
// to dispatch ready work items immediately (not waiting for the next
// TaskReconciler heartbeat). May be nil (fall back to heartbeat).
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger, pe PolicyEvaluator, td TaskDispatcher) *WorkflowReconciler {
	return &WorkflowReconciler{pool: pool, log: log, policy: pe, taskDispatcher: td}
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

	stepRuns, err := db.ListWorkflowStepRuns(ctx, ttx.Tx, tenantID, runID)
	if err != nil {
		return fmt.Errorf("list step runs: %w", err)
	}
	r.log.Debug("DEBUG: step runs loaded", "count", len(stepRuns))
	for _, sr := range stepRuns {
		r.log.Debug("DEBUG: step run detail", "id", sr.ID, "stepID", sr.StepID, "stepKind", sr.StepKind, "status", sr.Status, "version", sr.Version)
	}
	runByID := make(map[string]db.WorkflowStepRunRow, len(stepRuns))
	for _, sr := range stepRuns {
		runByID[sr.StepID] = sr
	}
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

		// Progress pending steps whose deps are satisfied → ready.
		// Use runByID (which reflects in-pass updates) for both the
		// pending check AND the dependency check so steps progressed
		// by Phase 2/3 in a prior outer iteration are not re-processed
		// with a stale version (fix: "db: not found" on re-update).
		for _, sr := range stepRuns {
			if cur, ok := runByID[sr.StepID]; ok && cur.Status != domain.StepRunPending {
				r.log.Debug("DEBUG: skipping step run (not pending in runByID)", "stepID", sr.StepID, "id", sr.ID, "status", cur.Status)
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

		// Dispatch ready steps by kind, evaluating gates first.
		for _, sr := range stepRuns {
			if sr.Status != domain.StepRunReady {
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
			if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindTask {
				continue
			}
			terminal, failed, err := r.pollTaskStep(ctx, ttx.Tx, tenantID, run, sr)
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
			}
		}

		// Poll running RECOVER steps: check if their linked recovery
		// execution has completed (terminal).
		for i, sr := range stepRuns {
			if sr.Status != domain.StepRunRunning || sr.StepKind != domain.StepKindRecover {
				continue
			}
			terminal, failed, err := r.pollRecoverStep(ctx, ttx.Tx, tenantID, run, sr)
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
					return fmt.Errorf("mark recover step terminal: %w", err)
				}
				stepRuns[i] = updated
				runByID[sr.StepID] = updated
				madeProgress = true
				evt := domain.WorkflowEventStepSucceeded
				if failed {
					evt = domain.WorkflowEventStepFailed
				}
				if err := r.enqueueStepEvent(ctx, ttx.Tx, evt, run, updated); err != nil {
					return fmt.Errorf("enqueue recover step result: %w", err)
				}
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
	allSucceeded := true
	anyFailed := false
	hasSteps := false
	var toSkip []string
	for _, sr := range stepRuns {
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
// terminal-success state (succeeded or skipped). For RECOVER steps,
// failed deps are also acceptable — they trigger the recovery wait path.
func (r *WorkflowReconciler) depsSatisfied(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	isRecover := step.Kind == domain.StepKindRecover
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
		if isRecover && sr.Status == domain.StepRunFailed {
			r.log.Debug("DEBUG: depsSatisfied recover dep satisfied (failed)", "step", step.ID, "dep", dep)
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
		// Worker node. Look upstream for WORK_ITEM steps in
		// step.DependsOn; the first one with a work_item_id is the
		// input to dispatch. (Multi-input fan-in: dispatch all in
		// series, track the last one on the step run — the previous
		// ones still complete on their own assigned worker.)
		upstream := upstreamWorkItemIDs(step, allSteps)
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
			composite, err := r.buildCompositePrompt(ctx, tx, tenantID, wi, allSteps, runs)
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

	case domain.StepKindRecover:
		// Check whether any dependency failed. If all deps succeeded,
		// skip recovery — the task completed normally.
		depFailed := false
		for _, dep := range step.DependsOn {
			if s, ok := runs[dep]; ok && s.Status == domain.StepRunFailed {
				depFailed = true
				break
			}
		}
		if !depFailed {
			// All deps succeeded → skip recovery.
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:    strPtr(domain.StepRunSucceeded),
				StartedAt: &now,
				EndedAt:   &now,
			})
			if err != nil {
				return fmt.Errorf("mark recover step succeeded (no-op): %w", err)
			}
			runs[step.ID] = updated
			r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated)
			break
		}
		// A dep failed — transition to running and wait for the recovery
		// engine to complete. We find the failed dep's work item id from
		// its result JSON (_work_item_id), then poll the recovery
		// execution status.
		var depResult struct {
			WorkItemID string `json:"_work_item_id"`
		}
		for _, dep := range step.DependsOn {
			if s, ok := runs[dep]; ok && s.Status == domain.StepRunFailed {
				if err := json.Unmarshal(s.Result, &depResult); err == nil && depResult.WorkItemID != "" {
					break
				}
			}
		}
		if depResult.WorkItemID == "" {
			// No work item found — the failed dep was never dispatched
			// (e.g. failStep due to missing config). There's nothing to
			// recover from; mark the step as failed and move on.
			r.log.Warn("recover step: failed dep has no work item id — nothing to recover", "run", run.ID, "step", step.ID)
			now := time.Now().UTC()
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunFailed),
				EndedAt: &now,
			})
			if err != nil {
				return fmt.Errorf("mark recover step failed (no work item): %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
				return fmt.Errorf("enqueue recover step_failed: %w", err)
			}
			break
		}
		recovery, err := db.GetLatestRecoveryForTask(ctx, tx, tenantID, depResult.WorkItemID)
		if err == db.ErrNotFound {
			// Recovery not yet created (TaskReconciler may not have
			// processed the failure). Leave ready and retry next pass.
			r.log.Info("recover step waiting for recovery execution", "run", run.ID, "step", step.ID, "work_item", depResult.WorkItemID)
			break
		}
		if err != nil {
			return fmt.Errorf("get latest recovery for task %s: %w", depResult.WorkItemID, err)
		}
		// Terminal recovery states: resumed (success), failed, cancelled,
		// escalated. Non-terminal: pending, running, blocked.
		switch recovery.Status {
		case domain.RecoveryResumed:
			// Recovery completed successfully.
			strategy := readConfigStrategy(step.Config)
			if strategy != "" {
				r.log.Info("workflow recover step completed", "run", run.ID, "step", step.ID, "strategy", strategy, "recovery", recovery.ID)
			}
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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
		case domain.RecoveryFailed, domain.RecoveryCancelled, domain.RecoveryEscalated:
			// Recovery terminated without success. The RECOVER step
			// still succeeds to let the DAG continue; downstream steps
			// see the failed work item and can decide how to react.
			r.log.Warn("workflow recover step: recovery did not resume", "run", run.ID, "step", step.ID, "recovery", recovery.ID, "recovery_status", recovery.Status)
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:    strPtr(domain.StepRunSucceeded),
				StartedAt: &now,
				EndedAt:   &now,
			})
			if err != nil {
				return fmt.Errorf("mark recover step succeeded (recovery %s): %w", recovery.Status, err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, updated); err != nil {
				return fmt.Errorf("enqueue recover step_succeeded: %w", err)
			}
		default:
			// Recovery still in progress — stay in running, retry next pass.
			// On first encounter, write the step-level recovery config
			// (max_retries, retry_delay_seconds) into the recovery execution's
			// DB fields so the engine observes them.
			if recovery.Status == domain.RecoveryPending && recovery.MaxRetries == 5 {
				_, mr, rd := readRecoveryConfig(step.Config)
				if _, err := db.UpdateRecoveryExecution(ctx, tx, tenantID, recovery.ID, recovery.Version, db.UpdateRecoveryExecutionFields{
					MaxRetries:        &mr,
					RetryDelaySeconds: &rd,
				}); err != nil {
					r.log.Warn("recover step: update recovery config", "recovery", recovery.ID, "error", err)
				}
			}
			if sr.Status != domain.StepRunRunning {
				updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:    strPtr(domain.StepRunRunning),
					StartedAt: &now,
				})
				if err != nil {
					return fmt.Errorf("mark recover step running: %w", err)
				}
				runs[step.ID] = updated
				if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
					return fmt.Errorf("enqueue recover step_started: %w", err)
				}
			}
			r.log.Info("recover step waiting for recovery", "run", run.ID, "step", step.ID, "recovery", recovery.ID, "recovery_status", recovery.Status)
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
//   1. # Task — the work item itself: title, description, acceptance
//      criteria. This is THE task; everything else is context.
//   2. # Project context — the ancestor chain walked via
//      work_items.parent_id (oldest first).
//   3. # Workflow context — a chronological timeline of every step in
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
//   4. # Recovery context (this task) — if THIS work item was
//      recovered from a previous execution failure, the recovery
//      summary is included here verbatim (recovery engine writes it
//      to the work item's results). Distinct from the per-step
//      recovery timeline above: this is the recovery for the task
//      the worker is about to execute, not for prior steps.
//   5. # File context — selected project files (PR: project context
//      files).
//   6. # Instructions — the worker's contract: emit the
//      ORCHICON WORKER SUMMARY marker at the end of the response so
//      the next stage can read it as upstream context.
//
// The composite is the opencode adapter's "message" (passed via the
// manifest Goal). The worker is instructed via the prompt's footer to
// end its response with `ORCHICON WORKER SUMMARY:` followed by a short
// summary that becomes the next stage's upstream context.
func (r *WorkflowReconciler) buildCompositePrompt(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, error) {
	var sb strings.Builder
	// 1. Task.
	sb.WriteString("# Task\n\n")
	fmt.Fprintf(&sb, "Title: %s\n\n", strings.TrimSpace(wi.Title))
	if d := strings.TrimSpace(wi.Description); d != "" {
		fmt.Fprintf(&sb, "Description:\n%s\n\n", d)
	}
	if ac := strings.TrimSpace(wi.AcceptanceCriteria); ac != "" {
		fmt.Fprintf(&sb, "Acceptance criteria:\n%s\n\n", ac)
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
	// 4. Recovery context (this task) — this work item may have been
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
	// 5. File context — selected project files (PR: project context files).
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

	case domain.StepKindRecover:
		// Look up the recovery execution that this step was waiting
		// on. The step's result JSON carries the work item id of the
		// failed dep (the one being recovered). We then read the
		// latest recovery for that work item.
		var ref struct {
			WorkItemID string `json:"_work_item_id"`
		}
		if err := json.Unmarshal(sr.Result, &ref); err != nil || ref.WorkItemID == "" {
			break
		}
		rec, err := db.GetLatestRecoveryForTask(ctx, tx, tenantID, ref.WorkItemID)
		if err != nil {
			if err == db.ErrNotFound {
				r.log.Debug("upstream step: recovery missing", "step", s.ID, "work_item_id", ref.WorkItemID)
				break
			}
			return fmt.Errorf("load recovery for upstream step %s: %w", s.ID, err)
		}
		if rec.Strategy != "" {
			fmt.Fprintf(sb, "Recovery strategy: %s\n", rec.Strategy)
		}
		if rec.TriggerReason != "" {
			fmt.Fprintf(sb, "Trigger reason: %s\n", rec.TriggerReason)
		}
		if rec.Summary != "" {
			fmt.Fprintf(sb, "\nRecovery narrative:\n%s\n", rec.Summary)
		} else if rec.Status != "" {
			fmt.Fprintf(sb, "\nRecovery status: %s (no narrative recorded).\n", rec.Status)
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

const (
	maxFileReadSize    = 50 * 1024  // 50KB per file content
	maxTotalReadSize   = 512 * 1024 // 512KB total across all files
	maxWalkDepth       = 32         // max directory recursion depth
)

// readProjectContextFiles reads the project's context_files from disk
// and embeds their contents directly into the prompt as markdown sections.
// Directories are walked recursively (up to maxWalkDepth), and each file's
// content is included up to maxFileReadSize per file. Binary files are
// detected by content-type sniffing and noted as binary rather than read.
func (r *WorkflowReconciler) readProjectContextFiles(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (string, error) {
	p, err := db.GetProject(ctx, tx, tenantID, projectID)
	if err != nil {
		return "", fmt.Errorf("get project for context files: %w", err)
	}
	if p.ProjectDir == "" || len(p.ContextFiles) == 0 {
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
	sb.WriteString("The following files are provided as project context. Their contents are inlined below.\n\n")

	var totalRead int
	for _, relPath := range files {
		if totalRead >= maxTotalReadSize {
			sb.WriteString("\n*(remaining files omitted — total context size limit reached)*\n")
			break
		}
		fullPath := filepath.Join(p.ProjectDir, relPath)
		if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean(p.ProjectDir)) {
			continue
		}
		n, err := r.readFileOrDir(&sb, fullPath, relPath, 0, &totalRead)
		if err != nil {
			r.log.Warn("error reading context file", "path", fullPath, "error", err)
			fmt.Fprintf(&sb, "\n> ⚠ Error reading `%s`: %s\n\n", relPath, err)
		}
		_ = n
	}
	sb.WriteString("\n")
	return sb.String(), nil
}

// readFileOrDir walks a single context_files entry (file or directory),
// reading file contents and appending them to sb. Returns the number of
// bytes read. depth tracks recursion depth for directory walking.
func (r *WorkflowReconciler) readFileOrDir(sb *strings.Builder, absPath, relPath string, depth int, totalRead *int) (int, error) {
	if depth > maxWalkDepth {
		fmt.Fprintf(sb, "\n> ⚠ `%s`: directory too deep (max %d levels), skipping\n\n", relPath, maxWalkDepth)
		return 0, nil
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return 0, fmt.Errorf("stat: %w", err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(absPath)
		if err != nil {
			return 0, fmt.Errorf("read dir: %w", err)
		}
		var total int
		for _, entry := range entries {
			if *totalRead >= maxTotalReadSize {
				sb.WriteString("\n*(remaining files omitted — total context size limit reached)*\n")
				break
			}
			childRel := filepath.Join(relPath, entry.Name())
			childAbs := filepath.Join(absPath, entry.Name())
			n, err := r.readFileOrDir(sb, childAbs, childRel, depth+1, totalRead)
			if err != nil {
				r.log.Warn("error reading context file", "path", childAbs, "error", err)
				fmt.Fprintf(sb, "\n> ⚠ Error reading `%s`: %s\n\n", childRel, err)
			}
			total += n
		}
		return total, nil
	}
	// Regular file.
	if *totalRead >= maxTotalReadSize {
		return 0, nil
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 0, fmt.Errorf("read file: %w", err)
	}
	// Detect binary files by checking for a null byte in the first 8KB.
	sniff := data
	if len(sniff) > 8192 {
		sniff = sniff[:8192]
	}
	if bytes.IndexByte(sniff, 0) != -1 {
		fmt.Fprintf(sb, "### `%s`\n\n_Binary file (%d bytes) — content not inlined._\n\n", relPath, len(data))
		*totalRead += len(data)
		return len(data), nil
	}
	// Truncate if too large.
	content := data
	if len(content) > maxFileReadSize {
		content = content[:maxFileReadSize]
	}
	fmt.Fprintf(sb, "### `%s`\n\n```\n%s\n```\n\n", relPath, string(content))
	if len(data) > maxFileReadSize {
		fmt.Fprintf(sb, "_File truncated: %d of %d bytes shown._\n\n", maxFileReadSize, len(data))
	}
	*totalRead += len(data)
	return len(data), nil
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
func readConfigStrategy(config string) string {
	if config == "" {
		return ""
	}
	var parsed struct {
		Strategy string `json:"strategy"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return ""
	}
	return parsed.Strategy
}

// readRecoveryConfig extracts strategy, max_retries and
// retry_delay_seconds from the step config JSON. Returns defaults
// for any missing field.
func readRecoveryConfig(config string) (strategy string, maxRetries, retryDelay int) {
	strategy = "summarize_restart"
	maxRetries = 5
	retryDelay = 10
	if config == "" {
		return
	}
	var parsed struct {
		Strategy          string `json:"strategy"`
		MaxRetries        int    `json:"max_retries"`
		RetryDelaySeconds int    `json:"retry_delay_seconds"`
	}
	if err := json.Unmarshal([]byte(config), &parsed); err != nil {
		return
	}
	if parsed.Strategy != "" {
		strategy = parsed.Strategy
	}
	if parsed.MaxRetries > 0 {
		maxRetries = parsed.MaxRetries
	}
	if parsed.RetryDelaySeconds > 0 {
		retryDelay = parsed.RetryDelaySeconds
	}
	return
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

// pollRecoverStep checks whether the recovery execution linked to a
// running RECOVER step has completed. Returns (terminal, error). The
// work item id is read from the failed dep step run's result JSON
// (_work_item_id).
func (r *WorkflowReconciler) pollRecoverStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow) (terminal bool, failed bool, err error) {
	// Find the latest stepRuns for this run to look up the dep's result.
	stepRuns, err := db.ListWorkflowStepRuns(ctx, tx, tenantID, run.ID)
	if err != nil {
		return false, false, fmt.Errorf("list step runs for recover poll: %w", err)
	}
	var workItemID string
	for _, s := range stepRuns {
		if s.StepKind == domain.StepKindTask && s.Status == domain.StepRunFailed {
			var parsed struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if err := json.Unmarshal(s.Result, &parsed); err == nil && parsed.WorkItemID != "" {
				workItemID = parsed.WorkItemID
				break
			}
		}
	}
	if workItemID == "" {
		// No recoverable work item found — tell caller to mark step
		// as failed so polling stops. Happens when the failed dep was
		// never dispatched (no _work_item_id in its result).
		return true, true, nil
	}
	recovery, err := db.GetLatestRecoveryForTask(ctx, tx, tenantID, workItemID)
	if err == db.ErrNotFound {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("get latest recovery for task %s: %w", workItemID, err)
	}
	switch recovery.Status {
	case domain.RecoveryResumed, domain.RecoveryFailed, domain.RecoveryCancelled, domain.RecoveryEscalated:
		r.log.Info("recover step recovery terminal", "run", run.ID, "step", sr.StepID, "recovery", recovery.ID, "status", recovery.Status)
		return true, false, nil
	default:
		return false, false, nil
	}
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
