package server

import (
	"context"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/aigateway"
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

// pricingResolver builds the cost authority the AI gateway wires into the usage
// recorder (Run calls SetPricingResolver(pricingResolver(modelDiscoverer))).
//
// The order is load-bearing:
//
//  1. The opencode DISCOVERER gets the first turn when it is wired: it reflects
//     the running CLI's own catalog/probe and is the freshest source. Its cache
//     is stale-on-error, so the recording path never blocks on a subprocess.
//  2. Only then does the OFFLINE VENDORED CATALOG answer (catalogModelCost).
//     It is the opencode-free authority: an anthropic/claude record is priced —
//     cache-read at the cache-read rate — on a plane with no opencode binary,
//     no network and no discoverer at all (Gap 4). This is the SAME shared
//     lookup the model picker consumes; there is no second catalog lookup.
//
// modelDiscoverer may be nil — "model discovery disabled", i.e. the opencode
// binary is absent. ListModels has no nil-receiver guard, so the nil check MUST
// come before any use of it; the catalog branch above is what turns a nil
// discoverer from a nil-panic into a supported configuration.
//
// Every branch fails closed (nil, false): no pricing beats a guess, and the
// recorder then keeps the adapter-reported cost verbatim.
func pricingResolver(modelDiscoverer *aigateway.ModelDiscoverer) aigateway.PricingResolver {
	return func(ctx context.Context, provider, model string) (*apiv1.ModelCost, bool) {
		if modelDiscoverer != nil {
			if models, err := modelDiscoverer.ListModels(ctx, provider); err == nil {
				ref := provider + "/" + model
				for _, m := range models {
					if (strings.EqualFold(m.ProviderId, provider) && strings.EqualFold(m.Id, model)) ||
						strings.EqualFold(m.ModelRef, ref) {
						return m.Cost, m.Cost != nil
					}
				}
			}
		}
		return catalogModelCost(provider, model)
	}
}
