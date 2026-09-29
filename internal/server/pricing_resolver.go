package server

import (
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// catalogModelCost converts the OFFLINE vendored catalog's pricing for a
// provider/bare-model-id into the gateway's ModelCost shape.
//
// It is the Gap-4 fallback the pricing resolver uses when the opencode
// discoverer is absent (the orchicon-only plane this claude/anthropic adapter
// exists to enable) or when the discoverer yields no match. Without it, a
// claude usage record on an opencode-free plane keeps the adapter-reported cost
// and the shared cache-aware cost gate stays blind.
//
// It sources from the SINGLE shared lookup (orchicon.CatalogModelCost, which
// the model-picker path also consumes — do NOT add a second one) and fails
// CLOSED: a genuine catalog miss (unknown model, wrong provider, or an entry
// with no pricing) returns (nil, false) so the adapter-reported cost stands.
// It never panics and never fabricates a price.
func catalogModelCost(provider, model string) (*apiv1.ModelCost, bool) {
	p, ok := orchicon.CatalogModelCost(provider, model)
	if !ok || p == nil {
		return nil, false
	}
	return &apiv1.ModelCost{
		Input:      p.InputPerM,
		Output:     p.OutputPerM,
		CacheRead:  p.CacheReadPerM,
		CacheWrite: p.CacheWritePerM,
	}, true
}
