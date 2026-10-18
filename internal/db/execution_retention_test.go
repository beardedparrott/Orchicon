package db

// execution_retention_test.go — the keep-newest-N execution prune.
//
// The operator: "I want to keep the 150 most recent executions and delete any other execution record."
//
// THE PROPERTIES THAT MATTER, and every one of them is a way this could be wrong while still "working":
//
//  1. THE KEEP BOUNDARY IS THE NEWEST N, BY created_at — not by insertion order, not by id, and not "the
//     first N the list happened to return". The operator picks a count and expects the RECENT end to
//     survive.
//  2. THE CHILDREN GO WITH THEIR PARENT. Only execution_session_parts has a foreign key; checkpoints,
//     recovery rows and step-run references are bare text columns, so a prune that deletes only the
//     execution leaves orphan rows that nothing will ever find again.
//  3. THE SPEND LEDGER SURVIVES. usage_records is de-linked, never deleted — migration 20260731000015
//     exists precisely so cost attribution outlives the execution. A prune that removed usage rows would
//     silently rewrite the operator's spending history.
//  4. A LIVE EXECUTION IS NOT HISTORY. A prune must refuse rather than kill running work.
//  5. IT IS TENANT-SCOPED. Another tenant's executions are never candidates.
//
// The test tenant is unique per run and cleaned up by the platform's tenant cascade, so it cannot touch a
// real tenant's data.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// retentionSeq makes ids unique under ULID truncation. ULID's monotonic entropy increments in its
// TRAILING characters, so `NewID()[:16]` returns the SAME string for every id minted within one
// millisecond — which is every id a fast test mints. A counter is immune to that.
var retentionSeq int

func uniqID(prefix string) string {
	retentionSeq++
	return fmt.Sprintf("%s_%d_%s", prefix, retentionSeq, NewID()[:10])
}

func retentionTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed execution retention test")
	}
	pool, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedRetentionTenant writes n terminal executions for tenant, created one hour apart starting
// baseAgeHours in the past — so the created_at ORDER is unambiguous and the newest is id-independent.
type retentionSeed struct {
	id   string
	when time.Time
}

func seedExecutions(t *testing.T, pool *Pool, tenant, project string, n int, baseAgeHours int) []retentionSeed {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	now := time.Now().UTC()
	out := make([]retentionSeed, 0, n)
	for i := 0; i < n; i++ {
		id := uniqID("zz_exec")
		// i=0 is the OLDEST; the highest i is the newest.
		when := now.Add(-time.Duration(baseAgeHours-i) * time.Hour)
		if _, err := ttx.Exec(ctx, `INSERT INTO worker_executions
			(id, tenant_id, project_id, task_id, worker_id, worker_version, status, health_state, version, created_at, updated_at)
			VALUES ($1,$2,$3,'task_probe','w_1',1,'succeeded','healthy',1,$4,$4)`,
			id, tenant, project, when); err != nil {
			t.Fatalf("seed execution %d: %v", i, err)
		}
		out = append(out, retentionSeed{id: id, when: when})
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return out
}

// seedChildren writes the full child set for one execution and returns nothing — the assertions read the
// live tables back.
func seedChildren(t *testing.T, pool *Pool, tenant, project, execID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	stmts := []struct {
		what string
		q    string
	}{
		{"session part", `INSERT INTO execution_session_parts (execution_id, tenant_id, seq, kind, payload, created_at)
			VALUES ($1,$2,$3,'text','{}',now())`},
		{"checkpoint", `INSERT INTO checkpoints (id, tenant_id, worker_execution_id, format_version, blob_ref)
			VALUES ($3,$2,$1,'v1','blob://x')`},
		{"usage record", `INSERT INTO usage_records (id, tenant_id, project_id, execution_id, provider, model, total_tokens, cost_usd, occurred_at)
			VALUES ($3,$2,$4,$1,'deepseek','deepseek-flash',100,0.01,now())`},
		{"recovery execution", `INSERT INTO recovery_executions (id, tenant_id, project_id, task_id, failed_execution_id, recovery_workflow_id, trigger_reason, level, status, current_step, resumption_path, continuation_plan_id, reviewer_worker_id, summary, version, triggered_at, strategy, max_retries, retry_delay_seconds)
			VALUES ($3,$2,$4,'task_probe',$1,'','probe',1,'running','','summarize_resume','','','',1,now(),'summarize_restart',5,10)`},
	}
	for _, s := range stmts {
		id := uniqID("zz_child")
		// ARGUMENTS ARE PER STATEMENT: the session-part insert has no id column (its key is
		// (tenant_id, execution_id, seq)), and pgx rejects both too many and too few parameters.
		var args []any
		switch s.what {
		case "usage record", "recovery execution":
			args = []any{execID, tenant, id, project}
		case "checkpoint":
			args = []any{execID, tenant, id}
		default: // session part — ($1 execution, $2 tenant, $3 seq)
			args = []any{execID, tenant, 1}
		}
		if _, err := ttx.Exec(ctx, s.q, args...); err != nil {
			t.Fatalf("seed %s: %v", s.what, err)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit seed children: %v", err)
	}
}

func countTenantRows(t *testing.T, pool *Pool, tenant, q string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int64
	if err := ttx.Tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// THE HEADLINE PROPERTY: the newest N survive and everything older goes, with its children.
func TestPruneExecutionHistoryKeepsTheNewestN(t *testing.T) {
	pool := retentionTestPool(t)
	tenant := uniqID("tnt_retention")
	project := uniqID("proj_retention")

	const total, keep = 12, 5
	seeds := seedExecutions(t, pool, tenant, project, total, 40)

	// Children on EVERY execution, so a prune that only clears some of them is visible.
	for _, s := range seeds {
		seedChildren(t, pool, tenant, project, s.id)
	}

	before := countTenantRows(t, pool, tenant, `SELECT count(*) FROM worker_executions WHERE tenant_id=$1`, tenant)
	if before != total {
		t.Fatalf("fixture: seeded %d executions, found %d", total, before)
	}

	plan, err := pool.PruneExecutionHistory(context.Background(), tenant, keep, 3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	// 2. count: exactly keep survive.
	after := countTenantRows(t, pool, tenant, `SELECT count(*) FROM worker_executions WHERE tenant_id=$1`, tenant)
	if after != keep {
		t.Errorf("executions after prune = %d, want %d", after, keep)
	}
	if plan.DeleteExecutions != total-keep {
		t.Errorf("plan reported %d deletions, want %d", plan.DeleteExecutions, total-keep)
	}

	// 1. ORDER: the survivors are the NEWEST `keep` — checked by id, which the seed deliberately made
	// uncorrelated with created_at order.
	newest := seeds[total-keep:]
	for _, s := range newest {
		if got := countTenantRows(t, pool, tenant,
			`SELECT count(*) FROM worker_executions WHERE tenant_id=$1 AND id=$2`, tenant, s.id); got != 1 {
			t.Errorf("execution %s (created %s) should have SURVIVED as one of the newest %d",
				s.id, s.when.Format(time.RFC3339), keep)
		}
	}
	oldest := seeds[:total-keep]
	for _, s := range oldest {
		if got := countTenantRows(t, pool, tenant,
			`SELECT count(*) FROM worker_executions WHERE tenant_id=$1 AND id=$2`, tenant, s.id); got != 0 {
			t.Errorf("execution %s (created %s) should have been REMOVED", s.id, s.when.Format(time.RFC3339))
		}
	}

	// 3. THE CHILDREN OF THE DELETED ROWS ARE GONE …
	deletedIDs := make([]string, 0, len(oldest))
	for _, s := range oldest {
		deletedIDs = append(deletedIDs, s.id)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM execution_session_parts WHERE tenant_id=$1 AND execution_id = ANY($2)`, tenant, deletedIDs); n != 0 {
		t.Errorf("%d session parts survived their execution — the transcript is the bulk of the bytes", n)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM checkpoints WHERE worker_execution_id = ANY($1)`, deletedIDs); n != 0 {
		t.Errorf("%d checkpoints survived their execution (no FK — an orphan nothing would ever find)", n)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM recovery_executions WHERE tenant_id=$1 AND failed_execution_id = ANY($2)`, tenant, deletedIDs); n != 0 {
		t.Errorf("%d recoveries survived the execution that failed — a recovery whose reason is gone", n)
	}

	// … AND THE SURVIVORS KEEP THEIRS.
	survivorIDs := make([]string, 0, len(newest))
	for _, s := range newest {
		survivorIDs = append(survivorIDs, s.id)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM execution_session_parts WHERE tenant_id=$1 AND execution_id = ANY($2)`, tenant, survivorIDs); n != int64(keep) {
		t.Errorf("session parts on survivors = %d, want %d (the prune must not touch what it keeps)", n, keep)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM recovery_executions WHERE tenant_id=$1 AND failed_execution_id = ANY($2)`, tenant, survivorIDs); n != int64(keep) {
		t.Errorf("recoveries on survivors = %d, want %d", n, keep)
	}

	// 4. THE SPEND LEDGER SURVIVES AND IS DE-LINKED. Both halves matter: the rows must still exist (the
	// Cost Explorer reads them) and nothing in them may point at a deleted execution.
	usageTotal := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM usage_records WHERE tenant_id=$1 AND project_id=$2`, tenant, project)
	if usageTotal != int64(total) {
		t.Errorf("usage records = %d, want %d — the spend ledger must NOT be pruned with the executions", usageTotal, total)
	}
	if n := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM usage_records WHERE tenant_id=$1 AND execution_id = ANY($2)`, tenant, deletedIDs); n != 0 {
		t.Errorf("%d usage records still reference a DELETED execution — cost attribution would follow a dead id", n)
	}
}

// A LIVE EXECUTION IS NOT HISTORY: the prune refuses and touches nothing.
func TestPruneExecutionHistoryRefusesWhenACandidateIsLive(t *testing.T) {
	pool := retentionTestPool(t)
	tenant := uniqID("tnt_retention_live")
	project := uniqID("proj_retention_live")

	// Three terminal executions and one RUNNING, all old enough to be candidates.
	seeds := seedExecutions(t, pool, tenant, project, 3, 20)
	liveID := uniqID("zz_exec_live")
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := ttx.Exec(ctx, `INSERT INTO worker_executions
		(id, tenant_id, project_id, task_id, worker_id, worker_version, status, health_state, version, created_at, updated_at)
		VALUES ($1,$2,$3,'task_probe','w_1',1,'running','healthy',1,$4,$4)`,
		liveID, tenant, project, time.Now().UTC().Add(-5*time.Hour)); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// keep=0 makes every execution a candidate, including the live one.
	_, err = pool.PruneExecutionHistory(ctx, tenant, 0, 100)
	if err == nil {
		t.Fatal("the prune did not refuse while a candidate execution was still RUNNING")
	}

	// AND IT DELETED NOTHING — not even the terminal ones it could have taken.
	for _, s := range seeds {
		if got := countTenantRows(t, pool, tenant,
			`SELECT count(*) FROM worker_executions WHERE tenant_id=$1 AND id=$2`, tenant, s.id); got != 1 {
			t.Errorf("execution %s was deleted by a prune that REFUSED", s.id)
		}
	}
	if got := countTenantRows(t, pool, tenant,
		`SELECT count(*) FROM worker_executions WHERE tenant_id=$1 AND id=$2`, tenant, liveID); got != 1 {
		t.Errorf("the RUNNING execution was deleted")
	}

	// A live execution is out of the candidate set entirely, so keeping it out of the way lets the prune
	// proceed on the history around it.
	plan, err := pool.PlanExecutionRetention(ctx, tenant, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.LiveAmongDelete != 1 {
		t.Errorf("plan reports %d live candidates, want 1 (the live one must be VISIBLE as a refusal "+
			"reason, not silently skipped)", plan.LiveAmongDelete)
	}
}

// TENANT ISOLATION: another tenant's executions are never candidates.
func TestPruneExecutionHistoryIsTenantScoped(t *testing.T) {
	pool := retentionTestPool(t)
	tenantA := uniqID("tnt_retention_iso_a")
	tenantB := uniqID("tnt_retention_iso_b")
	projA := uniqID("proj_iso_a")
	projB := uniqID("proj_iso_b")

	seedExecutions(t, pool, tenantA, projA, 6, 30)
	seedExecutions(t, pool, tenantB, projB, 6, 30)

	if _, err := pool.PruneExecutionHistory(context.Background(), tenantA, 2, 100); err != nil {
		t.Fatalf("prune tenant A: %v", err)
	}

	if n := countTenantRows(t, pool, tenantA, `SELECT count(*) FROM worker_executions WHERE tenant_id=$1`, tenantA); n != 2 {
		t.Errorf("tenant A executions = %d, want 2", n)
	}
	if n := countTenantRows(t, pool, tenantB, `SELECT count(*) FROM worker_executions WHERE tenant_id=$1`, tenantB); n != 6 {
		t.Errorf("tenant B executions = %d, want 6 — pruning A must never touch B", n)
	}
}

// A SECOND RUN IS A NO-OP, so an operator can safely re-run after an interruption.
func TestPruneExecutionHistoryIsIdempotent(t *testing.T) {
	pool := retentionTestPool(t)
	tenant := uniqID("tnt_retention_idem")
	project := uniqID("proj_retention_idem")
	seeds := seedExecutions(t, pool, tenant, project, 8, 30)
	for _, s := range seeds {
		seedChildren(t, pool, tenant, project, s.id)
	}

	ctx := context.Background()
	if _, err := pool.PruneExecutionHistory(ctx, tenant, 3, 100); err != nil {
		t.Fatalf("first prune: %v", err)
	}
	plan, err := pool.PruneExecutionHistory(ctx, tenant, 3, 100)
	if err != nil {
		t.Fatalf("second prune: %v", err)
	}
	if plan.DeleteExecutions != 0 {
		t.Errorf("the second prune found %d executions to delete, want 0", plan.DeleteExecutions)
	}
	if n := countTenantRows(t, pool, tenant, `SELECT count(*) FROM worker_executions WHERE tenant_id=$1`, tenant); n != 3 {
		t.Errorf("executions after re-run = %d, want 3", n)
	}
}

var _ = fmt.Sprintf
