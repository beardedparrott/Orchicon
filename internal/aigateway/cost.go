package aigateway

import apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"

// tokensPerMillion is the scaling factor for catalog prices.
const tokensPerMillion = 1_000_000

// usageCostFromCatalog prices a usage sample from catalog ModelCost.
func usageCostFromCatalog(in UsageInput, cost *apiv1.ModelCost) (float64, bool) {
	if cost == nil {
		return 0, false
	}
	usd := (float64(in.PromptTokens)*cost.Input +
		float64(in.CacheReadTokens)*cost.CacheRead +
		float64(in.CacheWriteTokens)*cost.CacheWrite +
		float64(in.CompletionTokens)*cost.Output) / tokensPerMillion
	return usd, true
}
