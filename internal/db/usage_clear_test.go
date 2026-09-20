package db_test

// DB-backed tests for the Ask conversation delete de-link
// (ClearUsageSessionIDs). Guarded by ORCHICON_TEST_DSN like the other
// DB-backed cost tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/db/ -run TestClearUsageSessionIDs -v
//
// The contract under test is deliberately asymmetric: DE-LINK the pointer,
// KEEP the money. Deleting these rows would retroactively rewrite Cost
// Explorer / Telemetry history, so the assertion that the rows SURVIVE is as
// important as the assertion that session_id is cleared.

import (
	"context"
	"os"
	"testing"
	"time"

	assets "github.com/beardedparrott/orchicon"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

func TestClearUsageSessionIDsDelinksKeepsRows(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed usage de-link test")
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
	convID := "delink-conv-" + db.NewID()
	sessionID := "orchicon-ask:" + convID

	// Two attribution styles must BOTH de-link: the canonical Ask attribution
	// (session_id = conversation id, what recordTurnUsage writes) and a
	// session-id attribution (defensive, for a session-ful adapter).
	var ids []string
	for _, sid := range []string{convID, sessionID} {
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		row, err := db.CreateUsageRecord(ctx, ttx.Tx, db.UsageRecordRow{
			TenantID:     tenant,
			AdapterKind:  "orchicon",
			SessionID:    sid,
			Provider:     "deepseek",
			Model:        "deepseek-flash",
			PromptTokens: 1234,
			CostUSD:      0.5,
			OccurredAt:   time.Now().UTC(),
		})
		if err != nil {
			ttx.Rollback(ctx)
			t.Fatalf("create usage record (%s): %v", sid, err)
		}
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		ids = append(ids, row.ID)
	}
	t.Cleanup(func() {
		c := context.Background()
		ttx, err := pool.BeginTenantTx(c, tenant)
		if err != nil {
			return
		}
		defer ttx.Rollback(c)
		for _, id := range ids {
			_, _ = ttx.Tx.Exec(c, `DELETE FROM usage_records WHERE tenant_id = $1 AND id = $2`, tenant, id)
		}
		_ = ttx.Commit(c)
	})

	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	n, err := db.ClearUsageSessionIDs(ctx, ttx.Tx, tenant, convID, sessionID)
	if err != nil {
		t.Fatalf("clear usage session ids: %v", err)
	}
	if n != 2 {
		t.Errorf("de-linked %d rows, want 2 (both attribution styles)", n)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The rows SURVIVE with session_id cleared — the ledger keeps the money.
	check, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin check tx: %v", err)
	}
	defer check.Rollback(ctx)
	for _, id := range ids {
		var sid string
		var prompt int64
		var cost float64
		if err := check.Tx.QueryRow(ctx,
			`SELECT session_id, prompt_tokens, cost_usd FROM usage_records WHERE tenant_id = $1 AND id = $2`,
			tenant, id).Scan(&sid, &prompt, &cost); err != nil {
			t.Fatalf("row %s must survive the de-link: %v", id, err)
		}
		if sid != "" {
			t.Errorf("row %s session_id = %q, want cleared", id, sid)
		}
		if prompt != 1234 || cost != 0.5 {
			t.Errorf("row %s lost its money: prompt=%d cost=%v", id, prompt, cost)
		}
	}
}

func TestClearUsageSessionIDsEmptyIDsNoop(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed usage de-link test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// A legacy conversation with no session id still de-links by conversation
	// id; with NO usable id at all the call is a no-op that cannot silently
	// clear every row (an empty-string id must never become a mass update).
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	n, err := db.ClearUsageSessionIDs(ctx, ttx.Tx, "tnt_dev", "", "")
	if err != nil {
		t.Fatalf("clear with empty ids: %v", err)
	}
	if n != 0 {
		t.Errorf("empty ids de-linked %d rows, want 0 (must never mass-clear)", n)
	}
}
