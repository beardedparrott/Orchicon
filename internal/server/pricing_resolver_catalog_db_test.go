package server

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"

	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
)

// pricing_resolver_catalog_db_test.go is the DB-backed half of the Gap-4 proof:
// on this plane there is NO opencode binary, so the pricing resolver's ONLY
// source is the offline vendored catalog. A claude/anthropic sample recorded
// through the REAL recorder into REAL Postgres must land with the catalog-priced
// cost_usd (cache reads at the cache-read rate) and the four cache-aware
// buckets — i.e. the exact values `get_usage` / the cost explorer read back.
//
// FIXTURES ONLY: no live Claude session, no spend. Guarded by ORCHICON_TEST_DSN
// like the other DB-backed tests, so it is skipped in DSN-less CI.
func TestClaudeUsageRowIsCatalogPricedInPostgres(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed claude usage test")
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

	const tenant = "tnt_claude_qa"
	const execID = "exec-claude-qa-1"

	write := func(fn func(ctx context.Context, ttx *db.TenantTx)) {
		t.Helper()
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer ttx.Rollback(ctx)
		fn(ctx, ttx)
		if err := ttx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Idempotent workspace: the tenant the row's RLS scope needs, and a clean
	// slate for this execution id.
	write(func(ctx context.Context, ttx *db.TenantTx) {
		if _, err := ttx.Exec(ctx, `INSERT INTO tenants (id, slug, name, status) VALUES ($1,$1,'Claude QA','active') ON CONFLICT DO NOTHING`, tenant); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
		if _, err := ttx.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id=$1 AND execution_id=$2`, tenant, execID); err != nil {
			t.Fatalf("clean usage_records: %v", err)
		}
	})
	t.Cleanup(func() {
		ttx, err := pool.BeginTenantTx(ctx, tenant)
		if err != nil {
			return
		}
		defer ttx.Rollback(ctx)
		_, _ = ttx.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id=$1 AND execution_id=$2`, tenant, execID)
		_ = ttx.Commit(ctx)
	})

	// The PRODUCTION resolver body, no discoverer: catalog-backed only.
	rec := aigateway.NewUsageRecorder(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(func(_ context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		return catalogModelCost(provider, model)
	})

	row, err := rec.Record(ctx, aigateway.UsageFromAnthropic(
		aigateway.AnthropicUsage{
			InputTokens:              1000,
			CacheReadInputTokens:     5000,
			CacheCreationInputTokens: 200,
			OutputTokens:             800,
		},
		aigateway.UsageInput{
			TenantID: tenant, ProjectID: "proj-claude-qa", ExecutionID: execID,
			Provider: "anthropic", Model: "claude-sonnet-4", AdapterKind: "claude",
			// An adapter-reported cost the catalog price MUST override.
			CostUSD: 999,
		},
	))
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	want := (1000*3.0 + 5000*0.3 + 200*3.75 + 800*15.0) / 1e6 // 0.01725
	if math.Abs(row.CostUSD-want) > 1e-12 {
		t.Fatalf("recorded cost_usd = %v, want the catalog-priced %v", row.CostUSD, want)
	}

	// Read back through the SAME query path get_usage uses.
	ttx, err := pool.BeginTenantTx(ctx, tenant)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{TenantID: tenant, ExecutionID: execID, PageSize: 10})
	if err != nil {
		t.Fatalf("ListUsageRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	// Attribution parity on the read path: provider/model are what `get_usage`
	// and the cost explorer surface. NOTE: ListUsageRecords does not SELECT the
	// adapter_kind column (a pre-existing read-path gap that affects the
	// opencode rows identically — the INSERT does persist it), so it is not
	// asserted here.
	if got.Provider != "anthropic" || got.Model != "claude-sonnet-4" {
		t.Fatalf("attribution = provider %q model %q, want anthropic/claude-sonnet-4", got.Provider, got.Model)
	}
	if got.PromptTokens != 1000 || got.CacheReadTokens != 5000 || got.CacheWriteTokens != 200 || got.CompletionTokens != 800 {
		t.Fatalf("persisted buckets = %+v, want 1000/5000/200/800 (never flattened)", got)
	}
	if got.TotalTokens != 7000 {
		t.Fatalf("TotalTokens = %d, want 7000", got.TotalTokens)
	}
	if math.Abs(got.CostUSD-want) > 1e-12 {
		t.Fatalf("persisted cost_usd = %v, want %v (adapter 999 must be overridden)", got.CostUSD, want)
	}
}
