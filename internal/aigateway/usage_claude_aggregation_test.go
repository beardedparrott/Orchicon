package aigateway

import (
	"context"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// usage_claude_aggregation_test.go is the MANDATORY fixtures-only test for
// claude telemetry: a canned Anthropic `result` usage object is routed through
// the SHARED UsageFromAnthropic transform and the SHARED UsageRecorder, and the
// recorded row must carry all FOUR buckets DISTINCTLY (never a flattened
// single token/cost total) with a cost whose cache-read component is priced at
// the CACHE-READ rate. NO live Claude session is involved.

// claudeCatalogCost mirrors the authored anthropic/claude-sonnet-4 catalog
// rates. Kept literal (rather than importing internal/orchicon) so this test
// documents exactly the cache rates a cache read is priced at while the
// gateway package stays provider-agnostic.
func claudeCatalogCost() *apiv1.ModelCost {
	return &apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
}

// TestClaudeUsageAggregationFixtures asserts shape + cache buckets +
// cache-aware cost + the per-token-class OTel split + claude adapter parity.
func TestClaudeUsageAggregationFixtures(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	rec := NewUsageRecorder(nil, discardLogger())
	rec.SetPricingResolver(func(context.Context, string, string) (*apiv1.ModelCost, bool) {
		return claudeCatalogCost(), true
	})

	// Canned per-turn aggregate exactly as Claude Code reports it on the
	// terminal `result` line.
	wire := AnthropicUsage{
		InputTokens:              1000,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 200,
		OutputTokens:             800,
	}
	in := UsageFromAnthropic(wire, UsageInput{
		TenantID:    "tnt",
		ExecutionID: "exec-1",
		Provider:    "anthropic",
		Model:       "claude-sonnet-4",
		AdapterKind: "claude",
		// An adapter-reported cost the catalog price must override.
		CostUSD: 999,
	})
	row, err := rec.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// 1. Four DISTINCT buckets, attributed to the execution — never flattened.
	if row.PromptTokens != 1000 || row.CacheReadTokens != 5000 || row.CacheWriteTokens != 200 || row.CompletionTokens != 800 {
		t.Fatalf("buckets = %+v, want prompt=1000 cache_read=5000 cache_write=200 completion=800", row)
	}
	if row.TotalTokens != 7000 {
		t.Fatalf("TotalTokens = %d, want 7000 (P+CR+CW+Co)", row.TotalTokens)
	}
	if row.ExecutionID != "exec-1" || row.AdapterKind != "claude" || row.Provider != "anthropic" || row.Model != "claude-sonnet-4" {
		t.Fatalf("attribution = exec=%q adapter=%q provider=%q model=%q", row.ExecutionID, row.AdapterKind, row.Provider, row.Model)
	}

	// 2. Cache-aware cost: the catalog formula weights all four buckets
	//    (cost.go:19-22), cache_read at the CACHE-READ rate (0.3/1M).
	wantCost := (1000*3.0 + 5000*0.3 + 200*3.75 + 800*15.0) / 1e6
	if !approx(row.CostUSD, wantCost, 1e-12) {
		t.Fatalf("CostUSD = %v, want the catalog-priced %v (adapter-reported 999 must be overridden, cache_read at 0.3/1M)", row.CostUSD, wantCost)
	}

	all := tm.collect(t)

	// 3. The OTel token split carries every cache bucket distinctly.
	tok := all["orchicon_tokens_consumed"]
	wantTok := map[string]float64{
		tokenClassPrompt:     1000,
		tokenClassCacheRead:  5000,
		tokenClassCacheWrite: 200,
		tokenClassCompletion: 800,
	}
	for class, want := range wantTok {
		if tok[class] != want {
			t.Errorf("orchicon_tokens_consumed[%s] = %v, want %v", class, tok[class], want)
		}
	}
	if sum := mapSum(tok); sum != 7000 {
		t.Fatalf("Σ token_class = %v, want 7000 (lossless)", sum)
	}

	// 4. The COST split prices each class at its OWN catalog rate — the literal
	//    "cache-read at the cache-read rate" acceptance. Each share equals
	//    tokens*rate/1e6 because the split renormalizes to the catalog-priced
	//    total, which IS the weighted sum.
	cost := all["orchicon_cost_usd"]
	wantShares := map[string]float64{
		tokenClassPrompt:     1000 * 3.0 / 1e6,
		tokenClassCacheRead:  5000 * 0.3 / 1e6,
		tokenClassCacheWrite: 200 * 3.75 / 1e6,
		tokenClassCompletion: 800 * 15.0 / 1e6,
	}
	for class, want := range wantShares {
		if !approx(cost[class], want, 1e-12) {
			t.Errorf("orchicon_cost_usd[%s] = %v, want %v (per-class catalog rate)", class, cost[class], want)
		}
	}
	if sum := mapSum(cost); !approx(sum, wantCost, 1e-12) {
		t.Fatalf("Σ orchicon_cost_usd = %v, want %v", sum, wantCost)
	}

	// 5. Claude adapter parity on the emitted points.
	if !emittedAdapterKind(t, tm.reader, "claude") {
		t.Fatal("OTel data points did not carry adapter_kind=claude parity attribute")
	}
}

// emittedAdapterKind reports whether any emitted int/float data point carries
// adapter_kind=<want>.
func emittedAdapterKind(t *testing.T, reader *metric.ManualReader, want string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, mt := range scope.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					if attrHas(dp.Attributes, "adapter_kind", want) {
						return true
					}
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					if attrHas(dp.Attributes, "adapter_kind", want) {
						return true
					}
				}
			}
		}
	}
	return false
}
