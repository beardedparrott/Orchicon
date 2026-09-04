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
// Skipped without ORCHICON_TEST_DSN (approvalTestPool), then again by
// design until full-run seeding lands.

import (
	"context"
	"os"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/runtime"
)

type failingLifecycle struct{ stubLifecycle }

func (failingLifecycle) EnsureServing(context.Context, db.WorkflowRunRow) error {
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
// with RuntimeReady=true, carry the NoServeImage sentinel, and progress
// past the gate (never written a serve-gate failure).
func TestNativeOnlyRunSkipsServeGate(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed serve-gate test")
	}
	t.Skip("parked: reconcileRun needs a full workflow+worker+step-run seed helper; the arm/gate contract is pinned by TestImageForRun + TestRunNeedsServeAdapterKinds")

	ctx := context.Background()
	pool := approvalTestPool(t)

	r := &WorkflowReconciler{pool: pool, runtime: failingLifecycle{}}
	wfID := seedPublishedWorkflowSteps(t, pool, "prj_dev", `[
		{"id":"w1","name":"Native","kind":"task","ref":"w-native-orchicon","depends_on":[]}
	]`)
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: "prj_dev",
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
	if got.RuntimeImage != runtime.NoServeImage {
		t.Errorf("runtime_image = %q, want sentinel %q", got.RuntimeImage, runtime.NoServeImage)
	}
	if !got.RuntimeReady {
		t.Error("runtime_ready = false, want true (native-only run must not gate)")
	}
}