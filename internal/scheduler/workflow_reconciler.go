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
	pool   *db.Pool
	log    *slog.Logger
	policy PolicyEvaluator // Phase 7: Rego gate evaluation (docs/02 §2.5)
}

// NewWorkflowReconciler creates a WorkflowReconciler. The policy
// evaluator evaluates gate_policy_ref before a ready step runs (Phase 7,
// docs/02 §2.5 Tier 1). May be nil (pass-through allow — v0.1 dev).
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger, pe PolicyEvaluator) *WorkflowReconciler {
	return &WorkflowReconciler{pool: pool, log: log, policy: pe}
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
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
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
		if err := r.dispatchStep(ctx, ttx.Tx, tenantID, run, step, sr, runByID, steps); err != nil {
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
func (r *WorkflowReconciler) dispatchStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, allSteps []workflow.StepWire) error {
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
		// PR D: a RECOVER step on the canvas is a passive marker; the
		// runtime records the strategy (from config.strategy — set by
		// the palette tiles for stop / human_escalation / retry_n /
		// summarize_restart) so operators can see what would have run
		// had the upstream worker actually failed. The actual recovery
		// flow is driven by the opencode-adapter failure path (see
		// TaskReconciler.transitionWorkItemOnResult → propagation of
		// the failed task to RecoveryEngine.TriggerOnFailure). Mark
		// succeeded so the DAG continues.
		strategy := readConfigStrategy(step.Config)
		if strategy != "" {
			r.log.Info("workflow recover step recorded", "run", run.ID, "step", step.ID, "strategy", strategy)
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
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepFailed, run, updated); err != nil {
		return fmt.Errorf("enqueue step_failed: %w", err)
	}
	return reason
}

// buildCompositePrompt assembles the prompt text the worker should see
// when this work item is dispatched (PR B — context propagation). It
// has three sections:
//
//   1. # Task — the work item itself: title, description, acceptance
//      criteria. This is THE task; everything else is context.
//   2. # Project context — the ancestor chain walked via
//      work_items.parent_id (oldest first).
//   3. # Upstream context — summaries from prior TASK step runs in
//      this workflow. Each upstream step's results._summary (set by
//      the TaskReconciler via ORCHICON WORKER SUMMARY extraction) is
//      included verbatim, in upstream order.
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
	// 3. Upstream context — prior TASK step runs that have completed.
	upstream := upstreamStepSummaries(ctx, allSteps, runs)
	if len(upstream) > 0 {
		sb.WriteString("# Upstream context\n\n")
		sb.WriteString("Summaries from prior worker steps in this workflow. Each summary is the worker's final output for that stage.\n\n")
		for i, s := range upstream {
			fmt.Fprintf(&sb, "## Stage %d\n%s\n\n", i+1, s)
		}
	}
	// Footer: instruction for the worker to emit the summary marker.
	sb.WriteString("# Instructions\n\n")
	sb.WriteString("Complete the task above. When you have finished, end your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one short paragraph summarizing what you did. Everything from that marker to the end of your output is passed to the next stage of the workflow as upstream context.\n")
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

// upstreamStepSummaries collects the `_summary` field from each prior
// TASK step run that is succeeded. Order: topological — the function
// walks the DAG in step-id order via allSteps (which is in the order
// the author placed them; for a linear chain that's left-to-right).
// Cycles are the caller's responsibility to prevent (validated at save
// time, docs/10 §11).
func upstreamStepSummaries(ctx context.Context, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) []string {
	var out []string
	for _, s := range allSteps {
		if s.Kind != domain.StepKindTask {
			continue
		}
		sr, ok := runs[s.ID]
		if !ok {
			continue
		}
		if sr.Status != domain.StepRunSucceeded {
			continue
		}
		if len(sr.Result) == 0 {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(sr.Result, &parsed); err != nil {
			continue
		}
		s, _ := parsed["_summary"].(string)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
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
