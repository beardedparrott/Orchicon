package aigateway

import apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"

// tokensPerMillion is the scaling factor for catalog prices. opencode
// reports ModelCost prices per million tokens (Input/Output/CacheRead/
// CacheWrite), so computed USD divides the token-weighted sum by 1e6.
const tokensPerMillion = 1_000_000

// usageCostFromCatalog prices a usage sample from catalog ModelCost.
// Prices are per-million-tokens; returns (computedUSD, usedCatalog).
// usedCatalog=false when cost is nil (no catalog pricing → caller falls back
// to the adapter-reported cost). A genuinely free model (cost with $0 rates)
// returns (0, true) — the catalog is authoritative, including the $0 case.
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
