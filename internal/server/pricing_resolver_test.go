package server

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/beardedparrott/orchicon/internal/aigateway"
)

// GAP 4 on an opencode-free plane: with NO discoverer (no opencode binary), the
// wired pricing resolver still answers for an anthropic model from the vendored
// catalog — and never nil-panics on the missing discoverer.
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

// The end of the chain: a claude/anthropic usage record on a plane with no
// opencode binary is priced from the catalog, cache-read at the cache-read
// rate — not from the adapter-reported number.
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
