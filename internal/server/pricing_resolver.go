package server

import (
	"context"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/aigateway"
)

// pricingResolver builds the cost authority the AI gateway wires into the usage
// recorder (Run calls SetPricingResolver(pricingResolver(modelDiscoverer))).
//
// The order is load-bearing:
//
//  1. The VENDORED CATALOG answers first (aigateway.CatalogCost). It is the
//     opencode-free authority: an anthropic/claude record is priced —
//     cache-read at the cache-read rate — on a plane with no opencode binary,
//     no network and no discoverer at all.
//  2. Only then does the opencode DISCOVERER get a turn: with one wired, a
//     model the catalog does not carry (an opencode-namespace model) is still
//     priced from the live CLI listing exactly as before. Nothing regresses.
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
		if c, ok := aigateway.CatalogCost(provider, model); ok {
			return c, true
		}
		if modelDiscoverer == nil {
			return nil, false
		}
		models, err := modelDiscoverer.ListModels(ctx, provider)
		if err != nil {
			return nil, false
		}
		ref := provider + "/" + model
		for _, m := range models {
			if (strings.EqualFold(m.ProviderId, provider) && strings.EqualFold(m.Id, model)) ||
				strings.EqualFold(m.ModelRef, ref) {
				return m.Cost, m.Cost != nil
			}
		}
		return nil, false
	}
}
