package scheduler

// Parallel-dispatch tests for the TaskReconciler's empty-key scan pass:
// independent ready work items dispatch CONCURRENTLY (bounded in-pass
// fan-out under a semaphore) instead of one-at-a-time, while
// dependency-blocked items are never dispatched early and blocked items
// whose gate satisfies clear + dispatch in the same pass. DB-backed;
// skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestParallelScan' -v

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// purgeScanTenant deletes residue rows the scan pass is sensitive to
// before a parallel-scan test runs: every worker execution (frees adapter
// capacity the scan's selectAdapter relies on — executions created by
// earlier tests stay "dispatching" forever) and every seq-env project's
// ready/blocked work items. The test DB is disposable (migrate re-applies
// + seeds dev workers on every run) and each test seeds its own rows, so
// deleting prior residue is safe and makes the scan assertions
// deterministic.
func purgeScanTenant(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		return
	}
	_, _ = ttx.Exec(ctx, `DELETE FROM worker_executions WHERE tenant_id = $1`, approvalTestTenant)
	_ = ttx.Rollback(ctx)
	purgeScanResidue(t, pool)
}

// newParallelScanEnv seeds a project with n ready standalone tasks (each
// with the seeded dev worker assigned) plus a ready opencode adapter with
// generous capacity so a whole scan batch can dispatch. Project teardown
// is handled by newSequenceTestEnv's cleanup.
func newParallelScanEnv(t *testing.T, n int) (*sequenceTestEnv, []db.WorkItemRow) {
	t.Helper()
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	var tasks []db.WorkItemRow
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		created, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
			ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
			Kind: domain.WorkItemKindTask, Title: "Parallel Task " + db.NewID()[:6],
			Status:            domain.WorkItemReady,
			AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}
	now := time.Now().UTC()
	if _, err := db.CreateAdapter(ctx, ttx.Tx, db.AdapterRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready",
		MaxConcurrentExecutions: 64, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return env, tasks
}

// newInFlightProbe attaches the test-only dispatchOverlap hook and returns
// a function to read the peak number of reconcileOne calls observed
// in-flight during the scan fan-out.
func newInFlightProbe(rec *TaskReconciler) func() int {
	var mu sync.Mutex
	peak := 0
	rec.dispatchOverlap = func(cur int) {
		mu.Lock()
		defer mu.Unlock()
		if cur > peak {
			peak = cur
		}
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

// TestParallelScanDispatchesAllReadyInOnePass: 6 ready, dependency-free
// tasks dispatch in a SINGLE scan pass — all reach assigned with exactly
// one execution each, and the pass actually ran concurrently (peak
// in-flight dispatches ≥ 2).
func TestParallelScanDispatchesAllReadyInOnePass(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env, tasks := newParallelScanEnv(t, 6)
	ctx := context.Background()
	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	peakInFlight := newInFlightProbe(rec)

	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	for _, task := range tasks {
		if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemAssigned {
			t.Errorf("task %s status = %q, want %q (dispatched in one pass)", task.ID, got.Status, domain.WorkItemAssigned)
		}
		if n := countExecutionsForTask(t, env.pool, task.ID); n != 1 {
			t.Errorf("executions for task %s = %d, want 1", task.ID, n)
		}
	}
	if peak := peakInFlight(); peak < 2 {
		t.Errorf("peak in-flight dispatches = %d, want >= 2 (parallel dispatch)", peak)
	}
}

// TestParallelScanDispatchLimitBoundsConcurrency: with dispatchLimit=2, a
// single scan pass still dispatches all 6 ready tasks (the batch stays at
// scanBatchSize) but never runs more than 2 reconcileOne calls in flight.
func TestParallelScanDispatchLimitBoundsConcurrency(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env, tasks := newParallelScanEnv(t, 6)
	ctx := context.Background()
	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	rec.SetDispatchConcurrency(2)
	peakInFlight := newInFlightProbe(rec)

	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	for _, task := range tasks {
		if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemAssigned {
			t.Errorf("task %s status = %q, want %q (single pass)", task.ID, got.Status, domain.WorkItemAssigned)
		}
		if n := countExecutionsForTask(t, env.pool, task.ID); n != 1 {
			t.Errorf("executions for task %s = %d, want 1", task.ID, n)
		}
	}
	if peak := peakInFlight(); peak > 2 {
		t.Errorf("peak in-flight dispatches = %d, want <= 2 (dispatchLimit)", peak)
	} else if peak < 1 {
		t.Errorf("peak in-flight dispatches = %d, want >= 1", peak)
	}
}

// TestParallelScanDependencyBlockedNotDispatched: a ready item with an
// unsatisfied blocker parks as blocked and creates NO execution, even while
// a dependency-free sibling dispatches in the same parallel pass. This
// guards the "never dispatched early" invariant under concurrency.
func TestParallelScanDependencyBlockedNotDispatched(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env, tasks := newParallelScanEnv(t, 2)
	ctx := context.Background()
	blocked := tasks[0]
	ready := tasks[1]
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, blocked.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	if got := mustGet(t, env.pool, blocked.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("dependency-blocked task status = %q, want %q (never dispatched early)", got.Status, domain.WorkItemBlocked)
	}
	if n := countExecutionsForTask(t, env.pool, blocked.ID); n != 0 {
		t.Errorf("executions for blocked task = %d, want 0", n)
	}
	if got := mustGet(t, env.pool, ready.ID); got.Status != domain.WorkItemAssigned {
		t.Errorf("satisfied task status = %q, want %q", got.Status, domain.WorkItemAssigned)
	}
	if n := countExecutionsForTask(t, env.pool, ready.ID); n != 1 {
		t.Errorf("executions for satisfied task = %d, want 1", n)
	}
}

// TestParallelScanBlockedClearsAndDispatchesSamePass: a blocked item whose
// blocker already turned terminal clears to ready and dispatches in the
// same parallel pass as an unrelated ready item — dependency re-evaluation
// semantics are preserved under the fan-out.
func TestParallelScanBlockedClearsAndDispatchesSamePass(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env, tasks := newParallelScanEnv(t, 1)
	ctx := context.Background()
	ready := tasks[0]

	// A second item parked blocked, whose blocker already succeeded.
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Clearing " + db.NewID()[:6],
		Status:            domain.WorkItemBlocked,
		AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, created.ID, domain.DependencyBlocks)
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	for _, id := range []string{ready.ID, created.ID} {
		if got := mustGet(t, env.pool, id); got.Status != domain.WorkItemAssigned {
			t.Errorf("task %s status = %q, want %q", id, got.Status, domain.WorkItemAssigned)
		}
		if n := countExecutionsForTask(t, env.pool, id); n != 1 {
			t.Errorf("executions for task %s = %d, want 1", id, n)
		}
	}
}

// stubLimiter is a test ConcurrencyLimiter returning a fixed per-project
// limit map.
type stubLimiter struct {
	mu   sync.Mutex
	vals map[string]int
}

func (s *stubLimiter) Limit(_ context.Context, projectID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vals[projectID]
}

// TestEffectiveDispatchLimitPerProjectMin (no DB): the ConcurrencyLimiter
// seam — the effective per-pass limit is the minimum of the global bound
// and the positive per-project limits of the candidate items; zero project
// limits impose no restriction; the global bound clamps to [1, scanBatchSize].
func TestEffectiveDispatchLimitPerProjectMin(t *testing.T) {
	rec := &TaskReconciler{}
	ctx := context.Background()
	candidates := []db.WorkItemRow{{ProjectID: "p-a"}, {ProjectID: "p-b"}}

	// Unset → default.
	if got := rec.effectiveDispatchLimit(ctx, candidates); got != defaultDispatchConcurrency {
		t.Errorf("unset effective limit = %d, want %d", got, defaultDispatchConcurrency)
	}
	// Global bound applied alone.
	rec.SetDispatchConcurrency(8)
	if got := rec.effectiveDispatchLimit(ctx, candidates); got != 8 {
		t.Errorf("global effective limit = %d, want 8", got)
	}
	// Per-project minimum wins when a candidate's project is tighter.
	limiter := &stubLimiter{vals: map[string]int{"p-a": 3, "p-b": 0}}
	rec.SetConcurrencyLimiter(limiter)
	if got := rec.effectiveDispatchLimit(ctx, candidates); got != 3 {
		t.Errorf("per-project min = %d, want 3", got)
	}
	// A zero (unrestricted) project limit must not clamp the global bound.
	limiter.mu.Lock()
	limiter.vals["p-a"] = 0
	limiter.vals["p-b"] = 0
	limiter.mu.Unlock()
	if got := rec.effectiveDispatchLimit(ctx, candidates); got != 8 {
		t.Errorf("zero per-project limits = %d, want 8 (no restriction)", got)
	}
	// Global above the batch cap clamps down to scanBatchSize.
	rec.SetDispatchConcurrency(100)
	if got := rec.effectiveDispatchLimit(ctx, candidates); got != scanBatchSize {
		t.Errorf("clamped limit = %d, want %d", got, scanBatchSize)
	}
}

// TestSetDispatchConcurrencyClamps (no DB): the setter clamps its input to
// [1, scanBatchSize].
func TestSetDispatchConcurrencyClamps(t *testing.T) {
	rec := &TaskReconciler{}
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, 1}, {-5, 1}, {1000, scanBatchSize}, {6, 6}, {scanBatchSize, scanBatchSize},
	} {
		rec.SetDispatchConcurrency(tc.in)
		if got := rec.dispatchConcurrency; got != tc.want {
			t.Errorf("SetDispatchConcurrency(%d) → %d, want %d", tc.in, got, tc.want)
		}
	}
}
