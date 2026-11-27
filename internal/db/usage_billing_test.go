package db_test

// usage_billing_test.go — the usage_records.billing column the plan-quota marker rides on (G2).
//
// A plan-quota turn and a genuinely free/unpriced turn both record cost_usd 0. The column is the only
// thing that tells them apart, so it has to survive the write, the read and the two aggregates the cost
// explorer renders. Gated on ORCHICON_TEST_DSN like every other DB-backed suite in this package: the
// column is a real Postgres type and a CHECK constraint, which no unit fake can vouch for.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

func TestUsageBillingColumnRoundTripsAndRollsUp(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed usage billing test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	const tnt = "tnt_usage_billing_test"
	if err := db.SeedDevTenant(ctx, pool, tnt); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	now := time.Now().UTC()
	// Two plan-quota turns and one metered turn of the same model — the shape a single Antigravity work
	// item produces when the operator switches auth method mid-flight.
	wantPlanQuota := 2
	wantTotal := 3
	ttx, err := pool.BeginTenantTx(ctx, tnt)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	for _, billing := range []string{db.BillingPlanQuota, db.BillingPlanQuota, ""} {
		row, err := db.CreateUsageRecord(ctx, ttx.Tx, db.UsageRecordRow{
			TenantID: tnt, Provider: "google", Model: "gemini-4-carbon-high",
			PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110,
			CostUSD: 0, Billing: billing, OccurredAt: now, CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("insert usage record: %v", err)
		}
		if row.Billing != billing {
			t.Errorf("CreateUsageRecord returned Billing = %q, want %q", row.Billing, billing)
		}
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	rtx, err := pool.BeginTenantTx(ctx, tnt)
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer rtx.Rollback(ctx)

	rows, err := db.ListUsageRecords(ctx, rtx.Tx, db.ListUsageRecordsFilter{TenantID: tnt})
	if err != nil {
		t.Fatalf("ListUsageRecords: %v", err)
	}
	gotPlanQuota := 0
	for _, r := range rows {
		if r.Billing == db.BillingPlanQuota {
			gotPlanQuota++
		}
	}
	if len(rows) != wantTotal || gotPlanQuota != wantPlanQuota {
		t.Fatalf("listed %d rows with %d plan-quota, want %d and %d", len(rows), gotPlanQuota, wantTotal, wantPlanQuota)
	}

	// The window total is what the cost explorer's header renders.
	total, err := db.GetCostTotal(ctx, rtx.Tx, tnt, "", "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetCostTotal: %v", err)
	}
	if total.PlanQuotaRecords != int32(wantPlanQuota) {
		t.Errorf("GetCostTotal PlanQuotaRecords = %d, want %d", total.PlanQuotaRecords, wantPlanQuota)
	}
	if total.RecordCount != int32(wantTotal) {
		t.Errorf("GetCostTotal RecordCount = %d, want %d", total.RecordCount, wantTotal)
	}
	if total.CostUSD != 0 {
		t.Errorf("GetCostTotal CostUSD = %v, want 0", total.CostUSD)
	}

	// And the per-model rollup row, which is what decides "Included in plan" vs "$x".
	rollup, err := db.GetCostRollup(ctx, rtx.Tx, tnt, db.RollupModel, "", "", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetCostRollup: %v", err)
	}
	if len(rollup) != 1 {
		t.Fatalf("GetCostRollup returned %d groups, want 1: %+v", len(rollup), rollup)
	}
	if rollup[0].PlanQuotaRecords != int32(wantPlanQuota) || rollup[0].RecordCount != int32(wantTotal) {
		t.Errorf("rollup group = %d/%d plan-quota of %d records, want %d of %d",
			rollup[0].PlanQuotaRecords, rollup[0].RecordCount, wantTotal, wantPlanQuota, wantTotal)
	}
}

// TestUsageBillingCheckConstraintRejectsUnknownRegimes pins the closed vocabulary. The UI has a rule for
// 'plan_quota' and for ” and none for anything else, so a typo must fail at the database rather than
// land a row a client renders as a bare "$0.0000".
func TestUsageBillingCheckConstraintRejectsUnknownRegimes(t *testing.T) {
	dsn := os.Getenv("ORCHICON_TEST_DSN")
	if dsn == "" {
		t.Skip("ORCHICON_TEST_DSN not set; skipping DB-backed usage billing test")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	const tnt = "tnt_usage_billing_check_test"
	if err := db.SeedDevTenant(ctx, pool, tnt); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	ttx, err := pool.BeginTenantTx(ctx, tnt)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	now := time.Now().UTC()
	if _, err := db.CreateUsageRecord(ctx, ttx.Tx, db.UsageRecordRow{
		TenantID: tnt, Provider: "google", Model: "x", TotalTokens: 1,
		Billing: "plan-quota-typo", OccurredAt: now, CreatedAt: now,
	}); err == nil {
		t.Fatal("the database accepted billing='plan-quota-typo'; the CHECK constraint must reject any " +
			"regime outside the closed vocabulary ('', 'plan_quota')")
	}
}
