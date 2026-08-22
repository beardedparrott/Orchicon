package telemetry_test

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	assets "github.com/beardedparrott/orchicon"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/migrate"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestGetDashboardAllTimeTotalsMatchCostExplorer pins the invariant that
// the telemetry Overview (the default page) and the Cost Explorer agree
// on all-time totals when no window is requested. The Cost Explorer
// (AIGateway.GetCost) is unbounded by default while GetDashboard used to
// default to last-24h, which made the Overview total (~5M) disagree with
// the Cost Explorer per-model sum (~38M). It asserts:
//
//  1. GetDashboard's summary equals GetCostTotal with a zero window
//     (the Cost Explorer's "Window total") — the cross-surface agreement.
//  2. GetDashboard's summary equals the sum of the per-model roll-up
//     rows (the Cost Explorer's per-model breakdown sum).
//
// Guarded by ORCHICON_TEST_DSN like the db cost tests:
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/telemetry/ -run TestGetDashboardAllTimeTotalsMatchCostExplorer -v
func TestGetDashboardAllTimeTotalsMatchCostExplorer(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed telemetry cost test")
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

	const tenantID = "tnt_dash_test"
	seedDashboardCostData(t, pool, tenantID)

	svc := telemetry.NewService(pool, nil, nil)
	req := connect.NewRequest(&apiv1.GetDashboardRequest{})
	resp, err := svc.GetDashboard(tenant.WithID(ctx, tenantID), req)
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}
	summary := resp.Msg.GetSummary()
	if summary == nil {
		t.Fatal("GetDashboard returned no summary")
	}

	// Independent ground truth from the same SQL path the Cost Explorer
	// uses (GetCost → GetCostRollup / GetCostTotal), unbounded.
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	total, err := db.GetCostTotal(ctx, ttx.Tx, tenantID, "", "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetCostTotal: %v", err)
	}
	modelRows, err := db.GetCostRollup(ctx, ttx.Tx, tenantID, db.RollupModel, "", "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetCostRollup: %v", err)
	}
	var modelTokens int64
	for i := range modelRows {
		modelTokens += modelRows[i].TotalTokens
	}

	// 1. Overview summary == Cost Explorer window total (unbounded).
	if summary.GetTotalTokens() != total.TotalTokens {
		t.Errorf("dashboard summary tokens %d != GetCostTotal unbounded %d (last-24h default regression?)",
			summary.GetTotalTokens(), total.TotalTokens)
	}
	if summary.GetTotalCostUsd() != total.CostUSD {
		t.Errorf("dashboard summary cost %.4f != GetCostTotal unbounded %.4f",
			summary.GetTotalCostUsd(), total.CostUSD)
	}

	// 2. Overview summary == per-model panel sum (the Cost Explorer's
	//    per-model breakdown), which the frontend renders as its own
	//    independent total.
	if summary.GetTotalTokens() != modelTokens {
		t.Errorf("dashboard summary tokens %d != sum of per-model rollup %d",
			summary.GetTotalTokens(), modelTokens)
	}
	var panelCost float64
	for _, p := range resp.Msg.GetPanels() {
		if len(p.GetPoints()) > 0 {
			panelCost += p.GetPoints()[0].GetValue()
		}
	}
	if summary.GetTotalCostUsd() != panelCost {
		t.Errorf("dashboard summary cost %.4f != sum of per-model panels %.4f",
			summary.GetTotalCostUsd(), panelCost)
	}

	// 3. The seeded data spans >24h, so a last-24h default would have
	//    produced a strictly smaller total — the regression trigger.
	if total.TotalTokens == 0 {
		t.Fatal("seed produced no usage rows; test is vacuous")
	}
}

// seedDashboardCostData inserts usage records across three models with
// occurred_at spanning well beyond 24h (the former GetDashboard default
// window). All rows carry the dedicated test tenant so the tenant-scoped
// tx + RLS backstop see them. Idempotent: wipes any rows a previous run
// left behind.
func seedDashboardCostData(t *testing.T, pool *db.Pool, tenantID string) {
	t.Helper()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)

	if _, err := ttx.Exec(ctx, `DELETE FROM usage_records WHERE tenant_id = $1 AND id LIKE 'rec-dash-%'`, tenantID); err != nil {
		t.Fatalf("clean stale dashboard seed: %v", err)
	}

	now := time.Now().UTC()
	rows := []struct {
		id     string
		model  string
		tokens int64
		cost   float64
		at     time.Time
	}{
		{"rec-dash-1", "opencode/deepseek-v4-flash-free", 20_000_000, 0.0, now.Add(-2 * time.Hour)},
		{"rec-dash-2", "opencode/gpt-4o", 15_000_000, 12.50, now.Add(-6 * time.Hour)},
		{"rec-dash-3", "opencode/claude-3.5-sonnet", 3_000_000, 2.00, now.Add(-48 * time.Hour)},
	}
	for _, r := range rows {
		if _, err := ttx.Exec(ctx,
			`INSERT INTO usage_records (id, tenant_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, occurred_at, created_at)
			 VALUES ($1, $2, 'opencode', $3, $4, $5, $6, $7, $8, $8)`,
			r.id, tenantID, r.model, r.tokens/2, r.tokens/2, r.tokens, r.cost, r.at,
		); err != nil {
			t.Fatalf("seed dashboard usage record %s: %v", r.id, err)
		}
	}

	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit dashboard seed: %v", err)
	}
}
