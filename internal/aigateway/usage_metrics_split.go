package aigateway

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"go.opentelemetry.io/otel/attribute"
)

// tokenClass is the closed set of canonical usage-sample buckets used to
// split orchicon_tokens_consumed / orchicon_cost_usd by cache class (ADR:
// otel-metrics-cache-aware-token-cost-counters.md). The values are the
// single source of truth shared by emit and the tests; the set is fixed and
// bounded so the metric family's cardinality grows by at most 4x.
const (
	tokenClassAttribute  = "token_class"
	tokenClassPrompt     = "prompt"
	tokenClassCacheRead  = "cache_read"
	tokenClassCacheWrite = "cache_write"
	tokenClassCompletion = "completion"
)

// usageBucketClasses is the canonical bucket order for emit and the split.
// It matches the four-bucket TotalTokens partition
// (Prompt + CacheRead + CacheWrite + Completion).
var usageBucketClasses = [...]string{
	tokenClassPrompt,
	tokenClassCacheRead,
	tokenClassCacheWrite,
	tokenClassCompletion,
}

// defaultPriceMultipliers are the documented fallback per-token weights used
// when no real ModelCost catalog price is available (ADR "Consequences"):
// cache_read=0.1x input (cheap, served from cache), cache_write=1.25x input
// (premium, newly stored), prompt=1x input, completion=output ~4x input.
// Only the ratios matter — splitCostByWeight renormalizes — so these are
// unit-consistent relative weights, not USD-per-token.
var defaultPriceMultipliers = [...]float64{
	1.0,  // prompt
	0.1,  // cache_read
	1.25, // cache_write
	4.0,  // completion
}

// bucketTokens returns the four canonical token counts in usageBucketClasses
// order. ReasoningTokens is deliberately excluded: it is a sub-bucket of
// completion, not additive to TotalTokens, and is never emitted as its own
// class (ADR acceptance #6).
func bucketTokens(r *db.UsageRecordRow) [4]int64 {
	return [4]int64{
		r.PromptTokens,
		r.CacheReadTokens,
		r.CacheWriteTokens,
		r.CompletionTokens,
	}
}

// bucketPricePerToken resolves per-token prices for the four canonical
// buckets in usageBucketClasses order. When modelCost is non-nil, catalog
// prices (USD per 1M tokens, converted to USD per token) are used; otherwise
// the documented default multipliers are the fallback.
func bucketPricePerToken(modelCost *apiv1.ModelCost) [4]float64 {
	if modelCost != nil {
		return [4]float64{
			modelCost.Input / 1e6,
			modelCost.CacheRead / 1e6,
			modelCost.CacheWrite / 1e6,
			modelCost.Output / 1e6,
		}
	}
	return defaultPriceMultipliers
}

// splitCostByWeight renormalizes total across buckets proportionally to
// tokens[i]*pricePerToken[i] (the pricing-weighted allocation the ADR
// specifies). It returns one share per bucket, always non-negative, and the
// shares always sum to exactly total: the largest-weight bucket absorbs any
// floating-point residual. A zero-weight bucket (0 tokens, or a zero price
// such as a free model) gets share 0.
//
// The special case where there is no allocatable weight (all buckets have
// zero tokens, or all prices are zero) but total != 0 puts the whole total
// in the first bucket so no cost is dropped and a sum() alarm never breaks.
func splitCostByWeight(total float64, tokens []int64, pricePerToken []float64) []float64 {
	n := len(tokens)
	shares := make([]float64, n)
	if n == 0 {
		return shares
	}
	weights := make([]float64, n)
	var wsum float64
	for i := 0; i < n; i++ {
		w := float64(tokens[i]) * pricePerToken[i]
		weights[i] = w
		wsum += w
	}
	if wsum == 0 || total == 0 {
		if total != 0 {
			shares[0] = total
		}
		return shares
	}
	maxIdx := 0
	for i := 1; i < n; i++ {
		if weights[i] > weights[maxIdx] {
			maxIdx = i
		}
	}
	alloc := 0.0
	for i := 0; i < n; i++ {
		if i == maxIdx {
			continue
		}
		shares[i] = total * weights[i] / wsum
		alloc += shares[i]
	}
	shares[maxIdx] = total - alloc
	return shares
}

// withTokenClass returns a copy of base with the token_class attribute
// appended. A fresh slice is allocated each call so repeated appends in the
// emit loop can never alias/overwrite the underlying array.
func withTokenClass(base []attribute.KeyValue, class string) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(base)+1)
	out = append(out, base...)
	out = append(out, attribute.String(tokenClassAttribute, class))
	return out
}
