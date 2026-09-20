package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// newRetentionTestPool opens the sandbox-plane test DB (ORCHICON_TEST_DSN);
// tests skip when unset — the same gate every DB-backed test in internal/db
// uses.
func newRetentionTestPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed outbox retention test")
	}
	pool, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// eligiblePublishedAt is deliberately far in the past so the rows under test
// sort ahead of any residue in the shared test table and always fall inside
// the prune batch (the prune orders oldest-first).
var eligiblePublishedAt = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// insertOutboxRow writes one outbox row inside a tenant tx (RLS requires the
// row's tenant_id to match the session's app.tenant_id) and returns its ULID.
func insertOutboxRow(t *testing.T, pool *Pool, tenant, eventType string, occurredAt time.Time, publishedAt *time.Time) string {
	t.Helper()
	ctx := context.Background()
	id := newULID()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := EnqueueOutbox(ctx, ttx.Tx, OutboxRow{
		ID: id, TenantID: tenant, EventType: eventType,
		AggregateType: "execution", AggregateID: id, AggregateVer: 1,
		Payload: []byte(`{}`), OccurredAt: occurredAt,
	}); err != nil {
		t.Fatalf("enqueue outbox row: %v", err)
	}
	if publishedAt != nil {
		if _, err := ttx.Tx.Exec(ctx, `UPDATE outbox SET published_at = $1 WHERE id = $2`, *publishedAt, id); err != nil {
			t.Fatalf("mark published: %v", err)
		}
	} else if _, err := ttx.Tx.Exec(ctx, `UPDATE outbox SET occurred_at = $1 WHERE id = $2`, occurredAt, id); err != nil {
		t.Fatalf("backdate occurred_at: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit outbox row: %v", err)
	}
	return id
}

// outboxRowExists reports whether a row with the given id is still present,
// read through a tenant tx so RLS cannot make the answer vacuously false.
func outboxRowExists(t *testing.T, pool *Pool, tenant, id string) bool {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	var n int64
	if err := ttx.Tx.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count row: %v", err)
	}
	return n > 0
}

func deleteOutboxRows(t *testing.T, pool *Pool, tenants map[string][]string) {
	t.Helper()
	ctx := context.Background()
	for tenant, ids := range tenants {
		if len(ids) == 0 {
			continue
		}
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			continue
		}
		_, _ = ttx.Tx.Exec(ctx, `DELETE FROM outbox WHERE id = ANY($1)`, ids)
		_ = ttx.Commit(ctx)
	}
}

// TestPrunePublishedOutboxAcrossTenants covers the retention acceptance
// criteria that pruning is tenant-safe and RLS-verified: published rows older
// than the cutoff are deleted for EVERY tenant (the relay prunes on the
// non-tenant pool path exactly like PollOutbox/MarkPublished), while
// unpublished rows are never pruned and recently published rows survive.
func TestPrunePublishedOutboxAcrossTenants(t *testing.T) {
	pool := newRetentionTestPool(t)
	ctx := context.Background()

	const (
		tA = "tnt_outbox_retention_a"
		tB = "tnt_outbox_retention_b"
	)
	fresh := time.Now().UTC()
	old := eligiblePublishedAt

	own := map[string][]string{tA: {}, tB: {}}
	appendOwn := func(tenant, id string) { own[tenant] = append(own[tenant], id) }

	unpubOldA := insertOutboxRow(t, pool, tA, "execution.created", time.Now().UTC().Add(-3*time.Hour), nil)
	appendOwn(tA, unpubOldA)
	pubOldA := insertOutboxRow(t, pool, tA, "execution.terminated", old, &old)
	appendOwn(tA, pubOldA)
	pubFreshA := insertOutboxRow(t, pool, tA, "execution.terminated", fresh, &fresh)
	appendOwn(tA, pubFreshA)
	pubOldB := insertOutboxRow(t, pool, tB, "workflow.step_finished", old, &old)
	appendOwn(tB, pubOldB)
	unpubB := insertOutboxRow(t, pool, tB, "execution.text", fresh, nil)
	appendOwn(tB, unpubB)
	t.Cleanup(func() { deleteOutboxRows(t, pool, own) })

	n, err := pool.PrunePublishedOutbox(ctx, time.Now().UTC().Add(-1*time.Hour), 1000)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected at least the 2 old published rows to be pruned, got %d", n)
	}

	if !outboxRowExists(t, pool, tA, unpubOldA) {
		t.Errorf("unpublished row of tenant A was pruned — unpublished rows must NEVER be pruned")
	}
	if !outboxRowExists(t, pool, tB, unpubB) {
		t.Errorf("unpublished row of tenant B was pruned — unpublished rows must NEVER be pruned")
	}
	if outboxRowExists(t, pool, tA, pubOldA) {
		t.Errorf("old published row of tenant A should have been pruned")
	}
	if outboxRowExists(t, pool, tB, pubOldB) {
		t.Errorf("old published row of tenant B should have been pruned")
	}
	if !outboxRowExists(t, pool, tA, pubFreshA) {
		t.Errorf("recently published row must survive retention")
	}
}

// TestPrunePublishedOutboxRespectsBatchBound verifies the prune deletes at
// most batchLimit rows per statement — the batched-delete AC that bounds WAL
// and lock duration on an 8M-row table.
func TestPrunePublishedOutboxRespectsBatchBound(t *testing.T) {
	pool := newRetentionTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_outbox_retention_batch"

	ids := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		pub := eligiblePublishedAt
		ids = append(ids, insertOutboxRow(t, pool, tenant, "execution.text", pub, &pub))
	}
	t.Cleanup(func() { deleteOutboxRows(t, pool, map[string][]string{tenant: ids}) })

	n, err := pool.PrunePublishedOutbox(ctx, time.Now().UTC().Add(-1*time.Hour), 10)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 10 {
		t.Fatalf("prune batch must be bounded to exactly 10 rows, got %d", n)
	}
	remaining := 0
	for _, id := range ids {
		if outboxRowExists(t, pool, tenant, id) {
			remaining++
		}
	}
	if remaining != 15 {
		t.Fatalf("expected 15 of the 25 eligible rows to survive one 10-row batch, got %d", remaining)
	}
}

// TestCountAndOldestUnpublished covers the observability data path: one query
// returns both the depth and the age of the oldest unpublished row, which is
// the relay stall signal.
func TestCountAndOldestUnpublished(t *testing.T) {
	pool := newRetentionTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_outbox_retention_lag"

	now := time.Now().UTC()
	a := insertOutboxRow(t, pool, tenant, "execution.text", now.Add(-3*time.Hour), nil)
	b := insertOutboxRow(t, pool, tenant, "execution.text", now.Add(-2*time.Hour), nil)
	t.Cleanup(func() { deleteOutboxRows(t, pool, map[string][]string{tenant: {a, b}}) })

	count, oldest, err := pool.CountAndOldestUnpublished(ctx)
	if err != nil {
		t.Fatalf("count and oldest: %v", err)
	}
	if count < 2 {
		t.Fatalf("expected at least the 2 seeded unpublished rows, got %d", count)
	}
	if oldest == nil {
		t.Fatal("oldest unpublished must not be nil when unpublished rows exist")
	}
	if !oldest.Before(now.Add(-time.Hour)) {
		t.Fatalf("oldest unpublished row (%s) should be at least an hour old", oldest)
	}
}
