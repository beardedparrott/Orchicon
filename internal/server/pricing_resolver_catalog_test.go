package server

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/aigateway"
)

// pricing_resolver_catalog_test.go pins Gap 4: on a plane with NO opencode
// binary (modelDiscoverer == nil) the pricing resolver must still answer for an
// anthropic model, from the OFFLINE vendored catalog, and still fail CLOSED on
// a genuine catalog miss.

// TestCatalogModelCostAnswersWithoutDiscoverer asserts the catalog-backed
// fallback returns the authored cache-aware rates (no discoverer needed).
func TestCatalogModelCostAnswersWithoutDiscoverer(t *testing.T) {
	cost, ok := catalogModelCost("anthropic", "claude-sonnet-4")
	if !ok || cost == nil {
		t.Fatalf("catalogModelCost(anthropic, claude-sonnet-4) = %v, %v; want the catalog price", cost, ok)
	}
	if cost.Input != 3 || cost.Output != 15 || cost.CacheRead != 0.3 || cost.CacheWrite != 3.75 {
		t.Fatalf("cost = %+v, want input=3 output=15 cache_read=0.3 cache_write=3.75", cost)
	}
}

// TestCatalogModelCostFailsClosedWithoutDiscoverer pins the fail-CLOSED half:
// a genuine catalog miss returns (nil,false) — never a panic, never a
// fabricated price.
func TestCatalogModelCostFailsClosedWithoutDiscoverer(t *testing.T) {
	for _, c := range []struct{ provider, model string }{
		{"anthropic", "claude-not-a-real-model"},
		{"opencode", "claude-sonnet-4"},
		{"anthropic", ""},
	} {
		if cost, ok := catalogModelCost(c.provider, c.model); ok || cost != nil {
			t.Errorf("catalogModelCost(%q, %q) = %v, %v; want (nil, false)", c.provider, c.model, cost, ok)
		}
	}
}

// TestClaudeCostUsdIsCatalogPricedWithoutDiscoverer is the end-to-end Gap-4
// assertion: a claude/anthropic usage sample recorded through a resolver whose
// ONLY pricing source is the offline catalog gets a cost_usd EQUAL to the
// catalog-priced value — cache reads priced at the cache-read rate — over the
// adapter-reported cost.
func TestClaudeCostUsdIsCatalogPricedWithoutDiscoverer(t *testing.T) {
	rec := aigateway.NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(func(_ context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		return catalogModelCost(provider, model)
	})

	in := aigateway.UsageFromAnthropic(
		aigateway.AnthropicUsage{
			InputTokens:              1000,
			CacheReadInputTokens:     5000,
			CacheCreationInputTokens: 200,
			OutputTokens:             800,
		},
		aigateway.UsageInput{
			TenantID: "t1", ExecutionID: "e1",
			Provider: "anthropic", Model: "claude-sonnet-4", AdapterKind: "claude",
			// A bogus adapter-reported cost the catalog price MUST override.
			CostUSD: 999,
		},
	)
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Cache-aware formula (cost.go:19-22): input*3 + cache_read*0.3 +
	// cache_write*3.75 + output*15, all per 1M tokens.
	want := (1000*3.0 + 5000*0.3 + 200*3.75 + 800*15.0) / 1e6
	if math.Abs(row.CostUSD-want) > 1e-12 {
		t.Fatalf("cost_usd = %v, want the catalog-priced %v (adapter 999 must be overridden)", row.CostUSD, want)
	}
	if row.CacheReadTokens != 5000 || row.CacheWriteTokens != 200 {
		t.Fatalf("cache buckets = read %d write %d, want 5000/200", row.CacheReadTokens, row.CacheWriteTokens)
	}
}
