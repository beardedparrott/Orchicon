package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// These tests exercise the recovery-dispatch gate against a real
// Postgres. They are skipped unless ORCHICON_TEST_DSN points at a
// disposable database (the migrations + dev workers are applied on every
// run, so the database must be safe to re-seed):
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run TestRecoveryDispatchGate -v
//
// They guard the acceptance criteria for the recovery-seed race fix: a
// recovering summarize_restart step is re-dispatched only after its
// recovery is terminal `resumed` AND a seed is resolvable — never in the
// same pass that flips it recovering (before the recovery row exists) and
// never cold.

// recordingDispatcher is a TaskDispatcher stub that counts the step run ids
// it was asked to dispatch.
type recordingDispatcher struct {
	mu       sync.Mutex
	stepRuns []string
}

func (d *recordingDispatcher) DispatchTask(ctx context.Context, taskID, stepRunID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stepRuns = append(d.stepRuns, stepRunID)
	return nil
}

func (d *recordingDispatcher) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.stepRuns)
}

// recoveryGateTestEnv is the shared fixture: project, ticket, run, the
// dead (failed) execution, and a recovering step run carrying the
// dead-execution identity + worker pin + recovery strategy.
type recoveryGateTestEnv struct {
	t          *testing.T
	pool       *db.Pool
	reconciler *WorkflowReconciler
	projectID  string
	ticketID   string
	run        db.WorkflowRunRow
	exec       db.ExecutionRow
	step       workflow.StepWire
	stepRun    db.WorkflowStepRunRow
}

func newRecoveryGateTestEnv(t *testing.T, strategy string) *recoveryGateTestEnv {
	t.Helper()
	pool := approvalTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	env := &recoveryGateTestEnv{t: t, pool: pool}
	env.reconciler = NewWorkflowReconciler(pool, logger, nil, nil, nil, nil)

	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	slug := "recovery-gate-" + db.NewID()
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Recovery Gate Test", Slug: slug,
		Status: "active", Goals: []byte("[]"),
		ProjectDir: "/tmp/orchicon/" + slug,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	env.projectID = proj.ID

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: proj.ID, Kind: domain.WorkItemKindTask,
		Title:              "The stalled ticket",
		Description:        "work that stalled and was recovered",
		AcceptanceCriteria: "done correctly",
		Status:             domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	env.ticketID = ticket.ID

	wfID := "wf-recovery-gate-" + db.NewID()
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, WorkflowVersion: 1,
		ProjectID: proj.ID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	// CreateWorkflowRun does not insert runtime_ready (column default false);
	// flip it true explicitly so reconcileRun's headless runtime gate does
	// not bump the run version mid-pass (which would stale the optimistic
	// lock at the terminal-state update).
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady: boolPtr(true),
	}); err != nil {
		t.Fatalf("set run runtime_ready: %v", err)
	}
	run, err = db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	env.run = run

	// The dead execution: the execution that just failed THIS step.
	now := time.Now().UTC()
	exec, err := db.CreateExecution(ctx, ttx.Tx, db.ExecutionRow{
		ID:             db.NewID(),
		TenantID:       approvalTestTenant,
		ProjectID:      proj.ID,
		TaskID:         ticket.ID,
		WorkerID:       "w_se_devops_engineer",
		WorkerVersion:  1,
		Status:         domain.ExecutionFailed,
		HealthState:    domain.HealthHealthy,
		StartedAt:      &now,
		WorkflowRunID:  run.ID,
		WorkflowStepID: "step-task",
	})
	if err != nil {
		t.Fatalf("create failed execution: %v", err)
	}
	env.exec = exec

	env.step = workflow.StepWire{
		ID: "step-task", Name: "Task", Kind: domain.StepKindTask,
		Ref: "w_se_devops_engineer", WorkerVersion: 1,
		Config: `{"recovery":{"strategy":"summarize_restart","max_attempts":3}}`,
	}

	// reconcileRun requires a published workflow version to drive the DAG —
	// create the workflow + version (with this step) and publish it.
	if _, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: wfID, TenantID: approvalTestTenant,
		ProjectID: proj.ID, Name: "Recovery Gate", CurrentVersion: 1,
		Status: "active", Type: "template",
	}); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	stepsJSON, err := json.Marshal([]workflow.StepWire{env.step})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowID: wfID, Version: 1,
		VersionNote: "test", Status: "draft",
		Steps: stepsJSON, Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatalf("create workflow version: %v", err)
	}
	if _, err := db.PublishWorkflowVersion(ctx, ttx.Tx, approvalTestTenant, wfID, 1); err != nil {
		t.Fatalf("publish workflow version: %v", err)
	}

	// Recovering step run carrying the dead-execution identity + strategy
	// (what pollTaskStep's summarize_restart branch writes today) plus the
	// worker pin preserved from the first dispatch (what the gate needs to
	// resolve the seed for the exact dispatching worker).
	stepResult := recoveringStepResult(ctx, ttx.Tx, approvalTestTenant, ticket.ID, exec.ID, strategy, nil)
	var srRes map[string]any
	_ = json.Unmarshal(stepResult, &srRes)
	srRes["_worker_id"] = "w_se_devops_engineer"
	srRes["_worker_version"] = float64(1)
	withWorker, _ := json.Marshal(srRes)
	sr, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		WorkflowRunID: run.ID, StepID: env.step.ID, StepName: env.step.Name,
		StepKind: env.step.Kind, Status: domain.StepRunRecovering,
		Attempt:           1,
		Result:            withWorker,
		WorkerExecutionID: exec.ID,
	})
	if err != nil {
		t.Fatalf("create recovering step run: %v", err)
	}
	env.stepRun = sr

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
	return env
}

// getStepRun reloads the step run by id.
func (env *recoveryGateTestEnv) getStepRun(ctx context.Context, id string) db.WorkflowStepRunRow {
	env.t.Helper()
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		env.t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, id)
	if err != nil {
		env.t.Fatalf("get step run: %v", err)
	}
	return sr
}

// TestRecoveryDispatchGateRace is THE regression test for the observed
// race: a recovering summarize_restart step must NOT be re-dispatched
// before its recovery is terminal `resumed` AND a seed is resolvable. It
// replays the exact sequence from the field incident: step recovering →
// recovery row absent (same-pass race) → active recovery → resumed. Only
// the terminal-resumed state unlocks dispatch, and the TaskReconciler's
// file gate then writes+verifies .orchicon/worker.recovery before the
// session starts (covered by the seedRecoveryFile unit tests).
func TestRecoveryDispatchGateRace(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	ctx := context.Background()

	// The recovering step run must carry the dead-execution identity that
	// survives re-dispatch (WorkerExecutionID gets cleared).
	sr := env.getStepRun(ctx, env.stepRun.ID)
	var meta struct {
		FailedExecID     string `json:"_failed_execution_id"`
		RecoveryStrategy string `json:"_recovery_strategy"`
	}
	if err := json.Unmarshal(sr.Result, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.FailedExecID != env.exec.ID {
		t.Fatalf("step run must record _failed_execution_id=%s, got %q", env.exec.ID, meta.FailedExecID)
	}
	if meta.RecoveryStrategy != "summarize_restart" {
		t.Fatalf("step run must record _recovery_strategy=summarize_restart, got %q", meta.RecoveryStrategy)
	}

	// Phase 1: NO recovery row yet — the same-pass race. The gate must HOLD
	// the dispatch (old behavior created the replacement execution here,
	// ~200ms before the engine published the seed keys).
	disp := &recordingDispatcher{}
	env.reconciler.taskDispatcher = disp
	if err := env.reconciler.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (no recovery row): %v", err)
	}
	if disp.calls() != 0 {
		t.Fatalf("phase 1: dispatch fired %d time(s) with NO recovery row — the race is back", disp.calls())
	}
	sr = env.getStepRun(ctx, env.stepRun.ID)
	if sr.Status != domain.StepRunRecovering {
		t.Fatalf("phase 1: step status = %q, want %q (held)", sr.Status, domain.StepRunRecovering)
	}

	// Phase 2: recovery row created (pending — active). Still held.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID:                 db.NewID(),
		TenantID:           approvalTestTenant,
		ProjectID:          env.projectID,
		TaskID:             env.ticketID,
		FailedExecutionID:  env.exec.ID,
		RecoveryWorkflowID: "wf-recovery",
		TriggerReason:      "step_recovery",
		Level:              1,
		Status:             domain.RecoveryPending,
		CurrentStep:        "capture",
		Summary:            "dead session summarized",
	})
	if err != nil {
		t.Fatalf("create recovery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.reconciler.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (active recovery): %v", err)
	}
	if disp.calls() != 0 {
		t.Fatalf("phase 2: dispatch fired with an ACTIVE recovery — must hold (got %d dispatches)", disp.calls())
	}

	// Phase 3: recovery becomes terminal `resumed` AND the seed keys are
	// published on the step run (the engine's resume branch does both
	// atomically — engine.go:811-836). The gate must unlock and the
	// dispatcher must fire exactly once.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, approvalTestTenant, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
		Status:  strPtr(domain.RecoveryResumed),
		EndedAt: &now,
	}); err != nil {
		t.Fatalf("flip recovery to resumed: %v", err)
	}
	sr = env.getStepRun(ctx, env.stepRun.ID)
	merged := map[string]any{}
	_ = json.Unmarshal(sr.Result, &merged)
	merged["_recovery_summary"] = "dead session summarized"
	merged["_recovery_execution_id"] = env.exec.ID
	merged["_recovery_worker_id"] = "w_se_devops_engineer"
	// Same-version pin (fixture step is _worker_version 1): the R7b
	// version check must pass and the dispatcher must fire exactly once.
	merged["_recovery_worker_version"] = float64(1)
	merged["_recovery_adapter"] = "test-adapter"
	mergedJSON, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{
		Result: &mergedJSON,
	}); err != nil {
		t.Fatalf("publish seed keys: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := env.reconciler.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (resumed + keys published): %v", err)
	}
	if disp.calls() != 1 {
		t.Fatalf("phase 3: dispatch fired %d time(s), want exactly 1 after terminal-resumed + resolvable seed", disp.calls())
	}
	// The re-dispatched step must have its WorkerExecutionID cleared (the
	// inline DispatchTask links the replacement) but keep the seed keys.
	sr = env.getStepRun(ctx, env.stepRun.ID)
	if sr.WorkerExecutionID != "" {
		t.Errorf("re-dispatched step must clear WorkerExecutionID, got %q", sr.WorkerExecutionID)
	}
	var after map[string]any
	_ = json.Unmarshal(sr.Result, &after)
	if after["_recovery_execution_id"] != env.exec.ID {
		t.Errorf("re-dispatched step run must PRESERVE the seed keys")
	}
}

// TestRecoveryDispatchGateRetryStrategy verifies the retry-clone flow is
// untouched: a recovering step with strategy retry dispatches immediately
// even with no recovery row (it is not engine-driven).
func TestRecoveryDispatchGateRetryStrategy(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "retry")
	ctx := context.Background()
	disp := &recordingDispatcher{}
	env.reconciler.taskDispatcher = disp
	if err := env.reconciler.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile (retry strategy): %v", err)
	}
	if disp.calls() != 1 {
		t.Fatalf("retry strategy must dispatch immediately (got %d dispatches)", disp.calls())
	}
}

// TestRecoveryDispatchGateTerminalFailed verifies a terminal NON-resumed
// recovery (failed/cancelled) fails the step loud instead of holding or
// dispatching cold.
func TestRecoveryDispatchGateTerminalFailed(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	ctx := context.Background()

	// Create the recovery row already terminal-failed.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID:                 db.NewID(),
		TenantID:           approvalTestTenant,
		ProjectID:          env.projectID,
		TaskID:             env.ticketID,
		FailedExecutionID:  env.exec.ID,
		RecoveryWorkflowID: "wf-recovery",
		TriggerReason:      "step_recovery",
		Level:              1,
		Status:             domain.RecoveryFailed,
		CurrentStep:        "summarize",
		EndedAt:            &now,
	}); err != nil {
		t.Fatalf("create failed recovery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	disp := &recordingDispatcher{}
	env.reconciler.taskDispatcher = disp
	if err := env.reconciler.reconcileRun(ctx, approvalTestTenant, env.run.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if disp.calls() != 0 {
		t.Fatalf("terminal-failed recovery must NOT dispatch, got %d dispatches", disp.calls())
	}
	sr := env.getStepRun(ctx, env.stepRun.ID)
	if sr.Status != domain.StepRunFailed {
		t.Fatalf("terminal-failed recovery must fail the step loud, got status %q", sr.Status)
	}
}

// TestRecoveryGatePredicateTruthTable exercises recoveryDispatchReady
// directly over the full truth table without a full reconcile pass.
func TestRecoveryGatePredicateTruthTable(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	ctx := context.Background()

	sr := env.getStepRun(ctx, env.stepRun.ID)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	// No recovery row → hold.
	ready, fail := env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, env.run, sr)
	if ready || fail != nil {
		t.Fatalf("no recovery row: (ready=%v, fail=%v), want (false, nil)", ready, fail)
	}

	// Active recovery → hold.
	rec, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID:                 db.NewID(),
		TenantID:           approvalTestTenant,
		ProjectID:          env.projectID,
		TaskID:             env.ticketID,
		FailedExecutionID:  env.exec.ID,
		RecoveryWorkflowID: "wf-recovery",
		TriggerReason:      "step_recovery",
		Level:              1,
		Status:             domain.RecoveryRunning,
		CurrentStep:        "capture",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, fail = env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, env.run, sr)
	if ready || fail != nil {
		t.Fatalf("active recovery: (ready=%v, fail=%v), want (false, nil)", ready, fail)
	}

	// Terminal resumed + seed keys published on the step run → ready.
	now := time.Now().UTC()
	if _, err := db.UpdateRecoveryExecution(ctx, ttx.Tx, approvalTestTenant, rec.ID, rec.Version, db.UpdateRecoveryExecutionFields{
		Status:  strPtr(domain.RecoveryResumed),
		EndedAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	sr2 := env.getStepRun(ctx, env.stepRun.ID)
	merged := map[string]any{}
	_ = json.Unmarshal(sr2.Result, &merged)
	merged["_recovery_execution_id"] = env.exec.ID
	merged["_recovery_worker_id"] = "w_se_devops_engineer"
	merged["_recovery_summary"] = "dead session summarized"
	mergedJSON, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr2.ID, sr2.Version, db.UpdateWorkflowStepRunFields{
		Result: &mergedJSON,
	}); err != nil {
		t.Fatal(err)
	}
	sr3 := env.getStepRun(ctx, env.stepRun.ID)
	ready, fail = env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, env.run, sr3)
	if !ready || fail != nil {
		t.Fatalf("resumed + seed keys: (ready=%v, fail=%v), want (true, nil)", ready, fail)
	}

	// Terminal failed recovery → fail the step loud, not dispatch cold.
	if _, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID:                 db.NewID(),
		TenantID:           approvalTestTenant,
		ProjectID:          env.projectID,
		TaskID:             env.ticketID,
		FailedExecutionID:  env.exec.ID,
		RecoveryWorkflowID: "wf-recovery",
		TriggerReason:      "step_recovery",
		Level:              1,
		Status:             domain.RecoveryFailed,
		CurrentStep:        "summarize",
		EndedAt:            &now,
	}); err != nil {
		t.Fatal(err)
	}
	sr4 := env.getStepRun(ctx, env.stepRun.ID)
	ready, fail = env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, env.run, sr4)
	if ready {
		t.Fatal("terminal failed recovery must not dispatch")
	}
	if fail == nil || !strings.Contains(fail.Error(), "recovery did not resume") {
		t.Fatalf("terminal failed recovery must fail the step with a clear reason, got %v", fail)
	}
}

// TestRecoveryGateWorkerChangeFailsFast is AC#8 (R7): a recovery-resumed
// step whose dead-execution worker differs from the currently-assigned
// worker must FAIL the step loud with a clear "worker changed" reason —
// never wedge in the gate trying to resolve a seed for a worker that can
// never write it. Reassignment (or a worker delete/version bump) between
// execute-fail and resume-dispatch must re-arbitrate via retry/fail-fast,
// not hold forever on "recovery seed never became resolvable".
func TestRecoveryGateWorkerChangeFailsFast(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	ctx := context.Background()

	// The dead execution ran on w_se_devops_engineer; the step has since
	// been REASSIGNED to w_se_qa_engineer. Rewrite the step run's worker
	// pin to the new worker, leaving _failed_worker_id (the dead exec's
	// worker) behind so the R7 check sees a mismatch.
	sr := env.getStepRun(ctx, env.stepRun.ID)
	merged := map[string]any{}
	_ = json.Unmarshal(sr.Result, &merged)
	if merged["_failed_worker_id"] != "w_se_devops_engineer" {
		t.Fatalf("fixture must pin _failed_worker_id to the dead exec worker, got %v", merged["_failed_worker_id"])
	}
	merged["_worker_id"] = "w_se_qa_engineer"
	merged["_worker_version"] = float64(2)
	mj, _ := json.Marshal(merged)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{Result: &mj}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("reassign step worker: %v", err)
	}

	// A terminal `resumed` recovery must exist so the gate reaches the R7
	// worker-change check rather than failing via "recovery did not resume".
	now := time.Now().UTC()
	if _, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: env.projectID, TaskID: env.ticketID,
		FailedExecutionID: env.exec.ID, RecoveryWorkflowID: "wf-recovery",
		TriggerReason: "step_recovery", Level: 1,
		Status: domain.RecoveryResumed, CurrentStep: "summarize", EndedAt: &now,
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create resumed recovery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit worker-change fixture: %v", err)
	}

	sr = env.getStepRun(ctx, env.stepRun.ID)
	ttx2, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	ready, fail := env.reconciler.recoveryDispatchReady(ctx, ttx2.Tx, approvalTestTenant, env.run, sr)
	if ready {
		t.Fatalf("worker-changed step must NOT dispatch against the dead worker's seed")
	}
	if fail == nil || !strings.Contains(fail.Error(), "worker changed") {
		t.Fatalf("worker-changed step must fail fast with a 'worker changed' reason, got %v", fail)
	}
}

// TestRecoveryGateWorkerVersionChangeFailsFast is R7b: a recovery-resumed
// step whose worker ID is unchanged but whose VERSION moved off the dead
// execution's version must FAIL loud with a "worker version changed"
// reason — same ID + different version may resolve a different adapter
// (model_ref is per-version: the v3-opencode vs v4-orchicon flip that
// dispatched four executions into a dead serve). Never wedge, never
// dispatch cross-adapter.
func TestRecoveryGateWorkerVersionChangeFailsFast(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	ctx := context.Background()

	// Same worker, bumped version: the step now pins _worker_version 2
	// while the dead execution ran version 1.
	sr := env.getStepRun(ctx, env.stepRun.ID)
	merged := map[string]any{}
	_ = json.Unmarshal(sr.Result, &merged)
	if merged["_worker_version"] != float64(1) {
		t.Fatalf("fixture must pin _worker_version 1, got %v", merged["_worker_version"])
	}
	merged["_worker_version"] = float64(2)
	// Engine-published pin for the dead execution (version 1, opencode).
	merged["_recovery_summary"] = "dead session summarized"
	merged["_recovery_execution_id"] = env.exec.ID
	merged["_recovery_worker_id"] = "w_se_devops_engineer"
	merged["_recovery_worker_version"] = float64(1)
	merged["_recovery_adapter"] = "opencode"
	mj, _ := json.Marshal(merged)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{Result: &mj}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("bump step worker version: %v", err)
	}

	// A terminal `resumed` recovery must exist so the gate reaches the R7b
	// version check rather than failing via "recovery did not resume".
	now := time.Now().UTC()
	if _, err := db.CreateRecoveryExecution(ctx, ttx.Tx, db.RecoveryExecutionRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		ProjectID: env.projectID, TaskID: env.ticketID,
		FailedExecutionID: env.exec.ID, RecoveryWorkflowID: "wf-recovery",
		TriggerReason: "step_recovery", Level: 1,
		Status: domain.RecoveryResumed, CurrentStep: "summarize", EndedAt: &now,
	}); err != nil {
		_ = ttx.Rollback(ctx)
		t.Fatalf("create resumed recovery: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit version-change fixture: %v", err)
	}

	sr = env.getStepRun(ctx, env.stepRun.ID)
	ttx2, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	ready, fail := env.reconciler.recoveryDispatchReady(ctx, ttx2.Tx, approvalTestTenant, env.run, sr)
	if ready {
		t.Fatalf("version-changed step must NOT dispatch against the dead version's seed")
	}
	if fail == nil || !strings.Contains(fail.Error(), "worker version changed") {
		t.Fatalf("version-changed step must fail fast with a 'worker version changed' reason, got %v", fail)
	}
}

// ageStepRecoveringSince rewrites the recovering step run's
// `_recovering_since` to the given time (committed), for RC2 fixtures that
// must age the STEP's own recovering entry rather than the run's age.
func (env *recoveryGateTestEnv) ageStepRecoveringSince(ctx context.Context, old time.Time) db.WorkflowStepRunRow {
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		env.t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	sr := env.getStepRun(ctx, env.stepRun.ID)
	merged := map[string]any{}
	_ = json.Unmarshal(sr.Result, &merged)
	merged["_recovering_since"] = old.Format(time.RFC3339Nano)
	mj, _ := json.Marshal(merged)
	if _, err := db.UpdateWorkflowStepRun(ctx, ttx.Tx, approvalTestTenant, sr.ID, sr.Version, db.UpdateWorkflowStepRunFields{Result: &mj}); err != nil {
		env.t.Fatalf("age step recovering since: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		env.t.Fatalf("commit aged step: %v", err)
	}
	return env.getStepRun(ctx, env.stepRun.ID)
}

// TestRecoveringStallGuardFailsStuckStep is the regression test for the
// observed 45-minute "running" limbo: a recovering summarize_restart step
// whose recovery row never materializes sits in the recovery-dispatch gate
// forever (no active recovery, no recovery at all), so the run never
// finalizes and the Retry button never surfaces. The recoveringStallTimeout
// cap must FAIL the stuck step terminal — keyed off the STEP's own
// `_recovering_since` timestamp (RC2), NEVER run.StartedAt.
func TestRecoveringStallGuardFailsStuckStep(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	t.Setenv("ORCHICON_RECOVERING_STALL_TIMEOUT", "1m")
	ctx := context.Background()

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// RC2 — the core regression: a LONG-RUNNING run (StartedAt well past the
	// stall window) whose recovery trigger has NOT landed yet must HOLD, not
	// fail. The step run carries a FRESH _recovering_since; run age is
	// irrelevant to the guard.
	agedRun := env.run
	oldRun := time.Now().UTC().Add(-2 * time.Hour)
	agedRun.StartedAt = &oldRun
	ready, fail := env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, agedRun, env.stepRun)
	if ready || fail != nil {
		t.Fatalf("long-running run + FRESH step ts: (ready=%v, fail=%v), want (false, nil) HOLD — run age must not fail the step", ready, fail)
	}

	// The genuinely-stuck case: age the STEP's own recovering timestamp past
	// the cap. The same stuck step must be FAILED terminal instead of
	// holding a run "running" indefinitely.
	stuckSR := env.ageStepRecoveringSince(ctx, time.Now().UTC().Add(-2*time.Hour))
	ready, fail = env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, env.run, stuckSR)
	if ready {
		t.Fatal("aged recovering step MUST NOT dispatch")
	}
	if fail == nil || !strings.Contains(fail.Error(), "stuck recovering") {
		t.Fatalf("aged recovering step must fail with a clear 'stuck recovering' reason, got %v", fail)
	}
}

// TestRecoveringStallDisabledHoldsLegacy verifies the cap can be disabled
// (ORCHICON_RECOVERING_STALL_TIMEOUT < 1s): the gate returns to legacy
// hold-only behavior even for a genuinely aged recovering step.
func TestRecoveringStallDisabledHoldsLegacy(t *testing.T) {
	env := newRecoveryGateTestEnv(t, "summarize_restart")
	t.Setenv("ORCHICON_RECOVERING_STALL_TIMEOUT", "0ms")
	ctx := context.Background()

	agedSR := env.ageStepRecoveringSince(ctx, time.Now().UTC().Add(-2*time.Hour))
	agedRun := env.run
	old := time.Now().UTC().Add(-2 * time.Hour)
	agedRun.StartedAt = &old

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	ready, fail := env.reconciler.recoveryDispatchReady(ctx, ttx.Tx, approvalTestTenant, agedRun, agedSR)
	if ready {
		t.Fatal("disabled cap + aged run must not dispatch")
	}
	if fail != nil {
		t.Fatalf("disabled cap must hold legacy (false, nil), got fail=%v", fail)
	}
}
