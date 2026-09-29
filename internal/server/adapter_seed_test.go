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
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/adapter"
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

	// The BOOT path (seedAllDevAdapters, the only seeder) is idempotent, so
	// calling it twice must not error and must leave exactly one ready row per
	// kind. Exercising the real boot path here — rather than a per-kind wrapper —
	// is the point: this test would have caught the claude omission.
	// A real (discard) logger is required — seedDevAdapterKind writes a Warn
	// through it when the kind row already exists, and a nil logger panics.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	seedAllDevAdapters(ctx, pool, logger)
	seedAllDevAdapters(ctx, pool, logger)

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

// TestEveryBuiltinAdapterKindHasASeededRow is the structure that stops this bug
// recurring. It had already shipped TWICE (opencode's row was the only one, so a
// native worker found no adapter; then orchicon was added and claude was missed)
// — each time as "a new adapter registered its bridge and forgot the row".
//
// A bridge registration and a runtime_adapters row are DIFFERENT requirements,
// and only the second one decides whether anything dispatches:
//
//	dispatcher.Register(kind, bridge)   → routes an execution that was chosen
//	db.ListReadyAdaptersByKind(kind)    → decides a task may be chosen at all
//
// Deliberately DB-FREE, so it cannot skip: a guard that only runs when
// ORCHICON_TEST_DSN is set is a guard that is absent in most CI.
func TestEveryBuiltinAdapterKindHasASeededRow(t *testing.T) {
	seeded := map[string]string{}
	for _, s := range devAdapterSeeds() {
		seeded[s.kind] = s.id
	}

	kinds := adapter.BuiltinAdapterKinds()
	if len(kinds) < 3 {
		t.Fatalf("the builtin catalog declared only %d adapter kinds — the live kind source shrank, so this guard proves nothing", len(kinds))
	}
	for kind := range kinds {
		// A kind that is NOT an in-process bridge would legitimately have no dev
		// seed — it would heartbeat itself over the adapter lease path. None exist
		// today; adding one means adding it to this exclusion list DELIBERATELY,
		// with a comment saying why, which is the point of the guard.
		const remoteKinds = ""
		if remoteKinds == kind {
			continue
		}
		if _, ok := seeded[kind]; !ok {
			t.Errorf("adapter kind %q has no dev adapter seed: register a bridge AND seed a runtime_adapters row "+
				"(devAdapterSeeds), or add it to the remoteKinds exclusion above if it heartbeats itself. "+
				"Without a row, selectAdapter fails with `no ready adapters of kind %q` and every worker on that "+
				"adapter is undispatchable.", kind, kind)
		}
	}

	// And the caps builder must produce something: an empty payload makes the row
	// useless to a capability-aware selectAdapter.
	for _, s := range devAdapterSeeds() {
		if s.caps == nil {
			t.Errorf("%s (%s) has no capabilities builder", s.id, s.kind)
			continue
		}
		if strings.TrimSpace(s.caps()) == "" {
			t.Errorf("%s (%s) produced empty capabilities; selectAdapter cannot judge capability fit", s.id, s.kind)
		}
	}

	// The ids must be distinct, or two kinds would fight over one row.
	seen := map[string]bool{}
	for _, s := range devAdapterSeeds() {
		if seen[s.id] {
			t.Errorf("duplicate adapter id %q in devAdapterSeeds", s.id)
		}
		seen[s.id] = true
	}
}

// The claude row specifically:
// a claude worker's model_ref (claude/<provider>/<model>) resolves kind "claude",
// so selectAdapter must find adp_claude_dev.
func TestSeedClaudeAdapterRegistersClaudeKind(t *testing.T) {
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

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	seedAllDevAdapters(ctx, pool, logger)
	seedAllDevAdapters(ctx, pool, logger) // idempotent

	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	rows, err := db.ListReadyAdaptersByKind(ctx, ttx.Tx, "tnt_dev", adapter.KindClaude, 60*time.Second)
	if err != nil {
		t.Fatalf("ListReadyAdaptersByKind(claude): %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == "adp_claude_dev" {
			found = true
			if len(r.Capabilities) == 0 {
				t.Error("adp_claude_dev capabilities empty; selectAdapter cannot judge capability fit")
			}
		}
	}
	if !found {
		t.Fatal("adp_claude_dev is not a ready adapter of kind claude — every claude worker would fail with `no ready adapters of kind \"claude\"`")
	}
}
