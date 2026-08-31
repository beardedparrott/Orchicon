package aigateway

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestUsageCostFromCatalog pins the pure pricing function: prices are
// per-million-tokens, so the token-weighted sum is divided by 1e6. A nil cost
// reports usedCatalog=false so the caller falls back to adapter cost.
func TestUsageCostFromCatalog(t *testing.T) {
	in := UsageInput{PromptTokens: 1000, CacheReadTokens: 5000, CacheWriteTokens: 200, CompletionTokens: 800}

	cost := &apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 0.5}

	usd, used := usageCostFromCatalog(in, cost)
	if !used {
		t.Fatalf("expected usedCatalog=true")
	}
	// (1000*3 + 5000*0.3 + 200*0.5 + 800*15) / 1e6 = 16600 / 1e6 = 0.0166
	if math.Abs(usd-0.0166) > 1e-12 {
		t.Fatalf("expected 0.0166, got %v", usd)
	}
}

// TestUsageCostFromCatalogNil pins the fallback signal: a nil cost means the
// catalog has no pricing, so the caller keeps the adapter-reported cost.
func TestUsageCostFromCatalogNil(t *testing.T) {
	usd, used := usageCostFromCatalog(UsageInput{PromptTokens: 100}, nil)
	if used {
		t.Fatalf("expected usedCatalog=false when cost is nil")
	}
	if usd != 0 {
		t.Fatalf("expected 0, got %v", usd)
	}
}

// TestUsageCostFromCatalogFreeModel pins that a genuinely free model is
// authoritative at $0: the catalog returns (0, true), so an adapter-reported
// nonzero is never substituted for a free model.
func TestUsageCostFromCatalogFreeModel(t *testing.T) {
	in := UsageInput{PromptTokens: 100, CompletionTokens: 50}
	cost := &apiv1.ModelCost{} // all rates $0
	usd, used := usageCostFromCatalog(in, cost)
	if !used {
		t.Fatalf("expected usedCatalog=true for a $0 model")
	}
	if usd != 0 {
		t.Fatalf("expected 0, got %v", usd)
	}
}

// TestRecordPricingCatalogAuthoritative pins that a wired pricing resolver with
// catalog pricing overrides the adapter-reported cost, and that the row carries
// the catalog cost (both the Postgres row value and the OTel-emitted value are
// derived from it).
func TestRecordPricingCatalogAuthoritative(t *testing.T) {
	rec := NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(func(ctx context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		return &apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 0.5}, true
	})

	in := UsageInput{
		TenantID: "tnt", Provider: "anthropic", Model: "claude-sonnet-4",
		PromptTokens: 1000, CacheReadTokens: 5000, CacheWriteTokens: 200,
		CompletionTokens: 800, CostUSD: 0.05, // adapter-reported (over-counted)
	}
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if math.Abs(row.CostUSD-0.0166) > 1e-12 {
		t.Fatalf("expected catalog cost 0.0166, got %v", row.CostUSD)
	}
}

// TestRecordPricingFallback pins the fallback: when the resolver cannot match
// the model (catalog absent), the adapter-reported cost stands unchanged.
func TestRecordPricingFallback(t *testing.T) {
	rec := NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(func(ctx context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		return nil, false
	})

	in := UsageInput{
		TenantID: "tnt", Provider: "ollama-cloud", Model: "unknown-model",
		PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.42,
	}
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if row.CostUSD != 0.42 {
		t.Fatalf("expected adapter fallback cost 0.42, got %v", row.CostUSD)
	}
}

// TestRecordPricingNilResolverPinsFallback pins that without a wired resolver
// the adapter-reported cost is untouched (no behavior change for recorder
// instances without catalog wiring).
func TestRecordPricingNilResolverPinsFallback(t *testing.T) {
	rec := NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	in := UsageInput{
		TenantID: "tnt", Provider: "opencode", Model: "deepseek-v4-flash",
		PromptTokens: 100, CompletionTokens: 50, CostUSD: 0.07,
	}
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if row.CostUSD != 0.07 {
		t.Fatalf("expected adapter cost 0.07, got %v", row.CostUSD)
	}
}

// TestRecordPricingFreeModelStaysZero pins the free-model guarantee: a $0
// catalog model is authoritative — an adapter-reported nonzero cost for a
// genuinely free model must not leak into the ledger.
func TestRecordPricingFreeModelStaysZero(t *testing.T) {
	rec := NewUsageRecorder(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec.SetPricingResolver(func(ctx context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		return &apiv1.ModelCost{}, true // all rates $0
	})

	in := UsageInput{
		TenantID: "tnt", Provider: "opencode", Model: "deepseek-v4-flash-free",
		PromptTokens: 1000, CompletionTokens: 500, CostUSD: 0.09, // adapter-reported (spurious)
	}
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if row.CostUSD != 0 {
		t.Fatalf("expected free model cost 0, got %v", row.CostUSD)
	}
}
