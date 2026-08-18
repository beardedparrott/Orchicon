package scheduler

// Standalone blocked-status tests: the TaskReconciler surfaces a
// dependency-gated stall by flipping ready → blocked (persisted), clears
// it back to ready on the next pass once the gate satisfies, and NEVER
// dispatches a blocked task. DB-backed; skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon@127.0.0.1:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestStandaloneBlocked' -v

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
)

// newBlockedStandaloneEnv seeds the fixture a standalone dispatch needs:
// a project (for project_dir resolution), a task with an assigned worker,
// and a ready opencode adapter with free capacity. Project teardown is
// handled by newSequenceTestEnv's cleanup.
func newBlockedStandaloneEnv(t *testing.T) (*sequenceTestEnv, db.WorkItemRow) {
	t.Helper()
	env := newSequenceTestEnv(t)
	ctx := context.Background()

	task := db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Blocked Task",
		Status:            domain.WorkItemReady,
		AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
	}
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, task)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.CreateAdapter(ctx, ttx.Tx, db.AdapterRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Kind: "opencode", Version: "test", Endpoint: "localhost:0",
		Capabilities: []byte("{}"), Status: "ready",
		MaxConcurrentExecutions: 8, LastHeartbeatAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return env, created
}

func countExecutionsForTask(t *testing.T, pool *db.Pool, taskID string) int {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	exs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{TenantID: approvalTestTenant, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	return len(exs)
}

// deleteTestProject removes a project and everything under it (work items,
// dependencies, workflows, runs, step runs). Blocked-scan tests run against
// the shared tnt_dev DB, so each test must clean up its own project to keep
// the count of blocked/ready tasks in the tenant stable across runs.
func deleteTestProject(t *testing.T, pool *db.Pool, projectID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	_ = db.DeleteProject(ctx, ttx.Tx, approvalTestTenant, projectID)
	_ = ttx.Commit(ctx)
}

// purgeScanResidue deletes every leftover seq-env fixture project (the name
// newSequenceTestEnv assigns) from the shared test tenant. The blocked-scan
// tests assert that the reconciler's bounded scan window reaches a specific
// blocked task, so any blocked/ready residue accumulated by earlier runs or
// aborted tests would shift the window and make the assertion non-deterministic.
// Purging first gives the scan tests a clean, deterministic surface regardless
// of prior DB state.
func purgeScanResidue(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		return
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListProjects(ctx, ttx.Tx, db.ListProjectsFilter{TenantID: approvalTestTenant, PageSize: 1000})
	if err != nil {
		return
	}
	for _, p := range rows {
		if p.Name == "seq-env" {
			_ = db.DeleteProject(ctx, ttx.Tx, approvalTestTenant, p.ID)
		}
	}
	_ = ttx.Commit(ctx)
}

// TestStandaloneBlockedReadyFlipsToBlocked: a ready task whose upstream
// dependency is not terminal-success parks as BLOCKED (persisted) and is
// never dispatched — the stall is surfaced, not silently requeued.
func TestStandaloneBlockedReadyFlipsToBlocked(t *testing.T) {
	env, task := newBlockedStandaloneEnv(t)
	ctx := context.Background()
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	got := mustGet(t, env.pool, task.ID)
	if got.Status != domain.WorkItemBlocked {
		t.Errorf("task status = %q, want %q (surfaced stall)", got.Status, domain.WorkItemBlocked)
	}
	// No dispatch: no execution row created.
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 0 {
		t.Errorf("executions for task = %d, want 0 (blocked task must never dispatch)", n)
	}
}

// TestStandaloneBlockedStaysBlocked: a blocked task whose blocker is still
// non-terminal (failed — the server gate accepts only succeeded) stays
// blocked across passes; still no dispatch.
func TestStandaloneBlockedStaysBlocked(t *testing.T) {
	env, task := newBlockedStandaloneEnv(t)
	ctx := context.Background()
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("task status = %q, want %q", got.Status, domain.WorkItemBlocked)
	}
	// A FAILED blocker still blocks (server gate: only succeeded unblocks).
	setStatus(t, env.pool, blocker.ID, domain.WorkItemFailed)
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemBlocked {
		t.Errorf("task status = %q, want %q (failed blocker still blocks)", got.Status, domain.WorkItemBlocked)
	}
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 0 {
		t.Errorf("executions for task = %d, want 0", n)
	}
}

// TestStandaloneBlockedClearsAndDispatches: once the blocker succeeds, the
// next pass flips blocked → ready → assigned and dispatches in the SAME
// pass (execution row created).
func TestStandaloneBlockedClearsAndDispatches(t *testing.T) {
	env, task := newBlockedStandaloneEnv(t)
	ctx := context.Background()
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("task status = %q, want %q", got.Status, domain.WorkItemBlocked)
	}

	// Blocker succeeds → the blocked task clears to ready and dispatches
	// (assigned) in the same pass.
	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne after unblock: %v", err)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemAssigned {
		t.Errorf("task status = %q, want %q (cleared + dispatched)", got.Status, domain.WorkItemAssigned)
	}
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 1 {
		t.Errorf("executions for task = %d, want 1 (dispatch happened)", n)
	}
}

// TestStandaloneBlockedScanClearsBlocked: the empty-key scan pass picks up
// blocked tasks (ListBlockedTasks) and clears them once the gate
// satisfies, without any notifier.
func TestStandaloneBlockedScanClearsBlocked(t *testing.T) {
	ctx := context.Background()
	// The scan pass re-evaluates a bounded window of blocked tasks in the
	// shared tnt_dev tenant. Purge any residue from earlier runs FIRST so
	// this test's own blocked task is guaranteed to be within the window.
	purgeScanResidue(t, approvalTestPool(t))
	env, task := newBlockedStandaloneEnv(t)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan: %v", res.Error)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemBlocked {
		t.Fatalf("task status = %q, want %q", got.Status, domain.WorkItemBlocked)
	}

	setStatus(t, env.pool, blocker.ID, domain.WorkItemSucceeded)
	if res := rec.Reconcile(ctx, ""); res.Error != nil {
		t.Fatalf("scan after unblock: %v", res.Error)
	}
	if got := mustGet(t, env.pool, task.ID); got.Status != domain.WorkItemAssigned {
		t.Errorf("task status = %q, want %q (scan cleared + dispatched)", got.Status, domain.WorkItemAssigned)
	}
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 1 {
		t.Errorf("executions for task = %d, want 1", n)
	}
}

// TestStandaloneBlockedScanListsBlocked verifies ListBlockedTasks returns
// blocked standalone tasks (and not ready ones) — the scan complement to
// ListReadyTasks.
func TestStandaloneBlockedScanListsBlocked(t *testing.T) {
	ctx := context.Background()
	purgeScanResidue(t, approvalTestPool(t))
	env, task := newBlockedStandaloneEnv(t)
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	blocked, err := db.ListBlockedTasks(ctx, ttx.Tx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range blocked {
		if b.ID == task.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("ListBlockedTasks missing blocked task %s", task.ID)
	}
}

// TestStandaloneBlockedNotDispatchedWorkflowBound: a blocked item bound to
// a workflow run is not touched by the standalone scan at all (workflow
// dispatch is the WorkflowReconciler's job).
func TestStandaloneBlockedNotDispatchedWorkflowBound(t *testing.T) {
	env, task := newBlockedStandaloneEnv(t)
	ctx := context.Background()
	setField(t, env.pool, task.ID, func(f *db.UpdateWorkItemFields) {
		rid := "run-" + db.NewID()
		f.WorkflowRunID = &rid
	})
	blocker := createWorkItem(t, env.pool, env.proj.ID, domain.WorkItemKindTask, "Blocker", nil, nil)
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)

	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})
	if err := rec.reconcileOne(ctx, task.ID, ""); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}
	got := mustGet(t, env.pool, task.ID)
	if got.Status != domain.WorkItemReady {
		t.Errorf("task status = %q, want %q (workflow-bound items are not gated here)", got.Status, domain.WorkItemReady)
	}
	if n := countExecutionsForTask(t, env.pool, task.ID); n != 0 {
		t.Errorf("executions for task = %d, want 0", n)
	}
}

// newBlockedStandalonePair creates one ready task with an assigned worker
// plus a distinct blocking work item, and a blocking edge blocker→task.
// Both rows live in env.proj so the project cleanup removes them.
func newBlockedStandalonePair(t *testing.T, env *sequenceTestEnv, idx int) (task, blocker db.WorkItemRow) {
	t.Helper()
	ctx := context.Background()
	task = db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Blocked Task " + db.NewID()[:6],
		Status:            domain.WorkItemReady,
		AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
	}
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateWorkItem(ctx, ttx.Tx, task)
	if err != nil {
		t.Fatal(err)
	}
	task = created
	blocker = db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: env.proj.ID,
		Kind: domain.WorkItemKindTask, Title: "Blocker " + db.NewID()[:6],
		Status: domain.WorkItemPending,
	}
	createdB, err := db.CreateWorkItem(ctx, ttx.Tx, blocker)
	if err != nil {
		t.Fatal(err)
	}
	blocker = createdB
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	addDependency(t, env.pool, env.proj.ID, blocker.ID, task.ID, domain.DependencyBlocks)
	_ = idx
	return task, blocker
}

// TestStandaloneBlockedScanRotationClearsBacklog guards the scan-batch
// starvation regression: with more blocked tasks than the per-tick scan
// batch, the re-evaluation window ROTATES so every blocked item is
// eventually re-checked. After every blocker turns terminal, repeated scan
// passes clear the WHOLE backlog to assigned (each pass processes a fresh
// window), instead of permanently re-scanning the same oldest rows.
func TestStandaloneBlockedScanRotationClearsBacklog(t *testing.T) {
	ctx := context.Background()
	// A fresh reconciler instance starts its blocked cursor at zero; purge
	// any residue so the window is fully deterministic.
	purgeScanResidue(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	rec := NewTaskReconciler(env.pool, slog.Default(), &manifestCaptureBridge{})

	// More blocked tasks than scanBatchSize forces rotation across passes.
	n := scanBatchSize + 4
	tasks := make([]db.WorkItemRow, 0, n)
	blockers := make([]db.WorkItemRow, 0, n)
	for i := 0; i < n; i++ {
		task, blocker := newBlockedStandalonePair(t, env, i)
		tasks = append(tasks, task)
		blockers = append(blockers, blocker)
	}

	// All blockers succeed → every blocked task becomes dispatchable.
	for _, b := range blockers {
		setStatus(t, env.pool, b.ID, domain.WorkItemSucceeded)
	}

	// Run scans until the whole backlog clears. With rotation, this must
	// converge: each pass dispatches the ready tasks it reaches (a blocked
	// task whose gate satisfied flips ready→dispatch in the same pass) and
	// rotates the blocked re-evaluation window, so the full backlog clears
	// in bounded passes. Before the rotation fix, a backlog larger than the
	// batch permanently re-scanned the same oldest rows and never cleared.
	deadline := 40 // scans
	cleared := 0
	for pass := 0; pass < deadline && cleared < n; pass++ {
		if res := rec.Reconcile(ctx, ""); res.Error != nil {
			t.Fatalf("scan pass %d: %v", pass, res.Error)
		}
		cleared = 0
		for _, task := range tasks {
			if got := mustGet(t, env.pool, task.ID); got.Status == domain.WorkItemAssigned {
				cleared++
			}
		}
	}
	if cleared != n {
		t.Errorf("backlog clear: cleared %d/%d blocked tasks within %d passes (rotation regression)", cleared, n, deadline)
	}
}

// TestStandaloneBlockedRotationWindowAdvance pins the rotation cursor
// math directly (no DB): a window of budget over n blocked items advances
// so a second call covers the next chunk.
func TestStandaloneBlockedRotationWindowAdvance(t *testing.T) {
	rec := &TaskReconciler{}
	n := 20
	budget := 16
	// Two successive calls must not return the same start offset.
	first := rec.blockedWindowStart(n)
	rec.advanceBlockedCursor(n, budget)
	second := rec.blockedWindowStart(n)
	if first == second {
		t.Errorf("rotation cursor did not advance: first=%d second=%d", first, second)
	}
	// Advancing exactly n returns the cursor to its origin (full cycle).
	rec.advanceBlockedCursor(n, n-budget) // 4 → cursor 0
	rec.advanceBlockedCursor(n, n)        // 20 → cursor 0
	if got := rec.blockedWindowStart(n); got != 0 {
		t.Errorf("window start after full cycle = %d, want 0", got)
	}
}
