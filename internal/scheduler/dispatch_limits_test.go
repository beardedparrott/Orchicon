package scheduler

// Dispatch-limit tests for the concurrency-guards work item
// (architecture-notes/per-project-dispatch-limits.md): the D2 admission
// gate in TaskReconciler.reconcileOne holds dispatches past a project's
// effective max-concurrent-runs cap (items stay 'ready' and dispatch when a
// slot frees), and the D3 WorktreeReconciler gate atomically serializes
// non-repo (in-place) runs. DB-backed; skipped without ORCHICON_TEST_DSN.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/scheduler/ -run 'TestDispatchLimit' -v

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// stubDispatchLimiter is a test DispatchLimiter with fixed per-project
// limits, so the D2/D3 gates are exercised without depending on the real
// tenant_settings/projects rows.
type stubDispatchLimiter struct {
	effLimit   func(projectID string) int
	inPlaceLim func(projectID string) int
}

func (s stubDispatchLimiter) Limit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	if s.effLimit != nil {
		return s.effLimit(projectID), nil
	}
	return 0, nil
}

func (s stubDispatchLimiter) InPlaceLimit(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	if s.inPlaceLim != nil {
		return s.inPlaceLim(projectID), nil
	}
	return 1, nil
}

// seedReadyTask creates a ready standalone task bound to the seeded dev
// worker for the given project.
func seedReadyTask(t *testing.T, pool *db.Pool, projectID string) db.WorkItemRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	created, err := db.CreateWorkItem(ctx, ttx.Tx, db.WorkItemRow{
		ID: db.NewID(), TenantID: approvalTestTenant, ProjectID: projectID,
		Kind: domain.WorkItemKindTask, Title: "Dispatch Limit Task " + db.NewID()[:6],
		Status:            domain.WorkItemReady,
		AssignedWorkerRef: []byte(`{"worker_id":"w_se_devops_engineer","version":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return created
}

// seedReadyAdapter registers a ready opencode adapter with generous
// capacity so the TaskReconciler's selectAdapter always finds one.
func seedReadyAdapter(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
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
}

// TestDispatchLimitGateHoldsSecondUntilSlotFrees exercises the D2 gate:
// with a project effective limit of 1, the first dispatch creates an
// execution and the second is held (stays 'ready'). Once the first
// execution reaches a terminal state, the held task dispatches.
func TestDispatchLimitGateHoldsSecondUntilSlotFrees(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	ctx := context.Background()
	first := seedReadyTask(t, env.pool, env.proj.ID)
	second := seedReadyTask(t, env.pool, env.proj.ID)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
	rec.SetDispatchLimiter(stubDispatchLimiter{effLimit: func(string) int { return 1 }})

	if err := rec.reconcileOne(ctx, first.ID, ""); err != nil {
		t.Fatalf("dispatch first: %v", err)
	}
	if got := mustGet(t, env.pool, first.ID); got.Status != domain.WorkItemAssigned {
		t.Fatalf("first task status = %q, want assigned", got.Status)
	}
	if err := rec.reconcileOne(ctx, second.ID, ""); err != nil {
		t.Fatalf("dispatch second: %v", err)
	}
	// Held: still ready, no execution created.
	if got := mustGet(t, env.pool, second.ID); got.Status != domain.WorkItemReady {
		t.Fatalf("second task status = %q, want ready (held at cap)", got.Status)
	}
	// Both executions for this project exist; the second task has none.
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
	secondExecs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID: approvalTestTenant, ProjectID: env.proj.ID, TaskID: second.ID,
	})
	_ = ttx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstExecs) != 1 {
		t.Fatalf("first task executions = %d, want 1", len(firstExecs))
	}
	if len(secondExecs) != 0 {
		t.Fatalf("second task executions = %d, want 0 (held at cap)", len(secondExecs))
	}

	// Free the slot: mark the first execution terminal.
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

// TestDispatchLimitNoGateWithoutLimiter is the control: with no limiter
// installed, both ready tasks dispatch.
func TestDispatchLimitNoGateWithoutLimiter(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newSequenceTestEnv(t)
	seedReadyAdapter(t, env.pool)
	ctx := context.Background()
	first := seedReadyTask(t, env.pool, env.proj.ID)
	second := seedReadyTask(t, env.pool, env.proj.ID)

	rec := NewTaskReconciler(env.pool, slog.Default(), testDispatcher(&manifestCaptureBridge{}))
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
		t.Fatalf("second task status = %q, want assigned (no limiter)", got.Status)
	}
}

// TestWorktreeInPlaceSerialization exercises the D3 gate: a non-repo
// project with in-place limit 1 admits its first run ('skipped' = the
// in-place token) and holds a second run ('pending'), then admits it once
// the first run reaches a terminal state.
func TestWorktreeInPlaceSerialization(t *testing.T) {
	purgeScanTenant(t, approvalTestPool(t))
	env := newWorktreeTestEnv(t)
	ctx := context.Background()

	plain := nonRepoDir(t)
	ttx, err := env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	proj, err := db.CreateProject(ctx, ttx.Tx, db.ProjectRow{
		ID: db.NewID(), TenantID: approvalTestTenant,
		Name: "In-Place Serialize Project", Slug: "inplace-" + strings.ToLower(db.NewID()),
		Status: domain.ProjectActive, Goals: []byte("[]"),
		ProjectDir: plain,
	})
	if err != nil {
		t.Fatalf("create non-repo project: %v", err)
	}
	mkRun := func() db.WorkflowRunRow {
		run, err := db.CreateWorkflowRun(ctx, ttx.Tx, db.WorkflowRunRow{
			ID: db.NewID(), TenantID: approvalTestTenant,
			WorkflowID: "wf-worktree-test", WorkflowVersion: 1,
			ProjectID: proj.ID, Status: domain.WorkflowRunPending,
			RunContext: []byte("{}"),
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return run
	}
	run1 := mkRun()
	run2 := mkRun()
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}

	env.rec.SetDispatchLimiter(stubDispatchLimiter{inPlaceLim: func(string) int { return 1 }})

	// First run is admitted in place.
	if res := env.rec.Reconcile(ctx, run1.ID); res.Error != nil {
		t.Fatalf("reconcile run1: %v", res.Error)
	}
	got1 := mustGetRun(t, env.pool, run1.ID)
	if got1.WorktreeStatus != domain.WorktreeSkipped {
		t.Fatalf("run1 worktree_status = %q, want skipped (admitted)", got1.WorktreeStatus)
	}

	// Second run is held: the first holds the single in-place slot.
	if res := env.rec.Reconcile(ctx, run2.ID); res.Error != nil {
		t.Fatalf("reconcile run2: %v", res.Error)
	}
	got2 := mustGetRun(t, env.pool, run2.ID)
	if got2.WorktreeStatus != domain.WorktreePending {
		t.Fatalf("run2 worktree_status = %q, want pending (held at in-place cap)", got2.WorktreeStatus)
	}

	// Free the slot: run1 reaches a terminal state.
	ttx, err = env.pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := db.UpdateWorkflowRun(ctx, ttx.Tx, approvalTestTenant, run1.ID, got1.Version, db.UpdateWorkflowRunFields{
		Status: strPtr(domain.WorkflowRunCompleted),
	}); err != nil {
		t.Fatalf("mark run1 terminal: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit terminal: %v", err)
	}

	if res := env.rec.Reconcile(ctx, run2.ID); res.Error != nil {
		t.Fatalf("reconcile run2 after slot freed: %v", res.Error)
	}
	got2 = mustGetRun(t, env.pool, run2.ID)
	if got2.WorktreeStatus != domain.WorktreeSkipped {
		t.Fatalf("run2 worktree_status after slot freed = %q, want skipped", got2.WorktreeStatus)
	}
}

func mustGetRun(t *testing.T, pool *db.Pool, id string) db.WorkflowRunRow {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, approvalTestTenant)
	if err != nil {
		t.Fatal(err)
	}
	defer ttx.Rollback(ctx)
	run, err := db.GetWorkflowRun(ctx, ttx.Tx, approvalTestTenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// TestEffectiveDispatchLimitFormula is a pure-formula check (no DB) for
// the min(tenant, project) effective limit and the non-repo in-place
// default.
func TestEffectiveDispatchLimitFormula(t *testing.T) {
	cases := []struct {
		name            string
		tenant, project int
		wantEff         int
		wantInPlace     int
	}{
		{"both unset", 0, 0, 0, 1},
		{"tenant only", 4, 0, 4, 1},
		{"project only", 0, 3, 3, 3},
		{"min wins", 2, 5, 2, 2},
		{"project under tenant", 5, 2, 2, 2},
		{"project opts in at 1", 0, 1, 1, 1},
		{"tenant caps opt-in", 1, 4, 1, 1},
		{"tenant unrestricted opt-in", 0, 4, 4, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := db.EffectiveDispatchLimit(c.tenant, c.project); got != c.wantEff {
				t.Errorf("EffectiveDispatchLimit(%d,%d) = %d, want %d", c.tenant, c.project, got, c.wantEff)
			}
			if got := db.InPlaceLimit(c.tenant, c.project); got != c.wantInPlace {
				t.Errorf("InPlaceLimit(%d,%d) = %d, want %d", c.tenant, c.project, got, c.wantInPlace)
			}
		})
	}
}
