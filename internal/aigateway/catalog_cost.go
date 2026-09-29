package aigateway

import (
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// CatalogCost resolves provider/model pricing from the vendored catalog via
// orchicon.GetModelForProvider — the ONE catalog-backed model/cost lookup the
// pickers and this pricing path share.
//
// It is the opencode-free pricing authority: it answers with no discoverer, no
// opencode binary and no network, which is exactly the plane the claude adapter
// targets. A ref-shaped provider ("adapter/provider", e.g. from a 3-segment
// model_ref) resolves by its PROVIDER segment — the catalog is keyed
// provider/id, never adapter/provider/id.
//
// It fails CLOSED: a model the catalog does not carry (or carries without
// pricing) returns (nil, false) so the caller keeps the adapter-reported cost.
// It never nil-panics on a missing receiver or a missing pricing block.
func CatalogCost(provider, model string) (*apiv1.ModelCost, bool) {
	if i := strings.LastIndex(provider, "/"); i >= 0 {
		provider = provider[i+1:]
	}
	if provider == "" || model == "" {
		return nil, false
	}
	mi, ok := orchicon.GetModelForProvider(provider, model)
	if !ok || mi.Pricing == nil {
		return nil, false
	}
	return &apiv1.ModelCost{
		Input:      mi.Pricing.InputPerM,
		Output:     mi.Pricing.OutputPerM,
		CacheRead:  mi.Pricing.CacheReadPerM,
		CacheWrite: mi.Pricing.CacheWritePerM,
	}, true
}
