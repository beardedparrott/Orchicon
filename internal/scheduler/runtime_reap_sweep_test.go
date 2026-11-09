package scheduler

// runtime_reap_sweep_test.go — A TERMINAL RUN MUST NOT LEAK ITS CONTAINER.

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
	sfx := db.NewID()[:10]
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_sweep_a_"+sfx, "sweep-a-"+sfx, db.ExecutionModeRuntime)

	var want []string
	for _, st := range []string{domain.WorkflowRunAborted, domain.WorkflowRunFailed, domain.WorkflowRunCompleted} {
		id := "run_sweep_" + st + "_" + sfx
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

// TestSweepSkipsRunsWithNoRuntimeEvidence: a run that never had a container must not be handed to
// the reaper at all.
func TestSweepSkipsRunsWithNoRuntimeEvidence(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()
	tenant := approvalTestTenant

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	sfx := db.NewID()[:10]
	prj := mkModeProject(t, ctx, ttx.Tx, "prj_sweep_b_"+sfx, "sweep-b-"+sfx, db.ExecutionModeLocal)

	localRun := "run_sweep_local_only_" + sfx
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

// TestSweepIsANoOpWithoutARuntime pins the defensive guard: a headless plane (no runtime wired)
// must not attempt to reap anything, and must not panic on a nil pool.
func TestSweepIsANoOpWithoutARuntime(t *testing.T) {
	r := &WorkflowReconciler{pool: nil, log: slog.Default()}
	r.sweepTerminalRunContainers(context.Background(), "tnt_dev")
}
