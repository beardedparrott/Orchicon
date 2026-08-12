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
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/beardedparrott/orchicon/internal/contextfiles"
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
	runtime        RuntimeLifecycle // per-workflow runtime container lifecycle (may be nil)
	// sequenceNotifier is called after a bound work item reaches a terminal
	// state AND it has a parent: the sequence engine advances the parent's
	// chain immediately instead of waiting for its next scan tick
	// (architecture-notes/sequential-multi-workflow-runs.md §2.2).
	// Optional — the scan pass is the safety net.
	sequenceNotifier func(ctx context.Context, parentID string)

	// warming tracks workflow runs whose runtime-serve readiness probe is
	// in flight (the async ensure-serving pass). Guards against spawning a
	// duplicate probe goroutine per run; a plane restart clears it and the
	// next reconcile pass re-triggers the (idempotent) probe.
	warmingMu sync.Mutex
	warming   map[string]bool
}

// RuntimeLifecycle creates/reaps the per-workflow runtime container and
// proves its opencode serve usable before a run dispatches. Implemented by
// runtime.Lifecycle; declared here to keep the reconciler decoupled. A nil
// implementation disables runtime containers (headless `orchicon serve`).
type RuntimeLifecycle interface {
	EnsureForRun(ctx context.Context, run db.WorkflowRunRow) error
	// EnsureServing ensures the run's runtime container exists with its
	// opencode serve brought up, then blocks until the serve is PROVEN
	// usable (L1: health + a real session-create round-trip). The
	// reconciler must not dispatch an execution for the run until it
	// returns nil.
	EnsureServing(ctx context.Context, run db.WorkflowRunRow) error
	ReapForRun(ctx context.Context, runID string) error
}

// runtimeEnabled reports whether runtime-container lifecycle is wired.
// It guards BOTH a nil interface and an interface wrapping a typed-nil
// pointer (a `var lc *runtime.Lifecycle` passed as RuntimeLifecycle is
// non-nil to the interface — calling its methods then nil-dereferences
// the receiver and crashes the plane; see server.go wiring).
func (r *WorkflowReconciler) runtimeEnabled() bool {
	if r.runtime == nil {
		return false
	}
	rv := reflect.ValueOf(r.runtime)
	return rv.Kind() != reflect.Ptr || !rv.IsNil()
}

// NewWorkflowReconciler creates a WorkflowReconciler. The policy
// evaluator evaluates gate_policy_ref before a ready step runs (Phase 7,
// docs/02 §2.5 Tier 1). May be nil (pass-through allow — v0.1 dev).
// The taskDispatcher is called after the workflow transaction commits
// to dispatch ready work items immediately (not waiting for the next
// TaskReconciler heartbeat). May be nil (fall back to heartbeat).
// recovery is the RecoveryEngine used by explicit `recover` steps.
// runtime is the per-workflow runtime container lifecycle; nil disables
// runtime containers.
func NewWorkflowReconciler(pool *db.Pool, log *slog.Logger, pe PolicyEvaluator, td TaskDispatcher, rt RecoveryTrigger, rl RuntimeLifecycle) *WorkflowReconciler {
	return &WorkflowReconciler{pool: pool, log: log, policy: pe, taskDispatcher: td, recovery: rt, runtime: rl, warming: make(map[string]bool)}
}

// SetSequenceNotifier injects the callback that advances a sequence parent
// when one of its bound children reaches a terminal state. Optional.
func (r *WorkflowReconciler) SetSequenceNotifier(fn func(ctx context.Context, parentID string)) {
	r.sequenceNotifier = fn
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
	// Only progress non-terminal runs. Terminal runs (including aborted)
	// have their runtime container reaped here so a run terminalized
	// elsewhere (e.g. AbortRun) never leaks its container.
	if run.Status == domain.WorkflowRunCompleted || run.Status == domain.WorkflowRunFailed || run.Status == domain.WorkflowRunAborted {
		if r.runtime != nil {
			if err := r.runtime.ReapForRun(context.Background(), run.ID); err != nil {
				r.log.Warn("reap terminal run runtime failed", "run", run.ID, "error", err)
			}
		}
		return nil
	}

	// Reaping a terminal run's runtime container is deferred until after
	// the DB transaction commits: the container is not part of run state,
	// and a failed container operation must not roll back run progress.
	// (Container CREATION for a started run is now owned entirely by the
	// async ensure-serving pass — the run-start serve gate — so it is not
	// deferred here.)
	reapRuntime := false
	defer func() {
		if !r.runtimeEnabled() {
			return
		}
		bg := context.Background()
		if reapRuntime {
			if err := r.runtime.ReapForRun(bg, run.ID); err != nil {
				r.log.Warn("reap workflow runtime failed", "run", run.ID, "error", err)
			}
		}
	}()

	// Load the published version's steps to drive DAG progression.
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, run.WorkflowID, run.WorkflowVersion)
	if err != nil {
		if err == db.ErrNotFound {
			// The workflow version the run references is gone (workflow
			// deleted, or a raw-seeded run never had one). The run can
			// never progress — fail it so it terminalizes instead of
			// sitting "running" forever and leaking its runtime container.
			if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
				fmt.Sprintf("workflow version v%d not found", run.WorkflowVersion)); ferr != nil {
				return ferr
			}
			// Commit the failure — the deferred rollback would otherwise
			// undo it on the early return.
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit fail-at-start: %w", cerr)
			}
			reapRuntime = true
			return nil
		}
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
	// A run whose published version has an EMPTY step DAG can never
	// progress: the terminal-state check requires hasSteps && allSucceeded,
	// so a zero-step run would sit "running" forever and leak its runtime
	// container (the 30s adopt sweep treats any running run as active and
	// keeps its container alive). As a sequence child it would also park its
	// parent's chain indefinitely. Fail it at start — same structural-config
	// treatment as an unresolvable runtime image.
	if len(steps) == 0 {
		if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
			"workflow has no steps (empty step DAG)"); ferr != nil {
			return ferr
		}
		// Commit the failure — the deferred rollback would otherwise undo
		// it on the early return (the run would stay "running" forever).
		if cerr := ttx.Commit(ctx); cerr != nil {
			return fmt.Errorf("commit fail-at-start: %w", cerr)
		}
		reapRuntime = true
		return nil
	}

	// Transition pending → running (docs/02 §2.4). Resolve the runtime
	// container image from the work item(s) the run will use:
	//   - template runs: the bound work item's runtime_image;
	//   - one-shot runs: the WORK_ITEM canvas markers' work items —
	//     all must agree (or be empty → base), conflicting images fail
	//     the run at start since one container can't serve two images.
	if run.Status == domain.WorkflowRunPending {
		resolved, rerr := r.resolveRuntimeImage(ctx, ttx.Tx, tenantID, run, steps)
		if rerr != nil {
			// Fail the run at start: an unresolvable image is a config
			// error, not a recoverable execution failure.
			if ferr := r.failRunAtStart(ctx, ttx.Tx, tenantID, run,
				fmt.Sprintf("runtime image: %v", rerr)); ferr != nil {
				return ferr
			}
			// Commit the failure — the deferred rollback would otherwise
			// undo it on the early return (the run would stay "pending"
			// forever, re-attempting the image resolve every pass).
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit fail-at-start: %w", cerr)
			}
			reapRuntime = true
			return nil
		}
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:       strPtr(domain.WorkflowRunRunning),
			RuntimeImage: &resolved,
			// The runtime-serve readiness gate: false when a runtime daemon
			// is wired (the async ensure-serving pass proves the serve and
			// flips it true), true for headless serve (no container — the
			// host serve is always-on).
			RuntimeReady: boolPtr(!r.runtimeEnabled()),
		})
		if err != nil {
			return fmt.Errorf("transition run to running: %w", err)
		}
		run = updated
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunStarted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_started: %w", err)
		}
	}

	// Runtime-serve readiness gate: no execution may start for a run until
	// its runtime container's opencode serve is PROVEN usable. The async
	// ensure-serving pass (startEnsureServing) flips runtime_ready=true;
	// while it is false, DAG progression is HELD so neither the inline
	// DispatchTask nor the TaskReconciler scan can create an execution.
	// This converts the old dispatch-time race (a cold-starting serve
	// failing the first execution's 30s window) into a deterministic
	// run-start check.
	if run.Status == domain.WorkflowRunRunning && !run.RuntimeReady {
		if r.runtimeEnabled() {
			r.startEnsureServing(run)
			// Commit the transition (the deferred rollback would undo it on
			// the early return) and hold progression until the probe flips
			// the gate.
			if cerr := ttx.Commit(ctx); cerr != nil {
				return fmt.Errorf("commit run start (warming runtime): %w", cerr)
			}
			return nil
		}
		// No runtime configured (headless): the gate is moot — flip it so
		// progression proceeds immediately. Covers runs created before the
		// runtime_ready migration (default false).
		if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			RuntimeReady: boolPtr(true),
		}); err != nil {
			return fmt.Errorf("clear runtime gate (headless): %w", err)
		}
		run.RuntimeReady = true
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

	// Collect steps dispatched in this pass for inline TaskReconciler
	// dispatch after the transaction commits. Each dispatch is scoped to a
	// (task, step run) pair — the work item is a shared input reference, so
	// parallel steps bound to it each get their own execution.
	var dispatchedSteps []dispatchReq

	// Recovery triggers collected during this pass are invoked AFTER the
	// transaction commits. TriggerOnFailure opens its OWN transaction on a
	// separate connection, so calling it synchronously from inside this
	// pass while the pass holds row locks on the affected work item (e.g.
	// dispatchStep just re-dispatched it) makes the child transaction
	// block on this pass's locks — and this pass then blocks waiting for
	// the child to return: a cross-connection self-deadlock Postgres
	// cannot detect, wedging the run forever. Deferring the trigger to
	// post-commit (like inline dispatch) removes the cycle.
	var recoveryTriggers []recoveryTriggerReq

	// DAG progression loop: repeat pending→ready, dispatch, and poll
	// until no step makes progress in a full pass. This ensures that
	// when a task step is polled terminal, downstream pending steps
	// whose deps just became satisfied are progressed and dispatched
	// in the SAME scan pass — no need to wait for the next heartbeat
	// (docs/03 §2, docs/02 §2.4).
	//
	// The pass is bounded to maxDAGPasses iterations. The loop breaks
	// on !madeProgress, but a pathological run (e.g. a step whose
	// status flips every iteration) must never pin a goroutine in a
	// busy loop — a wedged reconcile goroutine is what froze the whole
	// reconciler in the field (150% CPU, advisory lock never renewed).
	// The bound converts any such pathology into a single errored pass;
	// the run is then retried by the manager queue instead of looping
	// forever.
	progressed := false
	passes := 0
	for {
		if passes >= maxDAGPasses {
			r.log.Warn("workflow DAG pass limit reached — aborting pass",
				"run", runID, "passes", passes)
			return fmt.Errorf("workflow run %s: DAG pass limit (%d) exceeded — possible stuck run", runID, maxDAGPasses)
		}
		passes++
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
			// A SUPERSEDED run must never shadow the active run for its
			// step_id: loop_decision iterations are created in the same
			// transaction as the superseding update, so several rows can
			// share a created_at. With ORDER BY created_at ASC (even with
			// an id tiebreaker), "last row wins" is only safe if the
			// active row is also the last — prefer a non-superseded row
			// whenever one exists so a stale entry can't wedge the DAG
			// (the observed failure: runByID held a superseded succeeded
			// loop-decision run, the active pending iteration was never
			// dispatched, and the run sat "running" until force-progress).
			cur, exists := runByID[sr.StepID]
			if sr.SupersededBy != "" {
				// Superseded row: only use it if nothing exists yet; the
				// active row (when it appears) overwrites it.
				if exists {
					continue
				}
			} else if exists && cur.SupersededBy == "" {
				// Active row already won — never overwrite it.
				continue
			}
			runByID[sr.StepID] = sr
		}

		// Phase 0: Handle rejected approval steps. The decision comes
		// from the STEP RUN — the approval record. Human reviews write
		// _decision ("approved"/"rejected") via the ApproveStep RPC;
		// worker-backed approvals propagate the approver's ORCHICON
		// WORKER SUMMARY decision ("success"/"failure") onto the step
		// run when the execution completes. The work item is only
		// consulted for legacy step runs that carry no decision.
		// Map: success/approved → forward, failure/rejected → loop back.
		for _, sr := range runByID {
			if sr.SupersededBy != "" || sr.StepKind != domain.StepKindApproval {
				continue
			}
			if sr.Status != domain.StepRunSucceeded {
				continue
			}

			// Check step run result for _decision (human approval).
			var srResult struct {
				Decision    string `json:"_decision"`
				WorkItemID  string `json:"_work_item_id"`
			}
			json.Unmarshal(sr.Result, &srResult)

			// The step run is the approval record: its _decision (written
			// by the ApproveStep RPC for human reviews, or propagated from
			// the approver execution's ORCHICON WORKER SUMMARY for
			// worker-backed approvals) is authoritative. Fall back to the
			// work item's results ONLY when the step run carries no
			// decision at all (legacy rows) — a stale _decision left on a
			// shared ticket by a prior run/step must never override the
			// current step run's real decision.
			decision := srResult.Decision
			if decision == "" && srResult.WorkItemID != "" {
				if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, srResult.WorkItemID); err == nil {
					decision = approvalDecisionFromSources("", wi.Results)
				}
			}

			// Map: success/approved → forward; failure/rejected → loop.
			if decision != "failure" && decision != "rejected" {
				continue
			}

			step, ok := stepByID[sr.StepID]
			if !ok {
				continue
			}
			cfg := parseApprovalConfig(step.Config)
			if cfg.LoopBranch == "" || cfg.MaxIterations < 1 {
				continue
			}
			currentIter := currentLoopIteration(runByID, cfg.LoopBranch)
			if currentIter >= cfg.MaxIterations {
				// Mark the step failed and CONTINUE the pass so the
				// transaction commits (the completion check then fails the
				// run). A bare `return failStep(...)` returns before the
				// commit, the deferred rollback undoes the failed mark, and
				// every subsequent pass re-fails the same step forever.
				if err := r.failStep(ctx, ttx.Tx, tenantID, run, sr, runByID,
					fmt.Errorf("approval step %q: max_iterations (%d) exhausted (rejected)", step.Name, cfg.MaxIterations)); err != nil {
					return err
				}
				madeProgress = true
				continue
			}
			if err := r.approvalReenter(ctx, ttx.Tx, tenantID, run, sr, step, runByID, cfg.LoopBranch, currentIter, time.Now().UTC(), steps); err != nil {
				return err
			}
			madeProgress = true
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
			// For recovering steps, skip while their recovery is still in
			// flight. Recovery is scoped per failing step run (the work
			// item is a shared input reference and never flips to
			// "recovering"), so the gate consults the recovery rows: an
			// active recovery for the failed execution this step run is
			// waiting on → wait; none → re-dispatch. Applies to TASK and
			// worker-backed APPROVAL steps alike (an approval step using
			// the summarize_restart strategy must wait for its recovery
			// before the approver is re-dispatched).
			if sr.Status == domain.StepRunRecovering && (sr.StepKind == domain.StepKindTask || sr.StepKind == domain.StepKindApproval) {
				var parsed struct {
					WorkItemID string `json:"_work_item_id"`
				}
				if err := json.Unmarshal(sr.Result, &parsed); err == nil && parsed.WorkItemID != "" && sr.WorkerExecutionID != "" {
					if _, err := db.GetActiveRecoveryForExecution(ctx, ttx.Tx, tenantID, parsed.WorkItemID, sr.WorkerExecutionID); err == nil {
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
			var stepDispatches []dispatchReq
			if err := r.dispatchStep(ctx, ttx.Tx, tenantID, run, step, sr, runByID, steps, &stepDispatches, &recoveryTriggers); err != nil {
				return err
			}
			madeProgress = true
			dispatchedSteps = append(dispatchedSteps, stepDispatches...)
		}

		// Poll running task steps + worker-backed approval steps: check
		// their linked WorkItem status.
		for i, sr := range stepRuns {
			if sr.SupersededBy != "" {
				continue
			}
			if sr.Status != domain.StepRunRunning {
				continue
			}
			if sr.StepKind != domain.StepKindTask && sr.StepKind != domain.StepKindApproval {
				continue
			}
			stepCfg := ""
			if s, ok := stepByID[sr.StepID]; ok {
				stepCfg = s.Config
			}
			terminal, failed, err := r.pollTaskStep(ctx, ttx.Tx, tenantID, run, sr, stepCfg, runByID, &recoveryTriggers)
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
	// terminalParent, when set, is the parent of a bound work item that
	// reached a terminal state in this pass — the sequence engine is
	// notified after commit so the chain advances immediately.
	var terminalParent string
	// Collect every active non-terminal step run so that — once anyFailed is
	// known — ALL of them are skipped, not just the ones that happened to be
	// iterated after the first failed step (created_at order is not
	// necessarily the DAG order; a failing step created last would otherwise
	// leave the other steps "pending" on a failed run).
	var nonTerminal []db.WorkflowStepRunRow
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
			allSucceeded = false
		case domain.StepRunApprovalPending:
			allSucceeded = false
		case domain.StepRunRecovering:
			// Recovering is not terminal — the step is being retried.
			// Keep the run running (don't set anyFailed).
			allSucceeded = false
		default:
			allSucceeded = false
			nonTerminal = append(nonTerminal, sr)
		}
	}
	// If the run has failed, skip all remaining non-terminal steps so
	// the UI accurately reflects the run state instead of showing them
	// as "pending" forever.
	if anyFailed {
		for _, cur := range nonTerminal {
			now2 := time.Now().UTC()
			updated, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, tenantID, cur.ID, cur.Version, db.UpdateWorkflowStepRunFields{
				Status:  strPtr(domain.StepRunSkipped),
				EndedAt: &now2,
			})
			if err != nil {
				return fmt.Errorf("skip pending step on failed run: %w", err)
			}
			runByID[cur.StepID] = updated
		}
	}
	if hasSteps && allSucceeded {
		now := time.Now().UTC()
		// The bound work item is now complete: it stayed "running" for
		// the whole run (each step's execution transitions it to running,
		// not succeeded — see TaskReconciler.boundToActiveRun) and only
		// reaches "succeeded" when every step of the run has succeeded.
		if run.WorkItemID != "" {
			if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); err == nil {
				// Recurring items stay "recurring" after a successful run:
				// the next_run_at was pre-computed by RecurringFireReconciler
				// and the item should be re-scanned on the next occurrence.
				// Non-recurring items transition to "succeeded".
				status := domain.WorkItemSucceeded
				if wi.NextRunAt != nil && wi.Status == domain.WorkItemRecurring {
					status = domain.WorkItemRecurring
				}
				fields := db.UpdateWorkItemFields{Status: &status}
				if narrative, err := r.buildRunNarrative(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunCompleted); err != nil {
					r.log.Warn("build run narrative", "run", runID, "error", err)
				} else if narrative != nil {
					fields.Results = narrative
				}
				review := r.buildAcceptanceReview(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunCompleted)
				fields.AcceptanceReview = &review
				if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID, wi.Version, fields); err != nil {
					return fmt.Errorf("mark bound work item succeeded: %w", err)
				}
				// Sequence wiring: a bound child reaching terminal-success
				// must advance its parent's chain. Recurring items that
				// stay recurring do NOT trigger sequence advance — they
				// are standalone recurring tickets, not sequence children.
				if wi.ParentID != nil && status == domain.WorkItemSucceeded {
					terminalParent = *wi.ParentID
				}
			}
		}
		updated, err := db.UpdateWorkflowRun(ctx, ttx.Tx, tenantID, runID, run.Version, db.UpdateWorkflowRunFields{
			Status:  strPtr(domain.WorkflowRunCompleted),
			EndedAt: &now,
		})
		if err != nil {
			return fmt.Errorf("mark run completed: %w", err)
		}
		run = updated
		progressed = true
		reapRuntime = true
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunCompleted, run, ""); err != nil {
			return fmt.Errorf("enqueue run_completed: %w", err)
		}
	} else if anyFailed {
		now := time.Now().UTC()
		// Update the linked work item to failed, carrying the run-level
		// narrative.
		if run.WorkItemID != "" {
			if wi, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID); err == nil {
				status := domain.WorkItemFailed
				fields := db.UpdateWorkItemFields{Status: &status}
				// Recurring items: clear next_run_at on failure so the
				// schedule stops. The user must re-arm it manually or
				// the RecurringFireReconciler won't pick it up again.
				if wi.NextRunAt != nil && wi.Status == domain.WorkItemRecurring {
					clearRecurring := true
					fields.ClearRecurringSchedule = clearRecurring
				}
				if narrative, err := r.buildRunNarrative(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunFailed); err != nil {
					r.log.Warn("build run narrative", "run", runID, "error", err)
				} else if narrative != nil {
					fields.Results = narrative
				}
				review := r.buildAcceptanceReview(ctx, ttx.Tx, tenantID, run, stepRuns, domain.WorkflowRunFailed)
				fields.AcceptanceReview = &review
				_, _ = db.UpdateWorkItem(ctx, ttx.Tx, tenantID, run.WorkItemID, wi.Version, fields)
				// Sequence wiring: a bound child failing halts its
				// parent's chain.
				if wi.ParentID != nil {
					terminalParent = *wi.ParentID
				}
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
		reapRuntime = true
		if err := r.enqueueRunEvent(ctx, ttx.Tx, domain.WorkflowEventRunFailed, run, ""); err != nil {
			return fmt.Errorf("enqueue run_failed: %w", err)
		}
	}

	if err := ttx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	// Sequence advance: a bound child reached a terminal state and has a
	// parent — notify the sequence engine so the parent's chain advances
	// (or halts) immediately rather than on the next scan tick. Post-commit
	// so the child's terminal status is visible. The notifier fires for the
	// parent of ANY terminal bound work item; reconcileParent re-applies
	// the sequence-parent guard (status running/failed + no bound workflow
	// run + has children), so a non-sequence parent is a no-op there.
	if terminalParent != "" && r.sequenceNotifier != nil {
		r.sequenceNotifier(context.Background(), terminalParent)
	}
	// Recovery triggers run AFTER the transaction commits: TriggerOnFailure
	// opens its own transaction on a separate connection, and calling it
	// while this pass still holds row locks on the affected work item (the
	// dispatch section may have just re-dispatched it) would deadlock
	// (child tx waits on this pass's lock; this pass waits on the child's
	// return). Post-commit the locks are gone.
	if r.recovery != nil {
		for _, tr := range recoveryTriggers {
			if err := r.recovery.TriggerOnFailure(context.Background(), tr.tenantID, tr.workItemID, tr.failedExecID, tr.stepRunID, tr.reason); err != nil {
				r.log.Warn("post-commit recovery trigger failed",
					"run", runID, "work_item", tr.workItemID, "error", err)
			}
		}
	}
	// Inline dispatch: hand dispatched steps to the TaskReconciler
	// immediately so executions appear in the UI without waiting for the
	// next TaskReconciler heartbeat (~1s). The dispatch happens after the
	// workflow transaction commits so the step run + prompt are visible
	// to the TaskReconciler's own transaction (docs/03 §8 invariant #1:
	// only the TaskReconciler creates WorkerExecutions). Dispatch is scoped
	// per step run — the work item is a shared input reference, so parallel
	// steps bound to it each get their own execution.
	if r.taskDispatcher != nil {
		for _, d := range dispatchedSteps {
			if err := r.taskDispatcher.DispatchTask(context.Background(), d.taskID, d.stepRunID); err != nil {
				r.log.Warn("inline dispatch failed", "work_item", d.taskID, "step_run", d.stepRunID, "error", err)
			}
		}
	}
	if progressed {
		r.log.Info("workflow run progressed", "run", runID, "status", run.Status)
	}
	return nil
}

// failRunAtStart fails a workflow run before it can execute — a structural
// or configuration error (unresolvable runtime image, empty step DAG,
// missing workflow version). The run can never progress, and without this
// failure it would sit "running" forever, leaking its runtime container
// (the 30s adopt sweep treats any running run as active and keeps the
// container alive) and, for a sequence child, parking its parent's chain.
// The bound work item is marked failed so the sequence engine halts the
// chain. Callers set reapRuntime afterwards so the container (if any) is
// killed.
func (r *WorkflowReconciler) failRunAtStart(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, reason string) error {
	now := time.Now().UTC()
	if _, err := db.UpdateWorkflowRun(ctx, tx, tenantID, run.ID, run.Version, db.UpdateWorkflowRunFields{
		Status:  strPtr(domain.WorkflowRunFailed),
		EndedAt: &now,
	}); err != nil {
		return fmt.Errorf("fail run at start: %w", err)
	}
	// Mark the bound work item failed too.
	if run.WorkItemID != "" {
		if wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID); err == nil {
			status := domain.WorkItemFailed
			narrative, _ := json.Marshal(map[string]any{"_run_narrative": map[string]any{
				"run_id": run.ID, "status": domain.WorkflowRunFailed, "error": reason,
			}})
			review := fmt.Sprintf("## Acceptance Review\n\n**Run:** `%s` · **Status:** failed\n\nNo work was delivered — the run failed to start: %s", run.ID, reason)
			_, _ = db.UpdateWorkItem(ctx, tx, tenantID, run.WorkItemID, wi.Version, db.UpdateWorkItemFields{
				Status:           &status,
				Results:          &narrative,
				AcceptanceReview: &review,
			})
		}
	}
	_ = r.enqueueRunEvent(ctx, tx, domain.WorkflowEventRunFailed, run, reason)
	return nil
}

// startEnsureServing kicks off the ASYNC runtime-serve readiness probe for
// a run (idempotent — one goroutine per run; the in-flight map clears when
// it finishes). On success it flips the run's runtime_ready gate so the
// next reconcile pass progresses the DAG; on failure it fails the run at
// start with the serve error. A plane restart clears the map and the next
// reconcile pass re-triggers the (idempotent) probe.
func (r *WorkflowReconciler) startEnsureServing(run db.WorkflowRunRow) {
	r.warmingMu.Lock()
	if r.warming[run.ID] {
		r.warmingMu.Unlock()
		return
	}
	r.warming[run.ID] = true
	r.warmingMu.Unlock()

	go func() {
		defer func() {
			r.warmingMu.Lock()
			delete(r.warming, run.ID)
			r.warmingMu.Unlock()
		}()
		bg := context.Background()
		if err := r.runtime.EnsureServing(bg, run); err != nil {
			r.log.Error("workflow runtime serve failed to become usable — failing run", "run", run.ID, "error", err)
			r.failRunServeGate(bg, run, err)
			return
		}
		if err := r.markRuntimeReady(bg, run); err != nil {
			r.log.Warn("mark workflow runtime ready failed", "run", run.ID, "error", err)
		}
	}()
}

// markRuntimeReady flips the run's runtime_ready gate to true (its serve
// passed the L1 probe). No-op if the run terminalized while warming.
func (r *WorkflowReconciler) markRuntimeReady(ctx context.Context, run db.WorkflowRunRow) error {
	ttx, err := r.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID)
	if err != nil {
		return err
	}
	if cur.Status != domain.WorkflowRunRunning {
		return nil // run terminalized while warming — nothing to gate
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID, cur.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		return err
	}
	return ttx.Commit(ctx)
}

// failRunServeGate fails a run whose runtime serve could not be proven
// usable within the readiness window. Same fail-at-start treatment as the
// other structural run-start failures (unresolvable image, empty DAG) — a
// serve that cannot come up is a config/infra error, not a recoverable
// execution failure, and without this the gate would hold the run warming
// forever. The run's container (if any) is reaped.
func (r *WorkflowReconciler) failRunServeGate(ctx context.Context, run db.WorkflowRunRow, serveErr error) {
	reason := fmt.Sprintf("runtime opencode serve failed to become usable: %v", serveErr)
	ttx, err := r.pool.BeginTenantTx(ctx, run.TenantID)
	if err != nil {
		r.log.Error("fail run serve gate: begin tx", "run", run.ID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)
	cur, err := db.GetWorkflowRun(ctx, ttx.Tx, run.TenantID, run.ID)
	if err != nil {
		r.log.Error("fail run serve gate: get run", "run", run.ID, "error", err)
		return
	}
	if cur.Status != domain.WorkflowRunRunning {
		return // already terminal — nothing to fail
	}
	if err := r.failRunAtStart(ctx, ttx.Tx, run.TenantID, cur, reason); err != nil {
		r.log.Error("fail run serve gate", "run", run.ID, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("fail run serve gate: commit", "run", run.ID, "error", err)
		return
	}
	if r.runtime != nil {
		if err := r.runtime.ReapForRun(context.Background(), run.ID); err != nil {
			r.log.Warn("reap failed run runtime", "run", run.ID, "error", err)
		}
	}
}

// buildRunNarrative aggregates the run's step results + recovery episodes
// into the bound work item's results — the run-level narrative. The
// ticket is the Jira-style record of the whole run: step outputs live on
// the step runs, and this is the ticket's read-only summary of them. It
// returns the merged results JSON (or nil if the run has no bound item),
// leaving status/Results writes to the caller.
func (r *WorkflowReconciler) buildRunNarrative(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, finalStatus string) (*[]byte, error) {
	if run.WorkItemID == "" {
		return nil, nil
	}
	wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID)
	if err != nil {
		return nil, fmt.Errorf("get bound work item for narrative: %w", err)
	}
	var steps []map[string]any
	for _, sr := range stepRuns {
		var meta map[string]any
		_ = json.Unmarshal(sr.Result, &meta)
		entry := map[string]any{
			"step_id":   sr.StepID,
			"step_name": sr.StepName,
			"status":    sr.Status,
		}
		for _, k := range []string{"_summary", "_decision", "_issues", "_worker", "_worker_name", "_recovery_summary"} {
			if v, ok := meta[k]; ok {
				entry[strings.TrimPrefix(k, "_")] = v
			}
		}
		steps = append(steps, entry)
	}
	var recoveries []map[string]any
	if recs, err := db.ListRecoveries(ctx, tx, db.ListRecoveriesFilter{TenantID: tenantID, TaskID: run.WorkItemID}); err == nil {
		for _, rec := range recs {
			recoveries = append(recoveries, map[string]any{
				"recovery_id": rec.ID,
				"summary":     rec.Summary,
				"reason":      rec.TriggerReason,
				"status":      rec.Status,
			})
		}
	}
	merged := map[string]any{}
	if len(wi.Results) > 0 {
		_ = json.Unmarshal(wi.Results, &merged)
	}
	merged["_run_narrative"] = map[string]any{
		"run_id":     run.ID,
		"status":     finalStatus,
		"steps":      steps,
		"recoveries": recoveries,
	}
	mergedJSON, _ := json.Marshal(merged)
	return &mergedJSON, nil
}

// buildAcceptanceReview assembles the deterministic, human-readable
// acceptance review markdown for a terminal run — the faithful projection
// of the step data buildRunNarrative reads (the per-step worker _summary /
// _decision / _issues and any recovery episodes ARE the final work done).
//
// Rules (architecture note — add-acceptance-review-field-to-work-items):
//   - Deterministic: step runs are sorted by step_id; no LLM call, no
//     wall-clock-dependent ordering. A completed run lists "What was
//     delivered"; a failed run additionally lists "Not delivered /
//     needs attention" drawn from failed steps' _summary/_issues.
//   - Only steps with a non-empty _summary (or, on failure, _issues) are
//     listed — no empty noise. Skipped steps are omitted (a skip delivered
//     nothing). Superseded iterations are omitted (superseded_by != "" —
//     replaced by a later loop iteration).
//   - Recovery episodes (when any) are listed once under "Recovery",
//     deduplicated by recovery id.
//   - The document is capped at maxDescLen (1 MiB) so a pathological run
//     cannot bloat the column; the caller still validates via the API
//     boundary when a human edits it later.
func (r *WorkflowReconciler) buildAcceptanceReview(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, finalStatus string) string {
	var recoveries []db.RecoveryExecutionRow
	if recs, err := db.ListRecoveries(ctx, tx, db.ListRecoveriesFilter{TenantID: tenantID, TaskID: run.WorkItemID}); err == nil {
		recoveries = recs
	}
	return formatAcceptanceReview(run, stepRuns, recoveries, finalStatus)
}

// formatAcceptanceReview is the pure, deterministic formatter behind
// buildAcceptanceReview — split out so the aggregation rules are
// unit-testable without a database.
func formatAcceptanceReview(run db.WorkflowRunRow, stepRuns []db.WorkflowStepRunRow, recoveries []db.RecoveryExecutionRow, finalStatus string) string {
	sorted := append([]db.WorkflowStepRunRow(nil), stepRuns...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StepID != sorted[j].StepID {
			return sorted[i].StepID < sorted[j].StepID
		}
		return sorted[i].ID < sorted[j].ID
	})

	var b strings.Builder
	statusLabel := "completed"
	if finalStatus == domain.WorkflowRunFailed {
		statusLabel = "failed"
	}
	fmt.Fprintf(&b, "## Acceptance Review\n\n**Run:** `%s` · **Status:** %s\n", run.ID, statusLabel)
	if run.EndedAt != nil {
		fmt.Fprintf(&b, "**Completed at:** %s\n", run.EndedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")

	var delivered []string
	var notDelivered []string
	for _, sr := range sorted {
		if sr.SupersededBy != "" {
			// Replaced by a later loop iteration — not final work.
			continue
		}
		var meta map[string]any
		_ = json.Unmarshal(sr.Result, &meta)
		summary, _ := meta["_summary"].(string)
		issues, _ := meta["_issues"].(string)
		decision, _ := meta["_decision"].(string)
		switch sr.Status {
		case domain.StepRunSucceeded:
			summary = strings.TrimSpace(summary)
			if summary == "" {
				continue
			}
			label := sr.StepName
			if decision != "" {
				label += " — " + decision
			}
			delivered = append(delivered, fmt.Sprintf("- **%s** (succeeded): %s", label, summary))
		case domain.StepRunFailed, domain.StepRunBlocked:
			text := strings.TrimSpace(summary)
			if text == "" {
				text = strings.TrimSpace(issues)
			}
			if text == "" {
				text = "no summary recorded"
			}
			label := sr.StepName
			if decision != "" {
				label += " — " + decision
			}
			notDelivered = append(notDelivered, fmt.Sprintf("- **%s** (%s): %s", label, sr.Status, text))
		}
	}

	if len(delivered) > 0 {
		b.WriteString("### What was delivered\n\n")
		for _, d := range delivered {
			b.WriteString(d)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(notDelivered) > 0 {
		b.WriteString("### Not delivered / needs attention\n\n")
		for _, d := range notDelivered {
			b.WriteString(d)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(recoveries) > 0 {
		b.WriteString("### Recovery\n\n")
		for _, rec := range recoveries {
			b.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", rec.Status, rec.TriggerReason, strings.TrimSpace(rec.Summary)))
		}
		b.WriteString("\n")
	}

	if len(delivered) == 0 && len(notDelivered) == 0 && len(recoveries) == 0 {
		// A run with no eligible step summaries still records the terminal
		// outcome — an empty review field would be indistinguishable from
		// "no run yet".
		out := fmt.Sprintf("## Acceptance Review\n\n**Run:** `%s` · **Status:** %s\n\nNo step summaries were recorded.", run.ID, statusLabel)
		if len(out) > maxWorkItemDescLen {
			out = out[:maxWorkItemDescLen]
		}
		return out
	}

	out := strings.TrimSpace(b.String())
	if len(out) > maxWorkItemDescLen {
		out = out[:maxWorkItemDescLen]
	}
	return out
}

// maxWorkItemDescLen mirrors workitem's maxDescLen (1 MiB) so the
// reconciler's auto-generated review shares the API boundary's cap.
const maxWorkItemDescLen = 1 << 20

// depsSatisfied returns true if all depends_on steps of `step` are in a
// terminal-success state (succeeded or skipped). Loop decision steps
// accept a failed upstream as satisfied so they can evaluate looping.
//
// A SUPERSEDED step run never satisfies deps: it was replaced by a loop
// re-entry (e.g. a rejected approval loops back), and the success branch
// must not fire until the new iteration re-approves. Without this, a
// rejected approval left "succeeded" + superseded would still let the
// downstream success-branch step dispatch in the same pass.
func (r *WorkflowReconciler) depsSatisfied(step workflow.StepWire, runs map[string]db.WorkflowStepRunRow) bool {
	isLoopDecision := step.Kind == domain.StepKindLoopDecision
	for _, dep := range step.DependsOn {
		sr, ok := runs[dep]
		if !ok {
			r.log.Debug("DEBUG: depsSatisfied dep not in map", "step", step.ID, "dep", dep)
			return false
		}
		r.log.Debug("DEBUG: depsSatisfied check", "step", step.ID, "dep", dep, "depStatus", sr.Status)
		if sr.SupersededBy != "" {
			r.log.Debug("DEBUG: depsSatisfied dep superseded", "step", step.ID, "dep", dep)
			return false
		}
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
func (r *WorkflowReconciler) dispatchStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, step workflow.StepWire, sr db.WorkflowStepRunRow, runs map[string]db.WorkflowStepRunRow, allSteps []workflow.StepWire, dispatchedSteps *[]dispatchReq, recoveryTriggers *[]recoveryTriggerReq) error {
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
		// The work item is a shared INPUT reference: the step reads the
		// ticket (title/description/AC + upstream step-run context) and
		// produces its OWN execution + output. The ticket is NEVER mutated
		// here — no assigned_worker_ref / workflow_step_id / prompt_context
		// / status writes. Two steps bound to the same ticket can therefore
		// dispatch in parallel; each gets its own execution keyed by this
		// step run, and the ticket itself just stays "running" for the run.
		//
		// For recovering steps, the retry work item id is stored in
		// _work_item_id (set when the step was first dispatched).
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
		workerVer, err := db.GetWorkerVersionByID(ctx, tx, tenantID, step.Ref, fmt.Sprintf("v%d", step.WorkerVersion))
		if err != nil {
			if err == db.ErrNotFound {
				// Fall back to latest published — supports workflows
				// that don't pin a specific version.
				workerVer, err = db.GetLatestWorkerVersion(ctx, tx, tenantID, step.Ref, true)
				if err != nil {
					// The worker has no usable version (deleted, no
					// published version, or an index/catalog anomaly hid
					// it). Fail THIS step instead of aborting the whole
					// pass: returning an error here makes reconcileRun
					// roll back the ENTIRE transaction — including steps
					// that already completed this pass (e.g. an upstream
					// step that was polled terminal just before). The run
					// then stays "running" forever with a succeeded
					// execution underneath (the wedge seen in the field).
					// Failing the step surfaces a clear reason, the run
					// terminalizes, and recovery (or the RetryStepRun RPC)
					// re-drives it once the worker is resolvable.
					return r.failStep(ctx, tx, tenantID, run, sr, runs,
						fmt.Errorf("load worker version for %s: %w", step.Ref, err))
				}
			} else {
				return fmt.Errorf("load worker version for %s: %w", step.Ref, err)
			}
		}
		// Build the composite prompt from the (read-only) work item +
		// upstream step-run context. Mark THIS step as the current one in
		// the in-memory copy so buildCompositePrompt positions the worker
		// in the DAG — the DB row is untouched.
		var primaryWID string
		var composite string
		for _, wid := range upstream {
			wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
			if err != nil {
				if err == db.ErrNotFound {
					return r.failStep(ctx, tx, tenantID, run, sr, runs,
						fmt.Errorf("work item %s not found", wid))
				}
				return fmt.Errorf("load work item: %w", err)
			}
			wi.WorkflowStepID = sr.StepID
			composite, err = r.buildCompositePrompt(ctx, tx, tenantID, wi, workerVer, allSteps, runs)
			if err != nil {
				return fmt.Errorf("build composite prompt for %s: %w", wid, err)
			}
			primaryWID = wid
		}
		// Record the primary work item id + the composite prompt + the
		// step's worker on the STEP RUN (per-step, not on the shared
		// ticket). The inline DispatchTask reads _prompt / _worker_id /
		// _worker_version from the step run to build the execution
		// manifest; the ticket stays untouched.
		stepResult, _ := json.Marshal(map[string]any{
			"_work_item_id":   primaryWID,
			"_prompt":         composite,
			"_worker_id":      step.Ref,
			"_worker_version": step.WorkerVersion,
		})
		// Preserve the recovery narrative across a re-dispatch so the run
		// view keeps showing it after the step runs again.
		if sr.Status == domain.StepRunRecovering {
			var prev struct {
				RecoverySummary string `json:"_recovery_summary"`
			}
			_ = json.Unmarshal(sr.Result, &prev)
			if prev.RecoverySummary != "" {
				var newResult map[string]any
				_ = json.Unmarshal(stepResult, &newResult)
				newResult["_recovery_summary"] = prev.RecoverySummary
				stepResult, _ = json.Marshal(newResult)
			}
		}
		// Clear any stale worker_execution_id (a recovering step
		// re-dispatched here still references its FAILED execution):
		// pollTaskStep would otherwise see the old failed execution and
		// re-trigger recovery in the same pass, racing the inline dispatch
		// that links the replacement execution. With the id cleared, the
		// step WAITS (empty-link path) until the inline DispatchTask writes
		// the new execution onto the step run.
		updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
			Status:            strPtr(domain.StepRunRunning),
			Result:            &stepResult,
			WorkerExecutionID: strPtr(""),
			StartedAt:         &now,
		})
		if err != nil {
			return fmt.Errorf("mark task step running: %w", err)
		}
		runs[step.ID] = updated
		if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
			return fmt.Errorf("enqueue step_started: %w", err)
		}
		if dispatchedSteps != nil {
			*dispatchedSteps = append(*dispatchedSteps, dispatchReq{taskID: primaryWID, stepRunID: sr.ID})
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
			Decision   string `json:"_decision"`
		}
		var upRun db.WorkflowStepRunRow
		var upstreamStatus string
		for _, dep := range step.DependsOn {
			if s, ok := runs[dep]; ok {
				upRun = s
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
				// The failed execution + step run are the upstream step
				// run's own (not GetLatestExecutionForTask — on a shared
				// work item that could resolve to a different step's run).
				failedExecID := upRun.WorkerExecutionID
				// Defer the trigger to post-commit (see reconcileRun):
				// TriggerOnFailure opens its own transaction, which would
				// block on this pass's locks on the same work item.
				if recoveryTriggers != nil {
					*recoveryTriggers = append(*recoveryTriggers, recoveryTriggerReq{
						tenantID:     tenantID,
						workItemID:   upResult.WorkItemID,
						failedExecID: failedExecID,
						stepRunID:    upRun.ID,
						reason:       "loop_decision:upstream_failed",
					})
				}
			}
			// Guard against runaway iteration generation: when the upstream
			// reviewer is FAILED, depsSatisfied treats the failed upstream as
			// satisfied for THIS loop decision, so the freshly-created pending
			// iteration re-dispatches on the next DAG pass and (upstream still
			// failed) spawns ANOTHER iteration — all within one reconcile
			// transaction. That floods workflow_step_runs with same-transaction
			// rows (identical created_at → the "last row wins" ordering hazard
			// that wedged the DAG in the field). If an ACTIVE (non-superseded)
			// pending loop-decision iteration already exists for this step, the
			// recovery re-dispatches the reviewer and re-evaluates when it
			// lands — don't create a duplicate.
			hasPendingIter := false
			if iterRuns, ierr := listStepRunsByStepID(ctx, tx, tenantID, run.ID, step.ID); ierr == nil {
				for _, prev := range iterRuns {
					if prev.SupersededBy == "" && prev.ID != sr.ID && prev.Status == domain.StepRunPending {
						hasPendingIter = true
						break
					}
				}
			}
			if hasPendingIter {
				r.log.Info("loop_decision: upstream failed, pending iteration exists — waiting",
					"run", run.ID, "step", step.ID)
				break
			}
			nextIter := currentLoopIteration(runs, step.ID) + 1
			if err := r.createLoopDecisionIteration(ctx, tx, tenantID, run, sr, step, runs, nextIter, now, `{"loop":"recovered"}`); err != nil {
				return err
			}
			r.log.Info("loop_decision: upstream failed, new iteration waiting", "run", run.ID, "step", step.ID)
			break
		}

		// Upstream succeeded. Prefer the decision from the upstream STEP
		// RUN's result (the ticket is a shared input reference — its
		// results are the run-level narrative, not per-step decisions).
		// Fall back to the ticket for legacy/custom decision fields.
		var decision string
		if upResult.Decision != "" {
			decision = upResult.Decision
		} else if upResult.WorkItemID != "" {
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

			// Count REAL re-asks — step runs created by reaskDecisionStep
			// (StepName "Reviewer (re-ask)"). NOT the reviewer's loop
			// iteration count: a reviewer that legitimately looped back via
			// explicit _decision: failure decisions (or was accepted) has
			// never been RE-ASKED, so its iterations must not consume the
			// re-ask budget. Otherwise a truncated final turn (missing
			// signal) hits an already-exhausted budget and fails without
			// ever getting a genuine re-ask — the observed bug. Superseded
			// re-ask runs are included (they still happened).
			reaskList, err := listStepRunsByStepID(ctx, tx, tenantID, run.ID, reviewerStepID)
			if err != nil {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("loop_decision step %q: list reviewer runs: %w", step.Name, err))
			}
			reaskCount := countReaskRuns(reaskList)
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
		cfg := parseApprovalConfig(step.Config)

		// Capture upstream context from preceding steps for the reviewer.
		upstreamWorker := ""
		upstreamSummary := ""
		var upstreamFiles []string
		ac := ""
		upstreamWorkItemID := ""
		for _, depID := range step.DependsOn {
			us, ok := runs[depID]
			if !ok || us.Status != domain.StepRunSucceeded {
				continue
			}
			var upResult struct {
				WorkItemID string `json:"_work_item_id"`
			}
			if err := json.Unmarshal(us.Result, &upResult); err != nil || upResult.WorkItemID == "" {
				continue
			}
			upstreamWorkItemID = upResult.WorkItemID
			wi, err := db.GetWorkItem(ctx, tx, tenantID, upResult.WorkItemID)
			if err != nil {
				continue
			}
			ac = wi.AcceptanceCriteria
			if len(wi.Results) > 0 {
				var wiResult map[string]any
				if json.Unmarshal(wi.Results, &wiResult) == nil {
					if v, ok := wiResult["_summary"].(string); ok {
						upstreamSummary = v
					}
					if files, ok := wiResult["_touched_files"].([]any); ok {
						for _, f := range files {
							if s, ok := f.(string); ok {
								upstreamFiles = append(upstreamFiles, s)
							}
						}
					}
				}
			}
			if exec, err := db.GetLatestExecutionForTask(ctx, tx, tenantID, upResult.WorkItemID); err == nil && exec.WorkerID != "" {
				if wrk, err := db.GetWorker(ctx, tx, tenantID, exec.WorkerID); err == nil {
					upstreamWorker = wrk.Name
				}
				if upstreamWorker == "" {
					upstreamWorker = exec.WorkerID
				}
			}
		}

		if cfg.Reviewer == "worker" {
			// Worker-backed approval: dispatch like a task step to the
			// approver worker. The step's "ref" field carries the worker
			// ID (same as TASK steps — docs/02 §2.4). Fall back to
			// config.worker_ref for backward compatibility with older
			// workflows that stored the ref in the config instead.
			workerRefStr := step.Ref
			if workerRefStr == "" {
				workerRefStr = cfg.WorkerRef
			}
			if workerRefStr == "" {
				// Missing worker ref is a workflow-definition error on
				// THIS step — fail it (clear reason, run terminalizes)
				// rather than erroring out and rolling back the whole
				// pass (which would leave the run "running" forever,
				// exactly the wedge #192 fixed for TASK steps).
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("worker-backed approval step %s has no worker ref", step.ID))
			}
			// The approval execution runs against the run's shared work
			// item (the ticket) — NO per-step approval work item is ever
			// created; the step run IS the approval record. Resolve the
			// ticket the same way TASK steps do: for recovering steps the
			// ticket is already recorded in _work_item_id; otherwise look
			// for WORK_ITEM markers upstream, then the run's bound item.
			upstream := resolveApprovalWorkItems(sr, step, allSteps, run.WorkItemID)
			if len(upstream) == 0 {
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("worker-backed approval step %q has no upstream work_item", step.Name))
			}
			workerVer, err := db.GetLatestWorkerVersion(ctx, tx, tenantID, workerRefStr, true)
			if err != nil {
				// The approver worker has no usable version (deleted, no
				// published version, or an index/catalog anomaly hid it).
				// Fail THIS step instead of aborting the whole pass:
				// returning an error here makes reconcileRun roll back the
				// ENTIRE transaction — including steps that already
				// completed this pass (the wedge #192 fixed for TASK
				// steps). Failing the step surfaces a clear reason, the
				// run terminalizes, and recovery (or the RetryStepRun
				// RPC) re-drives it once the worker is resolvable.
				return r.failStep(ctx, tx, tenantID, run, sr, runs,
					fmt.Errorf("load approver worker version for %s: %w", workerRefStr, err))
			}
			var primaryWID string
			var composite string
			for _, wid := range upstream {
				wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
				if err != nil {
					if err == db.ErrNotFound {
						return r.failStep(ctx, tx, tenantID, run, sr, runs,
							fmt.Errorf("work item %s not found", wid))
					}
					return fmt.Errorf("load work item: %w", err)
				}
				wi.WorkflowStepID = sr.StepID
				composite, err = r.buildCompositePrompt(ctx, tx, tenantID, wi, workerVer, allSteps, runs)
				if err != nil {
					return fmt.Errorf("build composite prompt for approver: %w", err)
				}
				primaryWID = wid
			}
			// Record the ticket + composite prompt + the approver worker
			// pin on the STEP RUN (exactly the TASK-step shape). The
			// inline DispatchTask reads _prompt / _worker_id /
			// _worker_version from the step run to build the execution
			// manifest; the ticket stays untouched.
			stepResult := buildApprovalStepResult(primaryWID, composite, workerRefStr, workerVer.Version,
				upstreamWorker, upstreamSummary, upstreamFiles, ac, sr.Result)
			// Clear any stale worker_execution_id (a recovering step
			// re-dispatched here still references its FAILED execution):
			// pollTaskStep would otherwise see the old failed execution
			// and re-trigger recovery in the same pass, racing the inline
			// dispatch that links the replacement execution.
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:            strPtr(domain.StepRunRunning),
				Result:            &stepResult,
				WorkerExecutionID: strPtr(""),
				StartedAt:         &now,
			})
			if err != nil {
				return fmt.Errorf("mark approval step running: %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepStarted, run, updated); err != nil {
				return fmt.Errorf("enqueue approval step_started: %w", err)
			}
			if dispatchedSteps != nil {
				*dispatchedSteps = append(*dispatchedSteps, dispatchReq{taskID: primaryWID, stepRunID: sr.ID})
			}
			r.log.Info("workflow approver dispatched",
				"run", run.ID, "step", step.ID,
				"work_item", primaryWID, "worker", workerRefStr)
		} else {
			// Human approval: set to approval_pending and wait for
			// human review via the ApproveStep RPC.
			resultPayload, _ := json.Marshal(map[string]any{
				"_work_item_id": upstreamWorkItemID,
				"_review_context": map[string]any{
					"_upstream_worker":  upstreamWorker,
					"_upstream_summary": upstreamSummary,
					"_upstream_files":   upstreamFiles,
					"_ac":               ac,
				},
				"_decision": "pending",
			})
			updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
				Status:    strPtr(domain.StepRunApprovalPending),
				StartedAt: &now,
				Result:    &resultPayload,
			})
			if err != nil {
				return fmt.Errorf("mark approval step pending: %w", err)
			}
			runs[step.ID] = updated
			if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepApproval, run, updated); err != nil {
				return fmt.Errorf("enqueue step_approval: %w", err)
			}
			r.writeApprovalInitFiles(ctx, tx, tenantID, run, updated, upstreamWorker, upstreamSummary, upstreamFiles)
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
//   1. # Task — the work item itself: title, description, acceptance
//      criteria. This is THE task; everything else is context.
//   2. # Project context — the project directory (working dir) + the
//      project's `context_files`, rendered by the shared
//      internal/contextfiles renderer. A context path may be a file
//      (inlined, capped) or a directory (expanded into a bounded
//      listing with explicit "read every file, do NOT open the
//      directory as a file" instructions).
//   3. # Work item context — the work item's own `context_files`,
//      rendered exactly like the project's (same renderer, resolved
//      against the same project_dir).
//   4. # Instructions — the worker's contract: emit the
//      ORCHICON WORKER SUMMARY marker at the end of the response so
//      the next stage can read it as upstream context. Also carries
//      the workflow-aware role context, iteration/git-branch notes,
//      the per-step recovery summary, the `.orchicon/` files to read,
//      and the execution-history timeline.
//
// (The ancestor-chain and recovery sections historically described
// below are now part of the workflow timeline rendered inside the
// instructions block.)
//
// The composite is the opencode adapter's "message" (passed via the
// manifest Goal). Sections in order:
//
//  0. Role — the worker's purpose
//  1. Task — the work item
//  2. Project context — directory and file contents
//  3. Work item context — the item's own context files/directories
//  4. Instructions — read .orchicon/ for previous step results,
//     then output format including decision prefix
func (r *WorkflowReconciler) buildCompositePrompt(ctx context.Context, tx pgx.Tx, tenantID string, wi db.WorkItemRow, worker db.WorkerVersionRow, allSteps []workflow.StepWire, runs map[string]db.WorkflowStepRunRow) (string, error) {
	var sb strings.Builder
	sb.WriteString(workerIdentityPreamble)

	// 0. Worker identity — role, skills, behavior, and AGENTS.md.
	if r := strings.TrimSpace(worker.Role); r != "" {
		fmt.Fprintf(&sb, "# Role\n\n%s\n\n", r)
	}
	if s := strings.TrimSpace(worker.Skills); s != "" {
		fmt.Fprintf(&sb, "## Skills\n\n%s\n\n", s)
	}
	if b := strings.TrimSpace(worker.Behavior); b != "" {
		fmt.Fprintf(&sb, "## Behavior\n\n%s\n\n", b)
	}
	if a := strings.TrimSpace(worker.AgentsMD); a != "" {
		fmt.Fprintf(&sb, "## AGENTS.md\n\n%s\n\n", a)
	}

	// 1. Task.
	sb.WriteString("# Task\n\n")
	fmt.Fprintf(&sb, "Original work item: \"%s\"\n\n", strings.TrimSpace(wi.Title))
	if d := strings.TrimSpace(wi.Description); d != "" {
		fmt.Fprintf(&sb, "Description:\n%s\n\n", d)
	}
	if ac := strings.TrimSpace(wi.AcceptanceCriteria); ac != "" {
		fmt.Fprintf(&sb, "Acceptance criteria:\n%s\n\n", ac)
	}

	// 2. Project context — directory + context files (files AND
	//    directories; directories are expanded into a bounded listing
	//    with explicit read instructions — internal/contextfiles).
	if wi.ProjectID != "" {
		var p db.ProjectRow
		if err := tx.QueryRow(ctx,
			`SELECT project_dir, context_files FROM projects WHERE id = $1 AND tenant_id = $2`,
			wi.ProjectID, tenantID,
		).Scan(&p.ProjectDir, &p.ContextFiles); err == nil {
			var sb2 strings.Builder
			if p.ProjectDir != "" {
				fmt.Fprintf(&sb2, "Working directory: `%s`\n\n", p.ProjectDir)
			}
			var files []string
			_ = json.Unmarshal(p.ContextFiles, &files)
			sb2.WriteString(contextfiles.Render("# Project context", files, p.ProjectDir))
			if sb2.Len() > 0 {
				sb.WriteString(sb2.String())
			}
		}
	}

	// 3. Work item context — the item's own context_files (files AND
	//    directories), rendered exactly like the project's, resolved
	//    against the same project_dir (backward-compat rule).
	if len(wi.ContextFiles) > 0 {
		var files []string
		_ = json.Unmarshal(wi.ContextFiles, &files)
		projectDir := ""
		if wi.ProjectID != "" {
			var pd string
			_ = tx.QueryRow(ctx,
				`SELECT project_dir FROM projects WHERE id = $1 AND tenant_id = $2`,
				wi.ProjectID, tenantID,
			).Scan(&pd)
			projectDir = pd
		}
		if r := contextfiles.Render("# Work item context", files, projectDir); r != "" {
			sb.WriteString(r)
		}
	}

	// 4. Instructions.
	sb.WriteString("# Instructions\n\n")

	// The workflow decision comes from exactly ONE signal: the word after
	// ORCHICON WORKER SUMMARY:. There is no separate _issues:/_decision:
	// failure channel. Blocking problems are reported by ending with
	// `ORCHICON WORKER SUMMARY: failure` and describing them in the
	// summary text — that is the only way the workflow routes to a loop.
	sb.WriteString("The workflow routes on exactly one signal: the word after `ORCHICON WORKER SUMMARY:` — `success` or `failure`. There is no `_issues:` failure channel. If work genuinely cannot be accepted, end with `ORCHICON WORKER SUMMARY: failure` and say what needs fixing in the summary text. Non-blocking observations belong in the summary text only and never affect the routing.\n\n")

	// Workflow-aware role context: tell the worker where they fit in the
	// overall workflow so they don't perform work meant for other steps.
	// Count worker-facing steps (task and approval) in topological order
	// (execution flow determined by depends_on edges, not canvas position).
	// Routing nodes like loop_decision, decision, parallel, project, and
	// work_item are excluded from the count.
	type stepMeta struct{ idx int; name string; id string }
	var allMeta []stepMeta // all steps, used to build the dependency graph
	var activeMeta []stepMeta // only task + approval
	myPos := -1
	myName := ""
	stepIdx := make(map[string]int)
	for i, s := range allSteps {
		stepIdx[s.ID] = i
		allMeta = append(allMeta, stepMeta{i, s.Name, s.ID})
		if s.Kind == "task" || s.Kind == "approval" {
			activeMeta = append(activeMeta, stepMeta{0, s.Name, s.ID})
		}
	}
	// Kahn's algorithm for topological order over all steps.
	inDeg := make([]int, len(allSteps))
	adj := make([][]int, len(allSteps))
	for _, s := range allSteps {
		for _, dep := range s.DependsOn {
			if dIdx, ok := stepIdx[dep]; ok {
				adj[dIdx] = append(adj[dIdx], stepIdx[s.ID])
				inDeg[stepIdx[s.ID]]++
			}
		}
	}
	var topo []int
	q := make([]int, 0, len(allSteps))
	for i, d := range inDeg {
		if d == 0 {
			q = append(q, i)
		}
	}
	for len(q) > 0 {
		u := q[0]
		q = q[1:]
		topo = append(topo, u)
		for _, v := range adj[u] {
			inDeg[v]--
			if inDeg[v] == 0 {
				q = append(q, v)
			}
		}
	}
	// Build a position map: index in topological sort → step ID.
	topoPos := make(map[string]int)
	for i, idx := range topo {
		topoPos[allSteps[idx].ID] = i
	}
	// Sort active steps by their topological position in the full DAG.
	sort.SliceStable(activeMeta, func(i, j int) bool {
		return topoPos[activeMeta[i].id] < topoPos[activeMeta[j].id]
	})
	for i, s := range activeMeta {
		activeMeta[i].idx = i
		if s.id == wi.WorkflowStepID {
			myPos = i
			myName = s.name
		}
	}
	if myPos >= 0 && len(activeMeta) > 0 {
		fmt.Fprintf(&sb, "This workflow has %d steps. You are step %d — %s. Focus on your specific role and let other workers handle their steps.\n\n", len(activeMeta), myPos+1, myName)
	}

	// Iteration context: tell the worker if this is a re-do (loop-back).
	// The step run's iteration counter starts at 0 for the first pass
	// and increments each time the chain re-enters.
	if currentRun, ok := runs[wi.WorkflowStepID]; ok && currentRun.Iteration > 0 {
		fmt.Fprintf(&sb, "This is iteration %d of this step. You may have done this work before — review your previous output and the feedback from downstream steps before repeating yourself.\n\n", currentRun.Iteration)
	}

	// Git branch guidance: avoid creating multiple branches across
	// iterations. The worker should use the existing branch.
	sb.WriteString("Use the existing git branch from the previous iteration if one exists. Do NOT create a new branch unless the previous work was on `main`.\n\n")

	// Recovery context: if THIS step is being re-dispatched after a
	// recovery (its step run is recovering and carries a recovery
	// summary), show the narrative so the replacement execution learns
	// from the failure instead of repeating it ("same failure twice"
	// loop).
	if currentRun, ok := runs[wi.WorkflowStepID]; ok {
		var recMeta struct {
			RecoverySummary string `json:"_recovery_summary"`
		}
		_ = json.Unmarshal(currentRun.Result, &recMeta)
		if recMeta.RecoverySummary != "" {
			fmt.Fprintf(&sb, "## Recovery\n\nA previous execution of this step failed and was recovered. Recovery summary:\n%s\n\n", recMeta.RecoverySummary)
		}
	}

	// Only instruct the worker to read .orchicon/ files when prior
	// steps have completed. On the first step there's nothing to read.
	hasPriorSteps := false
	for _, sr := range runs {
		if sr.Status == domain.StepRunSucceeded || sr.Status == domain.StepRunFailed {
			hasPriorSteps = true
			break
		}
	}
	if hasPriorSteps {
		sb.WriteString("Read this run's `.orchicon/` files from the working directory to see the previous step's results:\n\n")
		fmt.Fprintf(&sb, "- `.orchicon/%s/status` — `success` or `failure` from the previous step\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/summary` — what the previous worker did\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/issues` — issues found by the previous reviewer (if any)\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/worker` — which worker produced the previous results\n", wi.WorkflowRunID)
		fmt.Fprintf(&sb, "- `.orchicon/%s/attachments/` — files/screenshots the human attached to an approval decision (read them!)\n\n", wi.WorkflowRunID)
	}
	sb.WriteString("Review the task above, but only complete the work that matches your Role and the step you are assigned to. When you have finished, end your response with the literal line `ORCHICON WORKER SUMMARY:` followed by one word — either `success` or `failure` — and a short paragraph summarizing what you did.\n\n")
	sb.WriteString("Format:\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: success — Implemented the feature.\n```\n")
	sb.WriteString("or\n")
	sb.WriteString("```\nORCHICON WORKER SUMMARY: failure — Found 3 bugs in the implementation.\n```\n\n")
	sb.WriteString("The first word (`success` or `failure`) is used to route the workflow. The text after `—` is passed to the next stage as the summary of your work.\n\n")
	sb.WriteString("**Important:** The workflow routes on the single word after `ORCHICON WORKER SUMMARY:` — `success` or `failure`. There is no `_issues:` failure channel: any `_issues:` block in your response is informational only and never changes the routing. If you find blocking problems, end with `failure` and explain them in the summary text. If you have only minor suggestions, keep the routing `success` and mention them in your summary text.\n\n")

	// Prior execution timeline: show what each completed step produced,
	// so the worker understands the full context including loop-backs.
	sb.WriteString("## Execution history\n\n")
	sb.WriteString("The following steps have completed in this workflow run. If a step ran multiple times (loop-back), each iteration is listed.\n")
	// Build a step-ID→name lookup from allSteps.
	stepNameByID := make(map[string]string)
	for _, s := range allSteps {
		stepNameByID[s.ID] = s.Name
	}
	type histEntry struct{ stepName, status, summary, issues, reason, iteration string; attachments []string }
	var history []histEntry
	seen := make(map[string]bool)
	for stepID, sr := range runs {
		sr := sr
		if _, ok := stepNameByID[stepID]; !ok {
			continue
		}
		if sr.Status != domain.StepRunSucceeded && sr.Status != domain.StepRunFailed {
			continue
		}
		var rData struct {
			Summary     string   `json:"_summary"`
			Issues      string   `json:"_issues"`
			Reason      string   `json:"_reason"`
			Attachments []struct {
				Filename string `json:"filename"`
				Path     string `json:"path"`
			} `json:"_attachments"`
		}
		json.Unmarshal(sr.Result, &rData)
		iterLabel := "first"
		if sr.Iteration > 0 {
			iterLabel = fmt.Sprintf("iteration %d", sr.Iteration)
		}
		entry := histEntry{
			stepName:  stepNameByID[stepID],
			status:    sr.Status,
			summary:   rData.Summary,
			issues:    rData.Issues,
			reason:    rData.Reason,
			iteration: iterLabel,
		}
		for _, a := range rData.Attachments {
			if a.Filename != "" {
				entry.attachments = append(entry.attachments, a.Filename)
			}
		}
		if !seen[sr.ID] {
			history = append(history, entry)
			seen[sr.ID] = true
		}
	}
	// Sort by topological order.
	sort.SliceStable(history, func(i, j int) bool {
		var pi, pj int
		for _, s := range allSteps {
			if s.Name == history[i].stepName { pi = topoPos[s.ID] }
			if s.Name == history[j].stepName { pj = topoPos[s.ID] }
		}
		return pi < pj
	})
	if len(history) > 0 {
		for _, h := range history {
			statusSym := "✓"
			if h.status == domain.StepRunFailed {
				statusSym = "✗"
			}
			fmt.Fprintf(&sb, "- **%s** %s [%s]", h.stepName, statusSym, h.iteration)
			if h.status == domain.StepRunSucceeded {
				sb.WriteString(" succeeded")
			} else {
				sb.WriteString(" failed")
			}
			if h.summary != "" {
				if len(h.summary) > 120 {
					h.summary = h.summary[:120] + "…"
				}
				fmt.Fprintf(&sb, ": %s", h.summary)
			}
			sb.WriteString("\n")
			if h.issues != "" {
				if len(h.issues) > 120 {
					h.issues = h.issues[:120] + "…"
				}
				fmt.Fprintf(&sb, "  - Issues: %s\n", h.issues)
			}
			// Human review feedback (approval step) + any attached files /
			// screenshots the human shared so the worker can SEE the issues.
			if h.reason != "" {
				fmt.Fprintf(&sb, "  - Human review: %s\n", h.reason)
			}
			if len(h.attachments) > 0 {
				fmt.Fprintf(&sb, "  - Human attachments (read them from the project dir): %s\n", strings.Join(h.attachments, ", "))
			}
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("No steps have completed yet. You are the first.\n\n")
	}
	sb.WriteString("If you produce an output file, use the `write` tool (not `bash` with a heredoc). The `write` tool saves the file and orchicon captures it as an inline artifact.\n")

	// Machine-generated runtime environment facts (never authored worker
	// context): the worker runs in an ephemeral rootless container with a
	// known toolkit, so it stops probing the environment and can focus on
	// the work. This reflects the resolved image for the run.
	sb.WriteString(runtimeEnvironmentBlock(wi.RuntimeImage))

	return sb.String(), nil
}

// runtimeEnvironmentBlock is the machine-generated "## Runtime
// environment" section appended to every composite prompt. It tells the
// worker the ground truth about its execution sandbox so it does not
// waste cycles empirically probing the container (and so it uses the
// rootless system-library escape hatch instead of hitting a wall).
func runtimeEnvironmentBlock(image string) string {
	img := strings.TrimSpace(image)
	if img == "" {
		img = "the default Orchicon runtime base image"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n## Runtime environment\n\n")
	fmt.Fprintf(&sb, "You are running inside an ephemeral, rootless Linux container (`%s`). Everything you install is wiped when the workflow run ends, so only save durable work to the project directory.\n\n", img)
	sb.WriteString("- **Scratch directory:** `/tmp/orchicon` is the ONE place outside the project you may read and write. Put ephemeral files there (screenshots, logs, downloaded artifacts you need to inspect). It is wiped at run end — never put durable work there, and always save final outputs to the project directory.\n")
	sb.WriteString("- You are **not root** and cannot become root: `sudo` is blocked and `apt-get` refuses to run without root. Do not attempt them.\n")
	sb.WriteString("- You may install tools freely into the ephemeral filesystem with the user-space package managers that ship in the image: `pip install` (PIP_BREAK_SYSTEM_PACKAGES is set), `npm install`, `mise install <tool>`, `uv`, `bun`, `curl`. These need no root and are wiped at run end.\n")
	sb.WriteString("- System packages are baked at build time; `apt-get install` will not work. If you need a system shared library that is missing (e.g. `libGL.so.1` for a GUI toolkit), fetch and extract it without root:\n\n")
	sb.WriteString("    apt-get download <pkg> && dpkg-deb -x <pkg>*.deb /tmp/libs && export LD_LIBRARY_PATH=/tmp/libs/usr/lib/x86_64-linux-gnu:$LD_LIBRARY_PATH\n\n")
	sb.WriteString("- There is no X server and usually no offscreen graphics libs. Prefer headless modes for GUI toolkits (e.g. `QT_QPA_PLATFORM=offscreen`), or install the missing libs with the pattern above.\n")
	return sb.String()
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

// upstreamContext renders a compact "Workflow context" section for
// the worker. Each prior step is a short card: decision, summary,
// and list of touched files. No full output dumps. The current step
// is marked so the worker can orient itself.
//
// Per-step rendering (see renderUpstreamStep):
//
//   - TASK: _decision, _summary, _touched_files from the work item's
//     results. No full output dump.
//   - WORK_ITEM: linked work item title + a short description.
//   - PROJECT: project name only.
//   - DECISION / APPROVAL / PARALLEL: status only.
//
// Returns "" when the run has no completed steps yet. Walks the DAG
// by step-id order via allSteps.
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
	// so the worker can see at a glance what is expected of them.
	sb.WriteString("---\n\n")
	if currentStepID != "" {
		for _, s := range allSteps {
			if s.ID != currentStepID {
				continue
			}
			sb.WriteString("**→ Your task:** ")
			fmt.Fprintf(&sb, "You are executing **%s**", strings.TrimSpace(s.Name))
			if s.Kind == domain.StepKindTask && s.Ref != "" {
				fmt.Fprintf(&sb, " as `%s`", s.Ref)
			}
			sb.WriteString(". Complete the work in the *Task* section above, then end with the marker.\n\n")
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
		// The step run's result holds the execution's fields — _decision,
		// _summary, _touched_files, _recovery_summary (written by
		// propagateStepRunResults on completion + the recovery engine on
		// resume). The shared ticket is NOT read: it is an input
		// reference, and its results are the run-level narrative.
		var parsed map[string]any
		if len(sr.Result) > 0 {
			_ = json.Unmarshal(sr.Result, &parsed)
		}
		if d, ok := parsed["_decision"].(string); ok && d != "" {
			fmt.Fprintf(sb, "Decision: %s\n", d)
		}
		if summary, ok := parsed["_summary"].(string); ok && summary != "" {
			fmt.Fprintf(sb, "Summary: %s\n", summary)
		}
		if files, ok := parsed["_touched_files"].([]any); ok && len(files) > 0 {
			paths := make([]string, 0, len(files))
			for _, f := range files {
				if s, ok := f.(string); ok {
					paths = append(paths, s)
				}
			}
			if len(paths) > 0 {
				fmt.Fprintf(sb, "Files: `%s`\n", strings.Join(paths, "`, `"))
			}
		}
		if recSummary, ok := parsed["_recovery_summary"].(string); ok && recSummary != "" {
			fmt.Fprintf(sb, "Recovery: %s\n", recSummary)
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

// readProjectContextFiles renders the project's context_files (files AND
// directories) as a "# Project context" prompt section via the shared
// contextfiles.Render. Directories are expanded into a bounded listing
// and the worker is instructed to read them rather than opening the
// directory as a file ("not a file"). Retained for the standalone
// dispatch path and as the canonical single place project context is
// turned into prompt text.
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
	return contextfiles.Render("# Project context", files, p.ProjectDir), nil
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

// resolveRuntimeImage determines the runtime container image for a run
// at start:
//   - template runs (run.WorkItemID set): the bound work item's stored
//     runtime_image (backend-stamped; empty = base image);
//   - one-shot runs: the WORK_ITEM canvas markers' work items' stored
//     runtime_image values. All empty → base image; one distinct non-empty
//     → that image; two different non-empty values → error (a single
//     container cannot serve two images).
//
// The resolved value is stored on the run row so the adapter's self-heal
// recreates the container with the identical image.
func (r *WorkflowReconciler) resolveRuntimeImage(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, steps []workflow.StepWire) (string, error) {
	if run.WorkItemID != "" {
		wi, err := db.GetWorkItem(ctx, tx, tenantID, run.WorkItemID)
		if err != nil {
			return "", fmt.Errorf("get bound work item: %w", err)
		}
		return strings.TrimSpace(wi.RuntimeImage), nil
	}
	// One-shot: collect WORK_ITEM markers' images; all must agree.
	values := []string{}
	for _, s := range steps {
		if s.Kind != domain.StepKindWorkItem {
			continue
		}
		wid := readConfigWorkItemID(s.Config)
		if wid == "" {
			continue
		}
		wi, err := db.GetWorkItem(ctx, tx, tenantID, wid)
		if err != nil {
			if err == db.ErrNotFound {
				continue
			}
			return "", fmt.Errorf("get marker work item %s: %w", wid, err)
		}
		if img := strings.TrimSpace(wi.RuntimeImage); img != "" {
			values = append(values, img)
		}
	}
	return resolveImageFromValues(values)
}

// resolveImageFromValues applies the one-shot image agreement rule: all
// non-empty values must be identical, or a single container cannot serve
// the run. Empty (no markers, or all unset) → "" (base image).
func resolveImageFromValues(values []string) (string, error) {
	var chosen string
	for _, img := range values {
		if img == "" {
			continue
		}
		if chosen == "" {
			chosen = img
			continue
		}
		if img != chosen {
			return "", fmt.Errorf("conflicting runtime images on work items (%s vs %s) — a workflow run uses one container", chosen, img)
		}
	}
	return chosen, nil
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

// resolveApprovalWorkItems determines the work item(s) a worker-backed
// approval execution runs against. NO approval work item is ever
// created — the step run IS the approval record, so the execution is
// dispatched against the run's shared ticket, resolved exactly like a
// TASK step:
//   - a recovering step re-uses the ticket already recorded in its
//     result's _work_item_id;
//   - otherwise WORK_ITEM markers upstream of the approval step;
//   - otherwise the run's bound work item (run.WorkItemID).
//
// Returns nil when no ticket exists; the caller fails the step with a
// clear message (same contract as TASK steps).
func resolveApprovalWorkItems(sr db.WorkflowStepRunRow, step workflow.StepWire, allSteps []workflow.StepWire, runWorkItemID string) []string {
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
		upstream = upstreamWorkItemIDs(step, allSteps)
		if len(upstream) == 0 && runWorkItemID != "" {
			upstream = []string{runWorkItemID}
		}
	}
	return upstream
}

// buildApprovalStepResult builds the result JSON written to a
// worker-backed approval step run when it is dispatched. The step run
// is the approval record: it carries the shared ticket id, the composite
// prompt, the approver worker pin (so TaskReconciler.workerVersionForStepRun
// resolves the approver without touching the ticket), the upstream
// review context, and the pending decision marker. When re-dispatching a
// recovering step, the previous _recovery_summary is preserved.
func buildApprovalStepResult(primaryWID, composite, workerRefStr string, workerVersion int, upstreamWorker, upstreamSummary string, upstreamFiles []string, ac string, prevResult []byte) []byte {
	stepResult, _ := json.Marshal(map[string]any{
		"_work_item_id":     primaryWID,
		"_prompt":           composite,
		"_worker_id":        workerRefStr,
		"_worker_version":   workerVersion,
		"_upstream_worker":  upstreamWorker,
		"_upstream_summary": upstreamSummary,
		"_upstream_files":   upstreamFiles,
		"_ac":               ac,
		"_decision":         "pending",
	})
	var prev struct {
		RecoverySummary string `json:"_recovery_summary"`
	}
	_ = json.Unmarshal(prevResult, &prev)
	if prev.RecoverySummary != "" {
		var newResult map[string]any
		_ = json.Unmarshal(stepResult, &newResult)
		newResult["_recovery_summary"] = prev.RecoverySummary
		stepResult, _ = json.Marshal(newResult)
	}
	return stepResult
}

// approvalDecisionFromSources resolves the authoritative decision for an
// approval step run. The step run's own _decision wins — it is the
// approval record (written by the ApproveStep RPC for human reviews, or
// propagated from the approver execution's ORCHICON WORKER SUMMARY for
// worker-backed approvals). The work item's _decision is consulted ONLY
// as a legacy fallback when the step run carries none, so a stale
// decision left on a shared ticket by a prior run/step can never
// override the current step run's real decision.
func approvalDecisionFromSources(srDecision string, wiResults []byte) string {
	if srDecision != "" {
		return srDecision
	}
	if len(wiResults) == 0 {
		return ""
	}
	var wiResult map[string]any
	if json.Unmarshal(wiResults, &wiResult) != nil {
		return ""
	}
	if v, ok := wiResult["_decision"].(string); ok {
		return v
	}
	return ""
}

// dispatchReq is an inline TaskReconciler dispatch queued during a
// reconcile pass and invoked after the pass's transaction commits.
// Dispatch is scoped to a (task, step run) pair: the work item is a
// shared input reference for all steps bound to it, so each step run
// gets its own execution without mutating the item.
type dispatchReq struct {
	taskID    string
	stepRunID string
}

// recoveryTriggerReq is a deferred recovery trigger collected during a
// reconcile pass and invoked AFTER the pass's transaction commits (see
// reconcileRun — TriggerOnFailure opens its own transaction on a separate
// connection, so invoking it while the pass still holds row locks on the
// affected work item would cross-connection self-deadlock). stepRunID is
// the failing step run the recovery targets ("" for non-workflow).
type recoveryTriggerReq struct {
	tenantID     string
	workItemID   string
	failedExecID string
	stepRunID    string
	reason       string
}

// dispatchLinkGrace is how long a task step running with NO execution
// link waits before it is treated as a lost dispatch (inline DispatchTask
// failed / adapter down) and routed to recovery instead of hanging as
// "running" forever. The inline dispatch links the execution moments
// after the reconcile transaction commits, so the grace is generous.
const dispatchLinkGrace = 15 * time.Second

// maxDAGPasses bounds the per-run DAG progression loop. A well-behaved
// run completes its progression in a handful of passes (each step
// transitions at most a few states); a pathological run whose step
// status flips every iteration would otherwise loop forever, wedging the
// reconcile goroutine in a busy loop (field incident: 150% CPU, the
// reconciler's advisory lock never renewed). The bound converts the
// pathology into a single errored pass; the manager requeues the run and
// a later pass (or the stuck-run re-drive) retries it.
const maxDAGPasses = 100

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
func (r *WorkflowReconciler) pollTaskStep(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, stepConfig string, runByID map[string]db.WorkflowStepRunRow, recoveryTriggers *[]recoveryTriggerReq) (bool, bool, error) {
	var parsed struct {
		WorkItemID string `json:"_work_item_id"`
	}
	if err := json.Unmarshal(sr.Result, &parsed); err != nil || parsed.WorkItemID == "" {
		return false, false, nil
	}
	wi, err := db.GetWorkItem(ctx, tx, tenantID, parsed.WorkItemID)
	if err != nil {
		if err == db.ErrNotFound {
			// The work item this step reads (its shared ticket, a work-item
			// marker's item, or a retry artifact) was hard-deleted mid-run —
			// project cleanup or a manual delete. The step can never produce
			// output: waiting forever would wedge the run "running" and leak
			// its runtime container (observed field symptom). Fail the step
			// terminal — the run then terminalizes and the container is
			// reaped. NOT an error return: bubbling an error would roll back
			// the whole reconcile transaction, including steps that already
			// completed this pass (the wedge anti-pattern).
			r.log.Warn("task step work item deleted — failing step",
				"run", run.ID, "step", sr.StepID, "work_item", parsed.WorkItemID)
			return true, true, nil
		}
		return false, false, fmt.Errorf("get work item: %w", err)
	}

	// Task AND worker-backed approval steps complete on their OWN
	// execution's terminal state. (Human approval steps sit in
	// `approval_pending` and are resolved by the ApproveStep/RejectStep
	// RPC, not polled here.) The approval step run's `_work_item_id`
	// points at the run's SHARED ticket, which is never polled for
	// completion: under the run-bound model its status does not track the
	// approver's execution (transitionWorkItemOnResult leaves it
	// untouched), so polling it would leave the step "running" forever
	// even after the approver succeeded. A step run with NO execution
	// link means the dispatch never produced an execution — WAIT rather
	// than fall back to another execution for the same work item.
	if sr.WorkerExecutionID == "" {
		// Two cases:
		//   (a) dispatchStep just set this step running (first dispatch
		//       or a recovering-step re-dispatch); the inline
		//       DispatchTask links the replacement execution moments
		//       after the reconcile transaction commits. Wait a short
		//       grace for the link instead of guessing.
		//   (b) the dispatch was lost (inline dispatch failed, adapter
		//       down) and no execution ever materialized. After the
		//       grace, fall through to the recovery block so the step
		//       is retried rather than wedged forever as "running".
		if sr.StartedAt != nil && time.Since(*sr.StartedAt) < dispatchLinkGrace {
			return false, false, nil
		}
	} else {
		exec, err := db.GetExecution(ctx, tx, tenantID, sr.WorkerExecutionID)
		if err != nil {
			if err == db.ErrNotFound {
				// The execution this step references was hard-deleted (or
				// never persisted) — e.g. a project cleanup that removed the
				// execution, or a lost inline-dispatch link. Waiting forever
				// would wedge the run "running" and leak its runtime
				// container. After the dispatch-link grace, fall through to
				// the recovery block so the step is retried (bounded by
				// max_attempts) rather than stuck "running" indefinitely.
				if sr.StartedAt != nil && time.Since(*sr.StartedAt) < dispatchLinkGrace {
					return false, false, nil
				}
				// fall through to the recovery block below.
			} else {
				// Transient DB error — not an orphan; retry next pass.
				return false, false, nil
			}
		} else {
			switch exec.Status {
			case domain.ExecutionSucceeded:
				return true, false, nil
			case domain.ExecutionFailed, domain.ExecutionFailedToStart, domain.ExecutionTerminated:
				// fall through to the recovery block below.
			default:
				return false, false, nil
			}
		}
	}

	{
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
			if sr.StepKind == domain.StepKindApproval {
				// Worker-backed approval: the step run IS the approval
				// record — never clone the shared ticket into a fresh
				// work item on retry. Keep the same _work_item_id and
				// mark the step recovering; the dispatch section
				// re-dispatches the SAME step run on the next pass
				// (dispatchStep re-resolves the ticket and clears the
				// stale execution link). Bounded by max_attempts below.
				stepResult, _ := json.Marshal(map[string]string{"_work_item_id": parsed.WorkItemID})
				updated, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
					Status:  strPtr(domain.StepRunRecovering),
					Attempt: &newAttempt,
					Result:  &stepResult,
				})
				if err != nil {
					return false, false, fmt.Errorf("update approval step run for retry: %w", err)
				}
				runByID[sr.StepID] = updated
				return false, false, nil
			}
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
			if r.recovery != nil && recoveryTriggers != nil {
				// Defer the trigger to post-commit (see reconcileRun):
				// TriggerOnFailure opens its own transaction, and this pass
				// may hold locks on the work item (dispatchStep re-dispatches
				// a recovering step in the same pass) — invoking it here
				// would deadlock against our own transaction. The failed
				// execution is sr.WorkerExecutionID (the one that just
				// failed THIS step — not GetLatestExecutionForTask, which on
				// a shared work item could resolve to another step's run).
				*recoveryTriggers = append(*recoveryTriggers, recoveryTriggerReq{
					tenantID:     tenantID,
					workItemID:   parsed.WorkItemID,
					failedExecID: sr.WorkerExecutionID,
					stepRunID:    sr.ID,
					reason:       "step_recovery",
				})
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

// approvalConfig is the step-level configuration for APPROVAL steps.
type approvalConfig struct {
	Reviewer      string `json:"reviewer"`
	WorkerRef     string `json:"worker_ref"`
	WorkerVersion int    `json:"worker_version"`

	LoopBranch    string `json:"loop_branch"`
	MaxIterations int    `json:"max_iterations"`

	DecisionField string `json:"decision_field"`
	SuccessValue  string `json:"success_value"`
	FailureValue  string `json:"failure_value"`
}

func parseApprovalConfig(cfgJSON string) approvalConfig {
	cfg := approvalConfig{
		DecisionField: "_decision",
		SuccessValue:  "approved",
		FailureValue:  "rejected",
	}
	json.Unmarshal([]byte(cfgJSON), &cfg)
	return cfg
}

// approvalReenter creates a new chain of step runs from loop_branch to the
// approval step, supersedes the current approval run, and creates a new
// PENDING iteration. Downstream steps are blocked because the new approval
// iteration is PENDING (not SUCCEEDED).
func (r *WorkflowReconciler) approvalReenter(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, step workflow.StepWire, runs map[string]db.WorkflowStepRunRow, loopBranch string, currentIter int, now time.Time, allSteps []workflow.StepWire) error {
	nextIter := currentIter + 1

	chainIDs := chainStepIDs(allSteps, loopBranch, step.ID)
	if err := r.createChainRuns(ctx, tx, tenantID, run.ID, chainIDs, nextIter, allSteps); err != nil {
		return fmt.Errorf("approval step %q: create chain runs: %w", step.Name, err)
	}

	// Re-create the approval step itself as a PENDING run so the loop
	// re-approves before the success branch can fire: step-b depends on
	// this approval step, and the superseded "succeeded" run must not
	// satisfy that dependency (see depsSatisfied). The new run blocks
	// downstream until its re-dispatch approves.
	newApproval := db.NewID()
	newRun := db.WorkflowStepRunRow{
		ID:            newApproval,
		TenantID:      tenantID,
		WorkflowRunID: run.ID,
		StepID:        step.ID,
		StepName:      step.Name + " (loop)",
		StepKind:      domain.StepKindApproval,
		Status:        domain.StepRunPending,
		Iteration:     nextIter,
	}
	if _, err := db.CreateWorkflowStepRun(ctx, tx, newRun); err != nil {
		return fmt.Errorf("approval step %q: create re-entry run: %w", step.Name, err)
	}

	supersededResult, _ := json.Marshal(map[string]string{"_decision": "rejected", "_loop": "re-entered"})
	superseded, err := db.UpdateWorkflowStepRun(ctx, tx, tenantID, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		EndedAt:      &now,
		SupersededBy: &newApproval,
		Result:       &supersededResult,
	})
	if err != nil {
		return fmt.Errorf("approval step %q: supersede: %w", step.Name, err)
	}
	runs[step.ID] = superseded
	if err := r.enqueueStepEvent(ctx, tx, domain.WorkflowEventStepSucceeded, run, superseded); err != nil {
		return fmt.Errorf("enqueue approval step_succeeded (supersede): %w", err)
	}

	r.log.Info("approval re-entered",
		"run", run.ID, "step", step.ID, "loop_branch", loopBranch,
		"iteration", nextIter, "chain", chainIDs)
	return nil
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

// reaskRunName marks a step run created by reaskDecisionStep (a loop
// decision re-asking its reviewer for a missing decision signal). It is
// the DISCRIMINATOR for the re-ask budget: counting runs with this name
// yields how many times the reviewer was genuinely re-asked, distinct from
// ordinary loop iterations driven by explicit _decision: failure results.
const reaskRunName = "Reviewer (re-ask)"

// countReaskRuns returns how many step runs for a reviewer were created by
// reaskDecisionStep (StepName == reaskRunName). Superseded runs are counted
// too — each re-ask that happened is one attempt regardless of whether it
// was later superseded by the next re-ask.
func countReaskRuns(runs []db.WorkflowStepRunRow) int {
	n := 0
	for _, sr := range runs {
		if sr.StepName == reaskRunName {
			n++
		}
	}
	return n
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
				"Your response must end with:\n" +
				"```\nORCHICON WORKER SUMMARY: success — <summary>\n```\n" +
				"or\n" +
				"```\nORCHICON WORKER SUMMARY: failure — <summary>\n```\n" +
				"The first word (`success` or `failure`) routes the workflow. " +
				"If the work is complete and correct, use `success`. " +
				"If there are issues that need fixing, use `failure` and explain what needs fixing in the summary text."

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
		StepName:      reaskRunName,
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

// writeApprovalInitFiles writes initial .orchicon/ files when an
// APPROVAL step enters approval_pending. The files signal to downstream
// workers that human review is pending. Called within dispatchStep's
// transaction so the project query reuses the same tx.
func (r *WorkflowReconciler) writeApprovalInitFiles(ctx context.Context, tx pgx.Tx, tenantID string, run db.WorkflowRunRow, sr db.WorkflowStepRunRow, upstreamWorker, upstreamSummary string, upstreamFiles []string) {
	if run.ProjectID == "" {
		return
	}
	proj, err := db.GetProject(ctx, tx, tenantID, run.ProjectID)
	if err != nil {
		r.log.Warn("write approval init files: get project", "project", run.ProjectID, "error", err)
		return
	}
	if proj.ProjectDir == "" {
		return
	}

	orchDir := filepath.Join(proj.ProjectDir, ".orchicon", sr.WorkflowRunID)
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		r.log.Warn("write approval init files: mkdir", "dir", orchDir, "error", err)
		return
	}

	writeFile := func(name, content string) {
		if err2 := os.WriteFile(filepath.Join(orchDir, name), []byte(content), 0644); err2 != nil {
			r.log.Warn("write approval init file", "file", name, "error", err2)
		}
	}

	writeFile("worker", "human_approval")
	writeFile("status", "pending")
	writeFile("summary", upstreamSummary)
}
