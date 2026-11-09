package server

import (
	"context"
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/aigateway"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// catalogModelCost converts the OFFLINE vendored catalog's pricing for a provider/bare-model-id
// into the gateway's ModelCost shape.
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

// ListModels has no nil-receiver guard, so the nil check MUST come before any use of it.
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
