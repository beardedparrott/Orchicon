package scheduler

// Per-worker concurrency gate tests (execution reliability part 2): the
// resolved worker version's concurrency_limit is now enforced in
// reconcileOne's capacity block, mirroring the project gate in
// dispatch_limits_test.go. DB-backed; skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestWorkerConcurrencyGate' -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// seedGateWorker creates a published worker + version pinned to the
// given concurrency_limit, for exercising the gate in isolation from the
// seeded dev workers (which are all limit=0).
func seedGateWorker(t *testing.T, pool *db.Pool, concurrencyLimit int) string {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	suffix := strings.ToLower(db.NewID())[:12]
	w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: "w-gate-" + suffix, TenantID: approvalTestTenant,
		Name: "gate-worker-" + suffix[:8], Slug: "w-gate-" + suffix,
		Status: domain.WorkerPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkerID: w.ID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "anthropic/claude-sonnet-4",
		ConcurrencyLimit: concurrencyLimit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx := context.Background()
		cttx, cerr := pool.BeginTenantTx(cctx, approvalTestTenant)
		if cerr != nil {
			return
		}
		defer cttx.Rollback(cctx)
		_, _ = cttx.Exec(cctx, `DELETE FROM worker_executions WHERE tenant_id = $1 AND worker_id = $2`, approvalTestTenant, w.ID)
		_ = db.DeleteWorker(cctx, cttx.Tx, approvalTestTenant, w.ID)
		_ = cttx.Commit(cctx)
	})
	return w.ID
}

// seedGateTask creates a ready standalone task pinned to the given
// worker via assigned_worker_ref (the selectWorker path).
func seedGateTask(t *testing.T, pool *db.Pool, projectID, workerID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	wfID := seedPublishedWorkflow(t, pool, projectID)
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	ref, _ := json.Marshal(map[string]any{"worker_id": workerID, "version": 1})
	created, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Worker Gate Task " + db.NewID()[:6],
		Status:            domain.WorkItemReady,
		AssignedWorkerRef: ref,
		WorkflowID:        &wfID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return created
}

// TestWorkerConcurrencyGateHoldsSecondUntilSlotFrees exercises the
// standalone (selectWorker) path: with the resolved worker version's
// concurrency_limit=1, a second task pinned to the SAME worker is held
// (no execution, task stays ready) until the first execution reaches a
// terminal state.
func TestWorkerConcurrencyGateHoldsSecondUntilSlotFrees(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	workerID := seedGateWorker(t, env.pool, 1)
	ctx := context.Background()
	first := seedGateTask(t, env.pool, env.proj.ID, workerID)
	second := seedGateTask(t, env.pool, env.proj.ID, workerID)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))

	if err := rec.reconcileOne(ctx, first.ID, ""); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if got := mustGet(t, env.pool, first.ID); got.Status != domain.WorkItemAssigned {
		t.Fatalf("first task status = %q, want assigned", got.Status)
	}
	if err := rec.reconcileOne(ctx, second.ID, ""); err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	if got := mustGet(t, env.pool, second.ID); got.Status != domain.WorkItemReady {
		t.Fatalf("second task status = %q, want ready (held at worker cap)", got.Status)
	}

	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	secondExecs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID: approvalTestTenant, ProjectID: env.proj.ID, TaskID: second.ID,
	})
	firstExecs, ferr := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID: approvalTestTenant, ProjectID: env.proj.ID, TaskID: first.ID,
	})
	_ = ttx.Rollback(ctx)
	if err != nil || ferr != nil {
		t.Fatal(err, ferr)
	}
	if len(secondExecs) != 0 {
		t.Fatalf("second task executions = %d, want 0 (held at worker cap)", len(secondExecs))
	}
	if len(firstExecs) != 1 {
		t.Fatalf("first task executions = %d, want 1", len(firstExecs))
	}

	// Free the slot: the first execution reaches a terminal state.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, firstExecs[0].ID, firstExecs[0].Version, db.UpdateExecutionFields{
		Status: strPtr(domain.ExecutionSucceeded),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := rec.reconcileOne(ctx, second.ID, ""); err != nil {
		t.Fatalf("dispatch second after slot freed: %v", err)
	}
	if got := mustGet(t, env.pool, second.ID); got.Status != domain.WorkItemAssigned {
		t.Fatalf("second task status after slot freed = %q, want assigned", got.Status)
	}
}

// TestWorkerConcurrencyGateUnlimitedNeverDefers covers AC3 explicitly over
// BOTH paths: concurrency_limit=0 never defers on the standalone
// (selectWorker) path nor on the step (workerVersionForStepRun) path.
func TestWorkerConcurrencyGateUnlimitedNeverDefers(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	workerID := seedGateWorker(t, env.pool, 0)
	ctx := context.Background()
	first := seedGateTask(t, env.pool, env.proj.ID, workerID)
	second := seedGateTask(t, env.pool, env.proj.ID, workerID)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))

	// Standalone path: both admit immediately.
	if err := rec.reconcileOne(ctx, first.ID, ""); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if err := rec.reconcileOne(ctx, second.ID, ""); err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	if got := mustGet(t, env.pool, first.ID); got.Status != domain.WorkItemAssigned {
		t.Fatalf("first task status = %q, want assigned", got.Status)
	}
	if got := mustGet(t, env.pool, second.ID); got.Status != domain.WorkItemAssigned {
		t.Fatalf("second task status = %q, want assigned (unlimited)", got.Status)
	}

	// Step path: a third and fourth step run pinned to the same unlimited
	// worker both dispatch without being deferred.
	run, stepA, stepB, ticketID := seedGateWorkflowRunWithTwoSteps(t, env.pool, env.proj.ID, workerID)
	if err := rec.reconcileOne(ctx, ticketID, stepA.ID); err != nil {
		t.Fatalf("dispatch step A: %v", err)
	}
	if err := rec.reconcileOne(ctx, ticketID, stepB.ID); err != nil {
		t.Fatalf("dispatch step B: %v", err)
	}
	gotA := mustGetStepRun(t, env.pool, run.ID, stepA.StepID)
	gotB := mustGetStepRun(t, env.pool, run.ID, stepB.StepID)
	if gotA.WorkerExecutionID == "" {
		t.Fatalf("step A not dispatched (unlimited worker)")
	}
	if gotB.WorkerExecutionID == "" {
		t.Fatalf("step B not dispatched (unlimited worker)")
	}
}

// TestWorkerConcurrencyGateStepPathSharesTheSingleGate covers AC4/5/6: a
// workflow step whose PINNED worker (_worker_id on the step run) is
// saturated routes through the SAME gate as the standalone path — it is
// deferred, carries an attributed `_dispatch_wait` note, and the note is
// cleared once a slot frees and the step actually dispatches.
func TestWorkerConcurrencyGateStepPathSharesTheSingleGate(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	workerID := seedGateWorker(t, env.pool, 1)
	ctx := context.Background()

	// Saturate the worker with one active (non-terminal) execution via the
	// standalone path first.
	first := seedGateTask(t, env.pool, env.proj.ID, workerID)
	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	if err := rec.reconcileOne(ctx, first.ID, ""); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}

	run, stepA, _, ticketID := seedGateWorkflowRunWithTwoSteps(t, env.pool, env.proj.ID, workerID)

	if err := rec.reconcileOne(ctx, ticketID, stepA.ID); err != nil {
		t.Fatalf("dispatch step A: %v", err)
	}
	deferred := mustGetStepRun(t, env.pool, run.ID, stepA.StepID)
	if deferred.WorkerExecutionID != "" {
		t.Fatalf("step A dispatched despite saturated pinned worker")
	}
	var result map[string]any
	if err := json.Unmarshal(deferred.Result, &result); err != nil {
		t.Fatalf("unmarshal step result: %v", err)
	}
	note, _ := result["_dispatch_wait"].(string)
	if note == "" || !strings.Contains(note, "1/1") {
		t.Fatalf("_dispatch_wait = %q, want a note naming 1/1 capacity", note)
	}

	// Free the slot: the first execution reaches a terminal state.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	firstExecs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID: approvalTestTenant, ProjectID: env.proj.ID, TaskID: first.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(firstExecs) != 1 {
		t.Fatalf("first task executions = %d, want 1", len(firstExecs))
	}
	if _, err := db.UpdateExecution(ctx, ttx.Tx, approvalTestTenant, firstExecs[0].ID, firstExecs[0].Version, db.UpdateExecutionFields{
		Status: strPtr(domain.ExecutionSucceeded),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := rec.reconcileOne(ctx, ticketID, stepA.ID); err != nil {
		t.Fatalf("dispatch step A after slot freed: %v", err)
	}
	dispatched := mustGetStepRun(t, env.pool, run.ID, stepA.StepID)
	if dispatched.WorkerExecutionID == "" {
		t.Fatalf("step A not dispatched after slot freed")
	}
	var clearedResult map[string]any
	if len(dispatched.Result) > 0 {
		_ = json.Unmarshal(dispatched.Result, &clearedResult)
	}
	if _, stillWaiting := clearedResult["_dispatch_wait"]; stillWaiting {
		t.Fatalf("_dispatch_wait note survived on a dispatched step: %v", clearedResult)
	}
}

// seedGateWorkflowRunWithTwoSteps seeds a running, runtime-ready workflow
// run with two independent ready step runs, each pinned to workerID via
// `_worker_id`/`_worker_version` on the step run's result — exactly how
// the WorkflowReconciler pins a dispatched step (reconciler.go's
// workerVersionForStepRun contract).
func seedGateWorkflowRunWithTwoSteps(t *testing.T, pool *db.Pool, projectID, workerID string) (db.WorkflowRunRow, db.WorkflowStepRunRow, db.WorkflowStepRunRow, string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)

	ticket, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Worker Gate Step Ticket " + db.NewID()[:6],
		Status: domain.WorkItemRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Name:           "Worker Gate Step Workflow " + db.NewID()[:6],
		CurrentVersion: 1, Status: "published", Type: "template",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkflowVersion(ctx, ttx.Tx, db.WorkflowVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		Version: 1, Status: "published", Steps: []byte("[]"),
		Inputs: []byte("{}"), Outputs: []byte("{}"),
	}); err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowID: wf.ID,
		WorkflowVersion: 1, ProjectID: projectID, Status: domain.WorkflowRunRunning,
		RunContext: []byte("{}"), WorkItemID: ticket.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run.ID, run.Version, db.UpdateWorkflowRunFields{
		RuntimeReady:   boolPtr(true),
		WorktreeStatus: strPtr(domain.WorktreeSkipped),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, approvalTestTenant, ticket.ID, ticket.Version, db.UpdateWorkItemFields{
		WorkflowRunID: &run.ID,
	}); err != nil {
		t.Fatal(err)
	}
	pinned, _ := json.Marshal(map[string]any{"_worker_id": workerID, "_worker_version": 1})
	stepA, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-a-" + db.NewID()[:6], StepName: "Step A", StepKind: "task",
		Status: domain.StepRunReady, Result: pinned,
	})
	if err != nil {
		t.Fatal(err)
	}
	stepB, err := db.CreateWorkflowStepRun(ctx, ttx.Tx, db.WorkflowStepRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkflowRunID: run.ID,
		StepID: "step-b-" + db.NewID()[:6], StepName: "Step B", StepKind: "task",
		Status: domain.StepRunReady, Result: pinned,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	db.CleanupProject(t, pool, approvalTestTenant, projectID)
	return run, stepA, stepB, ticket.ID
}

func mustGetStepRun(t *testing.T, pool *db.Pool, runID, stepID string) db.WorkflowStepRunRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	sr, err := db.GetWorkflowStepRunByStep(ctx, ttx.Tx, approvalTestTenant, runID, stepID)
	if err != nil {
		t.Fatal(err)
	}
	return sr
}
