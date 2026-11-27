package server

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/db"
)

// GAP 4 on an opencode-free plane: with NO discoverer (no opencode binary), the wired pricing
// resolver still answers for an anthropic model from the vendored catalog.
func TestPricingResolverAnswersWithoutADiscoverer(t *testing.T) {
	r := pricingResolver(nil)

	cost, ok := r(context.Background(), "anthropic", "claude-sonnet-4")
	if !ok || cost == nil {
		t.Fatalf("pricingResolver(nil) returned no pricing for anthropic/claude-sonnet-4 — the catalog must answer with no discoverer (Gap 4)")
	}
	if cost.CacheRead <= 0 {
		t.Fatalf("cache-read rate = %v, want the catalog's cache-aware rate (> 0)", cost.CacheRead)
	}
	if cost.CacheRead >= cost.Input {
		t.Fatalf("cache-read rate = %v >= input rate %v — the cache-aware distinction is lost", cost.CacheRead, cost.Input)
	}

	// Still fails CLOSED for a model the catalog does not carry — no discoverer,
	// no guess, and crucially no nil-panic.
	if c, ok := r(context.Background(), "anthropic", "no-such-model"); ok || c != nil {
		t.Fatalf("pricingResolver(nil) answered for an unknown model: (%v, %v)", c, ok)
	}
	if c, ok := r(context.Background(), "unknown-provider", "whatever"); ok || c != nil {
		t.Fatalf("pricingResolver(nil) answered for an unknown provider: (%v, %v)", c, ok)
	}
}

// The end of the chain: a claude/anthropic usage record on a plane with no opencode binary is
// priced from the catalog.
func TestUsageRecordPricedFromCatalogWithoutADiscoverer(t *testing.T) {
	rec := aigateway.NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(pricingResolver(nil)) // no opencode binary on this plane

	in := aigateway.UsageInput{
		TenantID: "tnt", Provider: "anthropic", Model: "claude-sonnet-4",
		PromptTokens: 1000, CacheReadTokens: 1_000_000, CompletionTokens: 1000,
		CostUSD: 99.0, // adapter-reported, deliberately absurd
	}
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Catalog rates for anthropic/claude-sonnet-4: in 3.0, out 15.0, cache-read
	// 0.3 per 1M → (1000*3 + 1000000*0.3 + 1000*15) / 1e6 = 0.318.
	if math.Abs(row.CostUSD-0.318) > 1e-9 {
		t.Fatalf("recorded cost = %v, want 0.318 (catalog cache-aware pricing)", row.CostUSD)
	}
}

// TestAntigravityUsagePricedByRegime is AC5's cost half at the recorder, which is the ONLY place the two
// pricing regimes can be told apart: the same tokens, the same model, and the ONLY difference is the
// billing marker the mapper set from the session's auth method.
//
// Plan quota (oauth-personal / oauth-business): the operator's subscription already paid for the turn, so
// cost is 0 AND catalog pricing must be SKIPPED — pricing it would fabricate a charge. Metered
// (gemini-api-key / agent-platform): the catalog price applies and must be non-zero, or an API-key run
// would look free.
func TestAntigravityUsagePricedByRegime(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const model = "gemini-4-carbon-high" // $2.00 in / $10.00 out per 1M on Google's pricing page
	in := aigateway.UsageInput{
		TenantID:         "tnt",
		Provider:         "google",
		Model:            model,
		PromptTokens:     1_000_000,
		CompletionTokens: 1_000_000,
	}

	metered := in
	metered.Billing = "" // an API-key method: metered
	rec := aigateway.NewUsageRecorder(nil, log)
	rec.SetPricingResolver(pricingResolver(nil))
	row, err := rec.Record(context.Background(), metered)
	if err != nil {
		t.Fatalf("Record (metered): %v", err)
	}
	// 1M in @ $2.00 + 1M out @ $10.00 = $12.00.
	if math.Abs(row.CostUSD-12.0) > 1e-9 {
		t.Fatalf("metered google/%s cost = %v, want 12.0 (from the catalog price) — an API-key run must not "+
			"look free", model, row.CostUSD)
	}
	if row.Billing != "" {
		t.Errorf("metered row Billing = %q, want \"\"", row.Billing)
	}

	plan := in
	plan.Billing = db.BillingPlanQuota // oauth-personal / oauth-business
	rec2 := aigateway.NewUsageRecorder(nil, log)
	rec2.SetPricingResolver(pricingResolver(nil))
	row2, err := rec2.Record(context.Background(), plan)
	if err != nil {
		t.Fatalf("Record (plan quota): %v", err)
	}
	if row2.CostUSD != 0 {
		t.Errorf("plan-quota google/%s cost = %v, want 0 — catalog pricing must be SKIPPED for a turn the "+
			"operator's plan already paid for", model, row2.CostUSD)
	}
	if row2.Billing != db.BillingPlanQuota {
		t.Errorf("plan-quota row Billing = %q, want %q", row2.Billing, db.BillingPlanQuota)
	}
}
