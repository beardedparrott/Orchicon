package scheduler

// local_mode_gate_loop_db_test.go — A LOCAL-MODE RUN MUST NOT SPIN THE RECONCILER FOR EVER.
//
// Reported from a real Windows host (canary 74187e1), and this is the whole shape of it:
//
//	work item  01M60945...  status=running
//	workflow run 01M64EN5... status=pending, runtime_ready=false, zero executions
//	msg="workflow run reconcile failed" error="clear runtime gate (headless): db: not found"
//	... repeated every ~200ms for 22 MINUTES (measured: 5552 occurrences)
//
// The mechanism is not a race, it is a deterministic double-update in one pass. When the container engine is
// absent, r.runtimeEnabled() is false, so the runtime-serve gate's ``needsServeGate`` collapses to false and
// the first UpdateWorkflowRun flips runtime_ready. That call is OPTIMISTIC-CONCURRENCY: it bumps ``version``.
// The second, "headless" arm then ran with the PRE-UPDATE run.Version, so its ``WHERE version = $3`` matched
// nothing and returned ErrNotFound — a plain error, so the reconciler retried, and retried, for ever.
//
// The operator consequence is the worst kind: a run that is neither progressing nor failing, with NOTHING in
// the UI to explain it. So the assertions here are about the loop, not just the write: a second reconcile pass
// must also succeed, which is what proves the version was carried forward.
//
// Skipped without ORCHICON_TEST_DSN (approvalTestPool).

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// TestLocalModeRunClearsTheGateWithoutErroring is the reported bug, end to end.
func TestLocalModeRunClearsTheGateWithoutErroring(t *testing.T) {
	ctx := context.Background()
	pool := approvalTestPool(t)

	// A LOCAL-MODE project — the configuration the operator had.
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	slug := "local-gate-" + strings.ToLower(db.NewID())
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "Local Gate", Slug: slug,
		Status: "active", Goals: []byte("[]"),
		ProjectDir:    "/tmp/orchicon/" + slug,
		ExecutionMode: db.ExecutionModeLocal,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// A PUBLISHED native worker, so runNeedsServe has something real to resolve.
	suffix := strings.ToLower(db.NewID())[:12]
	w, err := db.CreateWorker(ctx, ttx.Tx, db.WorkerRow{
		ID: "w-" + suffix, TenantID: approvalTestTenant,
		Name: "local-" + suffix[:8], Slug: "w-" + suffix,
		Status: domain.WorkerPublished,
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if _, err := db.CreateWorkerVersion(ctx, ttx.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkerID: w.ID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
	}); err != nil {
		t.Fatalf("create worker version: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflowSteps(t, pool, proj.ID, `[
		{"id":"w1","name":"Local","kind":"task","ref":"`+w.ID+`","depends_on":[]}
	]`)

	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.CreateWorkflowRun(ctx, ttx2.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: proj.ID,
		RunContext: []byte("{}"),
		WorkflowID: wfID, WorkflowVersion: 1, Status: domain.WorkflowRunPending,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := ttx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// NO RUNTIME. This is the Windows host in the report: a local-mode project with no usable engine, so
	// runtimeEnabled() is false and the gate falls through to the headless arm.
	r := &WorkflowReconciler{
		pool: pool, runtime: nil,
		log: slog.New(slog.DiscardHandler), warming: map[string]bool{},
	}

	// THE FAILURE WAS HERE: reconcileRun returned "clear runtime gate (headless): db: not found".
	if err := r.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun on a LOCAL-MODE run failed: %v\n"+
			"This is the reported hot loop — the error is retryable, so the reconciler retries for ever and "+
			"the run sits at pending with zero executions and nothing shown to the operator.", err)
	}

	// AND A SECOND PASS MUST ALSO SUCCEED. This is the assertion that pins the real defect: the version
	// carry-forward. A fix that merely suppressed the error would pass the check above and fail here.
	if err := r.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("the second reconcile pass failed: %v\n"+
			"A local run must be STABLE across passes, not merely survive the first one.", err)
	}

	// The gate is open and the run is progressing, not stuck pending.
	// COMMIT, THEN READ. Holding a read transaction open across the reconcile pins a snapshot and cannot
	// observe what the reconciler committed, which reports the pre-reconcile row and reads as "the fix did
	// nothing". The sibling tests get away with it because they read INSIDE the same tx they seeded; here the
	// writes under test happen in transactions of their own.
	ttx3, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetWorkflowRun(ctx, ttx3.Tx, approvalTestTenant, run.ID)
	_ = ttx3.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RuntimeReady {
		t.Error("runtime_ready = false after reconciling a local-mode run; the gate must be OPEN for local " +
			"mode — there is no container to wait for")
	}
	if got.Status != domain.WorkflowRunRunning {
		t.Errorf("run status = %q (ready=%v image=%q current_step=%q), want running",
			got.Status, got.RuntimeReady, got.RuntimeImage, got.CurrentStep)
	}
}
