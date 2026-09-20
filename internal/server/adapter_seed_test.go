package server

// Regression test for PR-Reviewer blocker B1: the native session engine
// (adapter kind "orchicon") must be registered as a ready runtime
// adapter row (adp_orchicon_dev) so the TaskReconciler's selectAdapter
// can dispatch native workers (model_ref orchicon/<provider>/<model> —
// the ref's adapter segment is the dispatch kind).
// Before this fix the DB only ever had adp_opencode_dev, so a native
// worker found no ready adapter and the task requeued forever.
//
// DB-backed (skips without ORCHICON_TEST_DSN), following the
// approvalTestPool pattern from internal/scheduler.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestSeedNativeAdapterRegistersOrchiconKind(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed adapter test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// seedNativeAdapter is idempotent (upsert + heartbeat), so calling
	// it twice must not error and must leave exactly one ready row.
	// A real (discard) logger is required — seedDevAdapterKind writes a
	// Warn through it when the kind row already exists, and a nil logger
	// panics.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	seedNativeAdapter(ctx, pool, logger)
	seedNativeAdapter(ctx, pool, logger)

	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListReadyAdaptersByKind(ctx, ttx.Tx, "tnt_dev", "orchicon", 60*time.Second)
	if err != nil {
		t.Fatalf("ListReadyAdaptersByKind(orchicon): %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == "adp_orchicon_dev" {
			found = true
			if string(r.Capabilities) == "" {
				t.Error("adp_orchicon_dev capabilities empty; selectAdapter cannot judge capability fit")
			}
		}
	}
	if !found {
		t.Fatalf("no ready adapter of kind orchicon (rows: %+v) — native dispatch black-hole (B1)", rows)
	}
}
