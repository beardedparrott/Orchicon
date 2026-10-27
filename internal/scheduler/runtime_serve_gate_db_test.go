package scheduler

// DB-backed regression test for the adapter-aware runtime-serve gate
// (the observed bug: an orchicon-only run failed at start with "runtime
// opencode serve failed to become usable" and sat "waiting for dispatch"
// until the serve deadline expired). The intended full-run shape lives
// here, parked behind an explicit skip until a full-run seeding helper
// exists; the load-bearing logic it would exercise is pinned by
// TestImageForRun + TestRunNeedsServeAdapterKinds (runtime_serve_gate_test.go)
// and the standing parity suite (internal/orchicon/parity_matrix_test.go).
//
// Skipped without ORCHICON_TEST_DSN (approvalTestPool).

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

type failingLifecycle struct{ stubLifecycle }

func (failingLifecycle) EnsureServing(context.Context, db.WorkflowRunRow, bool) error {
	// Mirror the observed failure: the serve never becomes usable.
	return errServeNeverUsable
}

// errServeNeverUsable mirrors the operator-visible serve-gate failure.
var errServeNeverUsable = errorfServe("runtime opencode serve failed to become usable (stub)")

func errorfServe(msg string) error { return fmtServeError{msg} }

type fmtServeError struct{ msg string }

func (e fmtServeError) Error() string { return e.msg }

// TestNativeOnlyRunSkipsServeGate arms + reconciles a native-only run
// under a runtime daemon that ALWAYS fails the serve: the run must arm
// with RuntimeReady=true, carry the resolved image (never the legacy
// no-serve sentinel — always-container), and progress past the gate
// WITHOUT the serve probe ever running (failingLifecycle would have
// failed it at start). This is the end-to-end shape of the adapter-aware
// gate that keeps local/native mode cheap.
func TestNativeOnlyRunSkipsServeGate(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed serve-gate test")
	}

	ctx := context.Background()
	pool := approvalTestPool(t)

	r := &WorkflowReconciler{
		pool: pool, runtime: failingLifecycle{},
		log: slog.New(slog.DiscardHandler), warming: map[string]bool{},
	}

	// The step ref must resolve to a PUBLISHED worker version whose model ref
	// is a native (serve-less) one: runNeedsServe is deliberately
	// CONSERVATIVE about an unresolvable ref (assume a serve), which is the
	// production rule this test is not about.
	ttxW, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ToLower(db.NewID())[:12]
	nativeWorker, err := db.CreateWorker(ctx, ttxW.Tx, db.WorkerRow{
		ID: "w-" + suffix, TenantID: approvalTestTenant,
		Name: "native-" + suffix[:8], Slug: "w-" + suffix,
		Status: domain.WorkerPublished,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkerVersion(ctx, ttxW.Tx, db.WorkerVersionRow{
		ID: db.NewID(), TenantID: approvalTestTenant, WorkerID: nativeWorker.ID, Version: 1,
		Status: domain.WorkerVersionPublished, ModelRef: "orchicon/deepseek/deepseek-v4-flash",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttxW.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	wfID := seedPublishedWorkflowSteps(t, pool, "prj_dev", `[
		{"id":"w1","name":"Native","kind":"task","ref":"`+nativeWorker.ID+`","depends_on":[]}
	]`)
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: "prj_dev",
		RunContext: []byte("{}"),
		WorkflowID: wfID, WorkflowVersion: 1, Status: domain.WorkflowRunPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileRun(ctx, approvalTestTenant, run.ID); err != nil {
		t.Fatalf("reconcileRun: %v", err)
	}
	ttx2, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx2.Rollback(ctx)
	got, err := db.GetWorkflowRun(ctx, ttx2.Tx, approvalTestTenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.WorkflowRunRunning {
		t.Fatalf("run status = %q, want running (run failed at the serve gate)", got.Status)
	}
	// The LIVE fallback (#849): the chain no longer returns the placeholder constant, so the
	// stamped image is the daemon's real base tag.
	if got.RuntimeImage != db.RuntimeBaseImage() {
		t.Errorf("runtime_image = %q, want the live fallback %q (always-container: resolved image, never the sentinel)", got.RuntimeImage, db.RuntimeBaseImage())
	}
	if !got.RuntimeReady {
		t.Error("runtime_ready = false, want true (native-only run must not gate)")
	}
}
