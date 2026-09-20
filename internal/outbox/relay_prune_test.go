package outbox

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// relayTestPool opens the disposable sandbox-plane DB (ORCHICON_TEST_DSN —
// set by the runtime container). Retention/lag tests skip when unset, the same
// gate every DB-backed test in the tree uses.
func relayTestPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed outbox relay test")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// eligible is far in the past so the rows sort ahead of any residue in the
// shared table and always fall inside the oldest-first prune batch.
var eligible = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

func insertRow(t *testing.T, pool *db.Pool, tenant, eventType string, occurred time.Time, published *time.Time) string {
	t.Helper()
	ctx := context.Background()
	id := db.NewID()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if err := db.EnqueueOutbox(ctx, ttx.Tx, db.OutboxRow{
		ID: id, TenantID: tenant, EventType: eventType,
		AggregateType: "execution", AggregateID: id, AggregateVer: 1,
		Payload: []byte(`{}`), OccurredAt: occurred,
	}); err != nil {
		t.Fatalf("enqueue outbox row: %v", err)
	}
	if published != nil {
		if _, err := ttx.Tx.Exec(ctx, `UPDATE outbox SET published_at = $1 WHERE id = $2`, *published, id); err != nil {
			t.Fatalf("mark published: %v", err)
		}
	} else if _, err := ttx.Tx.Exec(ctx, `UPDATE outbox SET occurred_at = $1 WHERE id = $2`, occurred, id); err != nil {
		t.Fatalf("backdate occurred_at: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit outbox row: %v", err)
	}
	return id
}

func rowExists(t *testing.T, pool *db.Pool, tenant, id string) bool {
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

func cleanupRows(t *testing.T, pool *db.Pool, tenant string, ids []string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		return
	}
	_, _ = ttx.Tx.Exec(ctx, `DELETE FROM outbox WHERE id = ANY($1)`, ids)
	_ = ttx.Commit(ctx)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestRelayPruneOnceDeletesPublishedKeepsUnpublished: a retention-enabled relay
// removes eligible PUBLISHED rows (across batches) and never touches
// unpublished rows, including an old one.
func TestRelayPruneOnceDeletesPublishedKeepsUnpublished(t *testing.T) {
	pool := relayTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_outbox_relay_prune"

	var publishedIDs, unpublishedIDs []string
	for i := 0; i < 12; i++ {
		pub := eligible
		publishedIDs = append(publishedIDs, insertRow(t, pool, tenant, "execution.terminated", eligible, &pub))
	}
	unpublishedIDs = append(unpublishedIDs,
		insertRow(t, pool, tenant, "execution.created", eligible, nil),
		insertRow(t, pool, tenant, "execution.text", time.Now().UTC().Add(-time.Minute), nil),
	)
	all := append(append([]string{}, publishedIDs...), unpublishedIDs...)
	t.Cleanup(func() { cleanupRows(t, pool, tenant, all) })

	r := NewRelay(pool, nil, testLogger(),
		WithRetention(time.Hour), WithPruneBatch(5))

	total, err := r.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("prune once: %v", err)
	}
	if total < 12 {
		t.Fatalf("expected at least the 12 eligible published rows to be pruned, got %d", total)
	}
	for _, id := range publishedIDs {
		if rowExists(t, pool, tenant, id) {
			t.Errorf("eligible published row %s should have been pruned", id)
		}
	}
	for _, id := range unpublishedIDs {
		if !rowExists(t, pool, tenant, id) {
			t.Errorf("unpublished row %s must never be pruned", id)
		}
	}
}

// TestRelayPruneDisabledIsNoop: retention <= 0 (ORCHICON_OUTBOX_RETENTION_DAYS=0)
// disables pruning entirely.
func TestRelayPruneDisabledIsNoop(t *testing.T) {
	pool := relayTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_outbox_relay_noprune"

	pub := eligible
	id := insertRow(t, pool, tenant, "execution.terminated", eligible, &pub)
	t.Cleanup(func() { cleanupRows(t, pool, tenant, []string{id}) })

	r := NewRelay(pool, nil, testLogger()) // no WithRetention => disabled
	n, err := r.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("prune once: %v", err)
	}
	if n != 0 {
		t.Fatalf("disabled retention must delete nothing, deleted %d", n)
	}
	if !rowExists(t, pool, tenant, id) {
		t.Fatal("disabled retention must leave the eligible row in place")
	}
}

// TestRelayLagAlertFires: the stall detector fires when the oldest unpublished
// row is older than the age threshold — the 6-week silent backlog this
// observability exists to prevent.
func TestRelayLagAlertFires(t *testing.T) {
	pool := relayTestPool(t)
	ctx := context.Background()
	const tenant = "tnt_outbox_relay_lag"

	id := insertRow(t, pool, tenant, "execution.created", eligible, nil)
	t.Cleanup(func() { cleanupRows(t, pool, tenant, []string{id}) })

	r := NewRelay(pool, nil, testLogger())
	before := r.lagAlerts.Load()
	r.reportLagOnce(ctx)

	if r.lagAlerts.Load() != before+1 {
		t.Fatalf("expected the lag alert to fire for an unpublished row older than %ds, alerts=%d",
			outboxLagAlertAgeSec, r.lagAlerts.Load())
	}
	if r.oldestSec.Load() <= outboxLagAlertAgeSec {
		t.Fatalf("oldest-unpublished-age gauge should exceed the threshold, got %d", r.oldestSec.Load())
	}
	if r.lagVal.Load() < 1 {
		t.Fatalf("unpublished-depth gauge should be at least 1, got %d", r.lagVal.Load())
	}
	// The two gauges and the alert counter must actually be registered on the
	// telemetry pipeline — a stalled relay is only visible if the metrics
	// exist in the first place.
	if r.lagGauge == nil {
		t.Error("orchicon_outbox_lag gauge was not registered")
	}
	if r.oldestGauge == nil {
		t.Error("orchicon_outbox_oldest_unpublished_seconds gauge was not registered")
	}
	if r.alertCtr == nil {
		t.Error("orchicon_outbox_lag_alerts_total counter was not registered")
	}
}
