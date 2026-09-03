package aigateway

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testMeter swaps the global meter provider for a manual-reader-backed one so
// the test can read back exactly what usageMetrics.emit recorded, and restores
// the previous provider on cleanup.
type testMeter struct {
	reader  *metric.ManualReader
	restore func()
}

func newTestMeter(t *testing.T) *testMeter {
	t.Helper()
	prev := otel.GetMeterProvider()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)
	return &testMeter{reader: reader, restore: func() { otel.SetMeterProvider(prev) }}
}

// collect returns metricName → {token_class: cumulative value} for every
// counter data point recorded since the meter provider was created.
func (tm *testMeter) collect(t *testing.T) map[string]map[string]float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tm.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]map[string]float64{}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			vals := map[string]float64{}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					vals[classValue(dp.Attributes)] = float64(dp.Value)
				}
			case metricdata.Sum[float64]:
				for _, dp := range d.DataPoints {
					vals[classValue(dp.Attributes)] = dp.Value
				}
			case metricdata.Gauge[float64]:
				for _, dp := range d.DataPoints {
					vals[classValue(dp.Attributes)] = dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					vals[classValue(dp.Attributes)] = float64(dp.Value)
				}
			default:
				t.Fatalf("unexpected metric data type for %s: %T", m.Name, m.Data)
			}
			out[m.Name] = vals
		}
	}
	return out
}

func classValue(set attribute.Set) string {
	for _, kv := range set.ToSlice() {
		if kv.Key == attribute.Key(tokenClassAttribute) {
			return kv.Value.AsString()
		}
	}
	return ""
}

func sampleRow() *db.UsageRecordRow {
	return &db.UsageRecordRow{
		TenantID:         "tnt_dev",
		ProjectID:        "prj_dev",
		WorkerID:         "worker_dev",
		Provider:         "anthropic",
		Model:            "claude-sonnet-4",
		PromptTokens:     1000,
		CacheReadTokens:  5000,
		CacheWriteTokens: 200,
		CompletionTokens: 800,
		TotalTokens:      7000,
		CostUSD:          1.5,
	}
}

func sliceSum(v []float64) float64 {
	var total float64
	for _, n := range v {
		total += n
	}
	return total
}

func mapSum(m map[string]float64) float64 {
	var total float64
	for _, v := range m {
		total += v
	}
	return total
}

func approx(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// TestSplitCostByWeightRenormalizes pins the pricing-weighted allocation: the
// shares always renormalize to the input total, and a cheap bucket (cache_read,
// priced ~10x below input) gets less than a token-proportional share.
func TestSplitCostByWeightRenormalizes(t *testing.T) {
	mult := defaultPriceMultipliers[:]
	cases := []struct {
		name   string
		total  float64
		tokens []int64
		prices []float64
	}{
		{"equal tokens, cheap cache_read", 1.5, []int64{1000, 5000, 200, 800}, mult},
		{"all buckets weighted", 0.32, []int64{10, 40, 25, 80}, []float64{1, 0.1, 1.25, 4}},
		{"single bucket", 7.0, []int64{0, 0, 0, 42}, mult},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shares := splitCostByWeight(tc.total, tc.tokens, tc.prices)
			if len(shares) != len(tc.tokens) {
				t.Fatalf("shares len = %d, want %d", len(shares), len(tc.tokens))
			}
			var wsum float64
			for i := range tc.tokens {
				wsum += float64(tc.tokens[i]) * tc.prices[i]
			}
			sum := sliceSum(shares)
			if !approx(sum, tc.total, 1e-9) {
				t.Fatalf("Σ shares = %v, want %v (must renormalize to total)", sum, tc.total)
			}
			for i := range shares {
				if shares[i] < 0 {
					t.Fatalf("share[%d] = %v < 0", i, shares[i])
				}
				want := tc.total * float64(tc.tokens[i]) * tc.prices[i] / wsum
				// The largest-weight bucket absorbs the residual, so it may
				// differ slightly from the exact proportional value.
				if !approx(shares[i], want, 1e-9) && !isMaxWeight(tc.tokens, tc.prices, i) {
					t.Fatalf("share[%d] = %v, want ~%v", i, shares[i], want)
				}
			}
		})
	}
}

func isMaxWeight(tokens []int64, prices []float64, idx int) bool {
	maxW := -1.0
	maxIdx := 0
	for i := range tokens {
		w := float64(tokens[i]) * prices[i]
		if w > maxW {
			maxW, maxIdx = w, i
		}
	}
	return idx == maxIdx
}

// TestSplitCostByWeightNoTokens guards the degenerate case: with no
// allocatable weight the whole cost lands in the first bucket (never dropped),
// and a zero total yields all-zero shares.
func TestSplitCostByWeightNoTokens(t *testing.T) {
	zeroTokens := []int64{0, 0, 0, 0}
	shares := splitCostByWeight(1.5, zeroTokens, defaultPriceMultipliers[:])
	if sh := shares[0]; !approx(sh, 1.5, 0) {
		t.Fatalf("zero-token row: share[0] = %v, want entire total 1.5", sh)
	}
	for i := 1; i < len(shares); i++ {
		if shares[i] != 0 {
			t.Fatalf("zero-token row: share[%d] = %v, want 0", i, shares[i])
		}
	}
	if all := splitCostByWeight(0, []int64{10, 20}, []float64{1, 1}); sliceSum(all) != 0 {
		t.Fatalf("zero total: shares %v, want all zero", all)
	}
}

// TestBucketPricePerToken pins both the catalog conversion (USD per 1M →
// per token) and the documented fallback multipliers.
func TestBucketPricePerToken(t *testing.T) {
	cat := bucketPricePerToken(&apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75})
	if !approx(cat[0], 3/1e6, 1e-12) || !approx(cat[1], 0.3/1e6, 1e-12) ||
		!approx(cat[2], 3.75/1e6, 1e-12) || !approx(cat[3], 15/1e6, 1e-12) {
		t.Fatalf("catalog prices wrong: %v", cat)
	}
	def := bucketPricePerToken(nil)
	if def != defaultPriceMultipliers {
		t.Fatalf("fallback prices wrong: %v", def)
	}
}

// TestEmitSplitsTokensLosslessly passes a sample with all four buckets and
// asserts one token Add per non-zero bucket whose Σ token_class equals
// TotalTokens (ADR acceptance #1, #2).
func TestEmitSplitsTokensLosslessly(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	m := newUsageMetrics(discardLogger())
	m.emit(context.Background(), sampleRow())

	got := tm.collect(t)["orchicon_tokens_consumed"]
	want := map[string]float64{"prompt": 1000, "cache_read": 5000, "cache_write": 200, "completion": 800}
	if len(got) != len(want) {
		t.Fatalf("token classes = %d (%v), want %d", len(got), got, len(want))
	}
	for class, val := range want {
		if got[class] != val {
			t.Fatalf("token_class %q = %v, want %v", class, got[class], val)
		}
	}
	if sum := mapSum(got); sum != float64(sampleRow().TotalTokens) {
		t.Fatalf("Σ token_class = %v, want %d (lossless)", sum, sampleRow().TotalTokens)
	}
}

// TestEmitSplitsCostRenormalized asserts the cost counter carries one
// pricing-weighted share per non-zero bucket and Σ shares == CostUSD
// (ADR acceptance #3).
func TestEmitSplitsCostRenormalized(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	row := sampleRow()
	m := newUsageMetrics(discardLogger())
	m.emit(context.Background(), row)

	got := tm.collect(t)["orchicon_cost_usd"]
	if len(got) != 4 {
		t.Fatalf("cost shares = %d (%v), want 4", len(got), got)
	}
	if sum := mapSum(got); !approx(sum, row.CostUSD, 1e-9) {
		t.Fatalf("Σ cost shares = %v, want %v (must renormalize to CostUSD)", sum, row.CostUSD)
	}
	// Pricing sanity: cache_read is priced ~10x below fresh input, so its
	// share of an equal-token base is smaller; completion (4x) dominates.
	if !(got["prompt"] > got["cache_read"]) {
		t.Fatalf("prompt share %v should exceed cache_read %v (cheap cache)", got["prompt"], got["cache_read"])
	}
	if !(got["completion"] > got["prompt"]) {
		t.Fatalf("completion share %v should dominate prompt %v", got["completion"], got["prompt"])
	}
}

// TestEmitZeroCacheDegradesToPromptCompletion checks the "no change for a
// zero-cache row" behavior: only prompt+completion buckets, no cache classes,
// and Σ shares still equals the total (ADR acceptance #4).
func TestEmitZeroCacheDegradesToPromptCompletion(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	row := &db.UsageRecordRow{
		TenantID: "tnt_dev", ProjectID: "prj_dev", WorkerID: "worker_dev",
		Provider: "anthropic", Model: "claude-sonnet-4",
		PromptTokens: 1000, CacheReadTokens: 0, CacheWriteTokens: 0,
		CompletionTokens: 800, TotalTokens: 1800, CostUSD: 0.5,
	}
	m := newUsageMetrics(discardLogger())
	m.emit(context.Background(), row)

	got := tm.collect(t)["orchicon_cost_usd"]
	if len(got) != 2 {
		t.Fatalf("zero-cache cost shares = %d (%v), want 2 (prompt+completion)", len(got), got)
	}
	if _, ok := got[tokenClassCacheRead]; ok {
		t.Fatalf("cache_read unexpectedly present for zero-cache row: %v", got)
	}
	if _, ok := got[tokenClassCacheWrite]; ok {
		t.Fatalf("cache_write unexpectedly present for zero-cache row: %v", got)
	}
	if sum := mapSum(got); !approx(sum, row.CostUSD, 1e-9) {
		t.Fatalf("Σ cost = %v, want %v", sum, row.CostUSD)
	}
}

// TestEmitReasoningExcluded asserts ReasoningTokens is never emitted as its
// own class nor folded into any bucket total (ADR acceptance #6).
func TestEmitReasoningExcluded(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	row := &db.UsageRecordRow{
		TenantID: "tnt_dev", ProjectID: "prj_dev", WorkerID: "worker_dev",
		Provider: "anthropic", Model: "claude-sonnet-4",
		PromptTokens: 100, CacheReadTokens: 0, CacheWriteTokens: 0,
		CompletionTokens: 50, ReasoningTokens: 9999, TotalTokens: 150, CostUSD: 0.1,
	}
	m := newUsageMetrics(discardLogger())
	m.emit(context.Background(), row)

	tok := tm.collect(t)["orchicon_tokens_consumed"]
	if _, ok := tok["reasoning"]; ok {
		t.Fatalf("reasoning emitted as its own class: %v", tok)
	}
	if sum := mapSum(tok); sum != float64(row.TotalTokens) {
		t.Fatalf("Σ token_class = %v, want %d (reasoning must not be added)", sum, row.TotalTokens)
	}
}

// TestEmitModelCostPricedPath drills the priced path: when real catalog
// prices are supplied, the split still renormalizes to CostUSD (they defeat
// the default-multiplier fallback), so a sum() alarm never fires.
func TestEmitModelCostPricedPath(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	row := sampleRow()
	m := newUsageMetrics(discardLogger())
	cost := &apiv1.ModelCost{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75}
	m.emit(context.Background(), row, cost)

	got := tm.collect(t)["orchicon_cost_usd"]
	if sum := mapSum(got); !approx(sum, row.CostUSD, 1e-9) {
		t.Fatalf("priced-path Σ cost shares = %v, want %v", sum, row.CostUSD)
	}
}

// TestRecordPrefixCacheEmitsHitMissAndRate drills the D3 prefix-cache
// metric: the session-terminal segment of the native adapter's cache
// telemetry emits the hit/miss counters and the hit-rate gauge, carrying
// tenant/execution/worker attrs, and is a no-op when Turns==0.
func TestRecordPrefixCacheEmitsHitMissAndRate(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	rec := NewUsageRecorder(nil, discardLogger())
	if err := rec.RecordPrefixCache(context.Background(), "tnt_dev", "prj_dev", "worker_dev", "exec_dev", 10, 7, 3, 5000, 200); err != nil {
		t.Fatalf("RecordPrefixCache: %v", err)
	}

	gotHits := tm.collect(t)["orchicon_prefix_cache_hit_total"]
	gotMiss := tm.collect(t)["orchicon_prefix_cache_miss_total"]
	gotRate := tm.collect(t)["orchicon_prefix_cache_hit_rate"]
	if gotHits[""] != 7 {
		t.Fatalf("prefix_cache_hit_total = %v, want 7", gotHits)
	}
	if gotMiss[""] != 3 {
		t.Fatalf("prefix_cache_miss_total = %v, want 3", gotMiss)
	}
	if gotRate[""] != 0.7 {
		t.Fatalf("prefix_cache_hit_rate = %v, want 0.7 (7/10)", gotRate)
	}
}

// TestRecordPrefixCacheNoOpOnZeroTurns guards the degenerate case: a session
// that never produced a provider turn must not emit the prefix-cache family.
func TestRecordPrefixCacheNoOpOnZeroTurns(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	rec := NewUsageRecorder(nil, discardLogger())
	if err := rec.RecordPrefixCache(context.Background(), "tnt_dev", "prj_dev", "worker_dev", "exec_dev", 0, 0, 0, 0, 0); err != nil {
		t.Fatalf("RecordPrefixCache(zero turns): %v", err)
	}
	got := tm.collect(t)
	if _, ok := got["orchicon_prefix_cache_hit_total"]; ok {
		t.Fatalf("hit_total emitted for a zero-turn session: %v", got)
	}
	if _, ok := got["orchicon_prefix_cache_hit_rate"]; ok {
		t.Fatalf("hit_rate emitted for a zero-turn session: %v", got)
	}
}

// TestAnthropicShapeSplitsDownToOtel feeds the canonical Anthropic sample
// through UsageRecorder.Record and asserts the emitted OTel split reflects
// the cache-aware buckets (ADR acceptance #7).
func TestAnthropicShapeSplitsDownToOtel(t *testing.T) {
	tm := newTestMeter(t)
	defer tm.restore()

	usage := AnthropicUsage{
		InputTokens:              1000,
		CacheReadInputTokens:     5000,
		CacheCreationInputTokens: 200,
		OutputTokens:             800,
		ReasoningTokens:          300,
	}
	base := UsageInput{
		TenantID: "tnt_dev", ProjectID: "prj_dev", TaskID: "task_dev",
		ExecutionID: "exec_dev", WorkerID: "worker_dev",
		Provider: "anthropic", Model: "claude-sonnet-4", WorkflowRunID: "wr_dev",
		CostUSD: 1.5,
	}
	rec := NewUsageRecorder(nil, discardLogger())
	if _, err := rec.Record(context.Background(), UsageFromAnthropic(usage, base)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	tok := tm.collect(t)["orchicon_tokens_consumed"]
	wantTok := map[string]float64{"prompt": 1000, "cache_read": 5000, "cache_write": 200, "completion": 800}
	for class, val := range wantTok {
		if tok[class] != val {
			t.Fatalf("anthropic token_class %q = %v, want %v (full bucket map %v)", class, tok[class], val, tok)
		}
	}
	if _, ok := tok["reasoning"]; ok {
		t.Fatalf("anthropic reasoning emitted as its own class: %v", tok)
	}
	if sum := mapSum(tok); sum != 7000 {
		t.Fatalf("anthropic Σ token_class = %v, want 7000", sum)
	}
	cost := tm.collect(t)["orchicon_cost_usd"]
	if sum := mapSum(cost); !approx(sum, base.CostUSD, 1e-9) {
		t.Fatalf("anthropic Σ cost shares = %v, want %v", sum, base.CostUSD)
	}
}
