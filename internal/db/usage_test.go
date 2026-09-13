package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// TestGetWorkflowRunCostsWorkItemName verifies the run-level cost breakdown
// resolves the bound work item's name server-side. Guarded by
// ORCHICON_TEST_DSN like the seed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestGetWorkflowRunCostsWorkItemName -v
//
// It guards the GROUP BY extension that makes the cross-JOIN name resolution
// legal (functional dependency does not cross the LEFT JOIN), the scan-order
// contract, and the one-shot fallback (no bound work item -> empty name).
func TestGetWorkflowRunCostsWorkItemName(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed cost test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const tenant = "tnt_dev"
	const workflowID = "workflow-cost-test"
	seedCostData(t, pool, tenant, workflowID)

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	rows, err := db.GetWorkflowRunCosts(ctx, ttx.Tx, tenant, workflowID, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetWorkflowRunCosts: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 runs, got %d: %+v", len(rows), rows)
	}

	byID := map[string]db.WorkflowRunCostRow{}
	for _, r := range rows {
		byID[r.WorkflowRunID] = r
	}

	if r := byID["run-cost-bound1"]; r.WorkItemID != "item-cost-bound1" || r.WorkItemName != "Add auth to the mobile app" {
		t.Errorf("bound run 1: want work_item_id=item-cost-bound1 name='Add auth to the mobile app', got %+v", r)
	}
	if r := byID["run-cost-bound2"]; r.WorkItemID != "item-cost-bound2" || r.WorkItemName != "Fix billing rounding bug" {
		t.Errorf("bound run 2: want work_item_id=item-cost-bound2 name='Fix billing rounding bug', got %+v", r)
	}
	if r := byID["run-cost-oneshot"]; r.WorkItemID != "" || r.WorkItemName != "" {
		t.Errorf("one-shot run: want empty work_item_id/name, got %+v", r)
	}
}

// TestSumUsageForExecution verifies the per-execution usage-record sum
// that now backs every per-execution token/cost figure (the
// worker_executions row columns are write-never). Guarded by
// ORCHICON_TEST_DSN like the seed tests.
func TestSumUsageForExecution(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed cost test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const tenant = "tnt_dev"
	const workflowID = "workflow-cost-test"
	seedCostData(t, pool, tenant, workflowID)

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	// exec-cost-1 has a single 150-token / $0.0015 record.
	tokens, cost, err := db.SumUsageForExecution(ctx, ttx.Tx, tenant, "exec-cost-1")
	if err != nil {
		t.Fatalf("SumUsageForExecution: %v", err)
	}
	if tokens != 150 || cost != 0.0015 {
		t.Errorf("exec-cost-1: got %d tokens $%f, want 150 $0.0015", tokens, cost)
	}
	// Unknown execution: zero sum, no error (consumers fall back cleanly).
	tokens, cost, err = db.SumUsageForExecution(ctx, ttx.Tx, tenant, "exec-cost-missing")
	if err != nil {
		t.Fatalf("SumUsageForExecution missing: %v", err)
	}
	if tokens != 0 || cost != 0 {
		t.Errorf("missing execution: got %d tokens $%f, want zeros", tokens, cost)
	}
}

// seedCostData inserts the minimal row set the run-cost query joins
// (projects → workflows → work_items → workflow_runs → worker_executions →
// usage_records) for two bound runs and one one-shot run, all scoped to the
// tenant so the tenant-scoped tx + RLS backstop see them.
func seedCostData(t *testing.T, pool *db.Pool, tenant, workflowID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	insert := func(q string, args ...any) {
		t.Helper()
		if _, err := ttx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed cost data: %v", err)
		}
	}

	// Idempotency: drop any rows a previous run of this test left behind.
	insert(`DELETE FROM usage_records WHERE tenant_id = $1 AND id LIKE 'rec-cost-%'`, tenant)
	insert(`DELETE FROM worker_executions WHERE tenant_id = $1 AND id LIKE 'exec-cost-%'`, tenant)
	insert(`DELETE FROM workflow_runs WHERE tenant_id = $1 AND id LIKE 'run-cost-%'`, tenant)
	insert(`DELETE FROM work_items WHERE tenant_id = $1 AND id LIKE 'item-cost-%'`, tenant)
	insert(`DELETE FROM workflows WHERE tenant_id = $1 AND id = $2`, tenant, workflowID)
	insert(`DELETE FROM projects WHERE tenant_id = $1 AND id = 'proj-cost-test'`, tenant)

	insert(`INSERT INTO projects (id, tenant_id, name, slug, status, version, created_at, updated_at)
		VALUES ('proj-cost-test', $1, 'Cost Test', 'cost-test', 'active', 1, now(), now())`, tenant)
	insert(`INSERT INTO workflows (id, tenant_id, project_id, name, current_version, status, version, created_at, updated_at, type)
		VALUES ($2, $1, 'proj-cost-test', 'Cost Workflow', 1, 'published', 1, now(), now(), 'template')`, tenant, workflowID)

	insert(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
		VALUES ('item-cost-bound1', $1, 'proj-cost-test', 'task', 'Add auth to the mobile app', 'succeeded', 1, now(), now())`, tenant)
	insert(`INSERT INTO work_items (id, tenant_id, project_id, kind, title, status, version, created_at, updated_at)
		VALUES ('item-cost-bound2', $1, 'proj-cost-test', 'task', 'Fix billing rounding bug', 'succeeded', 1, now(), now())`, tenant)

	insert(`INSERT INTO workflow_runs (id, tenant_id, workflow_id, workflow_version, project_id, status, version, created_at, updated_at, work_item_id)
		VALUES ('run-cost-bound1', $1, $2, 1, 'proj-cost-test', 'completed', 1, now(), now(), 'item-cost-bound1')`, tenant, workflowID)
	insert(`INSERT INTO workflow_runs (id, tenant_id, workflow_id, workflow_version, project_id, status, version, created_at, updated_at, work_item_id)
		VALUES ('run-cost-bound2', $1, $2, 1, 'proj-cost-test', 'completed', 1, now(), now(), 'item-cost-bound2')`, tenant, workflowID)
	insert(`INSERT INTO workflow_runs (id, tenant_id, workflow_id, workflow_version, project_id, status, version, created_at, updated_at, work_item_id)
		VALUES ('run-cost-oneshot', $1, $2, 1, 'proj-cost-test', 'failed', 1, now(), now(), NULL)`, tenant, workflowID)

	insert(`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, version, created_at, updated_at, workflow_run_id)
		VALUES ('exec-cost-1', $1, 'proj-cost-test', 'item-cost-bound1', 'worker-cost', 1, 'completed', 1, now(), now(), 'run-cost-bound1')`, tenant)
	insert(`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, version, created_at, updated_at, workflow_run_id)
		VALUES ('exec-cost-2', $1, 'proj-cost-test', 'item-cost-bound2', 'worker-cost', 1, 'completed', 1, now(), now(), 'run-cost-bound2')`, tenant)
	insert(`INSERT INTO worker_executions (id, tenant_id, project_id, task_id, worker_id, worker_version, status, version, created_at, updated_at, workflow_run_id)
		VALUES ('exec-cost-3', $1, 'proj-cost-test', 'item-cost-bound1', 'worker-cost', 1, 'completed', 1, now(), now(), 'run-cost-oneshot')`, tenant)

	insert(`INSERT INTO usage_records (id, tenant_id, project_id, task_id, execution_id, worker_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at, workflow_run_id)
		VALUES ('rec-cost-1', $1, 'proj-cost-test', 'item-cost-bound1', 'exec-cost-1', 'worker-cost', 'openai', 'gpt-4o', 100, 50, 150, 0.0015, now(), now(), 'run-cost-bound1')`, tenant)
	insert(`INSERT INTO usage_records (id, tenant_id, project_id, task_id, execution_id, worker_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at, workflow_run_id)
		VALUES ('rec-cost-2', $1, 'proj-cost-test', 'item-cost-bound2', 'exec-cost-2', 'worker-cost', 'openai', 'gpt-4o', 200, 100, 300, 0.0030, now(), now(), 'run-cost-bound2')`, tenant)
	insert(`INSERT INTO usage_records (id, tenant_id, project_id, task_id, execution_id, worker_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at, workflow_run_id)
		VALUES ('rec-cost-3', $1, 'proj-cost-test', 'item-cost-bound1', 'exec-cost-3', 'worker-cost', 'openai', 'gpt-4o', 50, 25, 75, 0.0008, now(), now(), 'run-cost-oneshot')`, tenant)

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// TestListUsageRecordsSessionFilter guards the Ask-session read-back path:
// scope one conversation's usage by session_id, and round-trip the cache
// counters a chat client needs for its cache-hit ratio.
//
// Guarded by ORCHICON_TEST_DSN like the other DB-backed tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestListUsageRecordsSessionFilter -v
//
// It guards THREE contracts that a compile alone cannot:
//  1. the WHERE clause filters on session_id (and ” means UNSCOPED, so
//     worker-execution queries keep working);
//  2. the SELECT/scan order carries session_id + the cache counters (the
//     scan is positional, so a SELECT that lists a column the scanner does
//     not read — or vice versa — fails here);
//  3. tenant scoping still holds (a session filter never widens the tenant).
func TestListUsageRecordsSessionFilter(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed usage test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, assets.MigrationsFS, assets.MigrationsDir); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	const tenant = "tnt_dev"
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	insert := func(q string, args ...any) {
		t.Helper()
		if _, err := ttx.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed usage row: %v", err)
		}
	}
	insert(`DELETE FROM usage_records WHERE tenant_id = $1 AND id LIKE 'rec-sess-%'`, tenant)

	// Two rows for conv-a (one with cache counters), one for conv-b, one
	// worker row with NO session (the unscoped case).
	insert(`INSERT INTO usage_records (id, tenant_id, session_id, provider, model,
			prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, occurred_at, created_at)
		VALUES ('rec-sess-a1', $1, 'conv-a', 'anthropic', 'claude-sonnet-4', 1000, 200, 1200, 800, 100, 0.0100, now(), now())`, tenant)
	insert(`INSERT INTO usage_records (id, tenant_id, session_id, provider, model,
			prompt_tokens, completion_tokens, total_tokens, cache_read_tokens, cache_write_tokens,
			cost_usd, occurred_at, created_at)
		VALUES ('rec-sess-a2', $1, 'conv-a', 'anthropic', 'claude-sonnet-4', 500, 100, 600, 400, 0, 0.0050, now(), now())`, tenant)
	insert(`INSERT INTO usage_records (id, tenant_id, session_id, provider, model,
			prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at)
		VALUES ('rec-sess-b1', $1, 'conv-b', 'openai', 'gpt-4o', 10, 10, 20, 0.0001, now(), now())`, tenant)
	insert(`INSERT INTO usage_records (id, tenant_id, session_id, provider, model,
			prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at)
		VALUES ('rec-sess-worker', $1, '', 'openai', 'gpt-4o', 7, 7, 14, 0.0002, now(), now())`, tenant)

	// Scoped to conv-a: exactly its two rows, with the cache counters intact.
	scoped, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{
		TenantID: tenant, SessionID: "conv-a", PageSize: 100,
	})
	if err != nil {
		t.Fatalf("ListUsageRecords(session=conv-a): %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("session=conv-a returned %d rows, want 2", len(scoped))
	}
	var promptTotal, cacheReadTotal int64
	for _, r := range scoped {
		if r.SessionID != "conv-a" {
			t.Errorf("row %s carries session_id %q, want conv-a", r.ID, r.SessionID)
		}
		promptTotal += r.PromptTokens
		cacheReadTotal += r.CacheReadTokens
	}
	if promptTotal != 1500 || cacheReadTotal != 1200 {
		t.Errorf("conv-a totals = %d prompt / %d cache-read, want 1500 / 1200", promptTotal, cacheReadTotal)
	}

	// Unscoped: every row for the tenant (worker rows included), so an empty
	// SessionID never hides usage.
	all, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{TenantID: tenant, PageSize: 200})
	if err != nil {
		t.Fatalf("ListUsageRecords(unscoped): %v", err)
	}
	if len(all) < 4 {
		t.Fatalf("unscoped returned %d rows, want at least the 4 seeded", len(all))
	}

	// A session that has no usage returns an empty slice, not an error.
	none, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{
		TenantID: tenant, SessionID: "conv-missing", PageSize: 100,
	})
	if err != nil {
		t.Fatalf("ListUsageRecords(session=conv-missing): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown session returned %d rows, want 0", len(none))
	}
}
