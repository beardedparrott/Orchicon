package scheduler

// runtime_reap_sweep_test.go — A TERMINAL RUN MUST NOT LEAK ITS CONTAINER.
//
// Observed on a live host: an ABORTED run's container was still `running` while a
// COMPLETED run beside it had been returned to the pool ("pool release: resetting
// run container" appeared for one and not the other).
//
// Cause: the reconcile-path reaper only sees runs the scan returns, and the scan
// returns NON-TERMINAL runs only (ListPendingWorkflowRuns: pending/running/paused).
// A run that reached `aborted` therefore never arrives at the branch that reaps
// it, and AbortWorkflow does not reap either. The sweep this file tests closes
// that by looking at terminal runs directly.
//
// THESE TESTS BUILD THEIR OWN ROWS. An earlier version skipped when the scratch
// DB happened to hold no terminal runs, which proves nothing at all — a test that
// can silently skip is the failure mode this repository keeps paying for.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// recordingReaper records ReapForRun calls and can fail on demand.
type recordingReaper struct {
	stubLifecycle
	reaped []string
	failOn map[string]error
}

func (r *recordingReaper) ReapForRun(_ context.Context, runID string) error {
	if r.failOn != nil {
		if err, ok := r.failOn[runID]; ok {
			return err
		}
	}
	r.reaped = append(r.reaped, runID)
	return nil
}

// TestTheSweepReapsEveryTerminalRunItFinds is the direct regression: terminal
// runs are created here, and every one of them must reach ReapForRun.
func TestTheSweepReapsEveryTerminalRunItFinds(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	tenant := approvalTestTenant

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_sweep_a", "sweep-a", db.ExecutionModeRuntime)

	var want []string
	for _, st := range []string{domain.WorkflowRunAborted, domain.WorkflowRunFailed, domain.WorkflowRunCompleted} {
		id := "run_sweep_" + st
		if _, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: id, TenantID: tenant, WorkflowID: "wf-sweep", WorkflowVersion: 1,
			ProjectID: prj.ID, Status: st, RuntimeImage: "orchicon-runtime:orchicon-dev",
			RunContext: []byte("{}"), StartedAt: timePtr(time.Now()),
		}); err != nil {
			t.Fatalf("create %s run: %v", st, err)
		}
		want = append(want, id)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Only the runs this test created matter; the scratch DB is shared.
	rl := &recordingReaper{}
	r := &WorkflowReconciler{pool: pool, runtime: rl, log: slog.Default()}
	r.sweepTerminalRunContainers(ctx, tenant)

	got := map[string]bool{}
	for _, id := range rl.reaped {
		got[id] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("terminal run %q was NOT reaped — its container would leak, which is the whole "+
				"defect: a run terminalised outside the reconciler never reaches the reconcile-path "+
				"reaper (swept: %v)", id, rl.reaped)
		}
	}
}

// TestSweepSkipsRunsWithNoRuntimeEvidence: a run that never had a container must
// not be handed to the reaper at all. The predicate is the guard that keeps the
// sweep from poking the daemon for every run the tenant ever finished.
func TestSweepSkipsRunsWithNoRuntimeEvidence(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	tenant := approvalTestTenant

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_sweep_b", "sweep-b", db.ExecutionModeLocal)

	const localRun = "run_sweep_local_only"
	if _, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
		ID: localRun, TenantID: tenant, WorkflowID: "wf-sweep", WorkflowVersion: 1,
		ProjectID: prj.ID, Status: domain.WorkflowRunCompleted,
		// No runtime image, runtime_ready false: a local run that never had a container.
		RunContext: []byte("{}"), StartedAt: timePtr(time.Now()),
	}); err != nil {
		t.Fatalf("create local run: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rl := &recordingReaper{}
	r := &WorkflowReconciler{pool: pool, runtime: rl, log: slog.Default()}
	r.sweepTerminalRunContainers(ctx, tenant)

	for _, id := range rl.reaped {
		if id == localRun {
			t.Errorf("a run with NO runtime evidence (%q) was handed to the reaper — the predicate "+
				"must exclude runs that never had a container", id)
		}
	}
}

// TestSweepIsANoOpWithoutARuntime pins the defensive guard: a headless plane (no
// runtime wired) must not attempt to reap anything, and must not panic on a nil
// pool.
func TestSweepIsANoOpWithoutARuntime(t *testing.T) {
	r := &WorkflowReconciler{pool: nil, log: slog.Default()}
	r.sweepTerminalRunContainers(context.Background(), "tnt_dev")
}
