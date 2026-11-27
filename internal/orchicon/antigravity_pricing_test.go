package orchicon_test

// antigravity_pricing_test.go — the `google/<agy-model-id>` pricing rows (G3, AC5's metered half).
//
// An Antigravity run on an API-key method IS priced per token, and the mapper records it with
// Provider:"google" and the BARE agy model id, so CatalogModelCost has to resolve exactly that pair.
// The rows are added ONLY for ids whose list price is established on Google's public Gemini API pricing
// page; every other id stays unpriced on purpose, because a fabricated number in front of an operator is
// worse than an honest "billing applies".

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/orchicon"
)

// TestGooglePricedModelsResolve pins the shape the recorder depends on: a bare agy id under provider
// `google` resolves to non-zero pricing.
func TestGooglePricedModelsResolve(t *testing.T) {
	priced := map[string]struct{ in, out float64 }{
		"gemini-4-carbon-high":    {2.00, 10.00},
		"gemini-4-carbon-low":     {2.00, 10.00},
		"gemini-4-argon-medium":   {2.00, 10.00},
		"gemini-4-neon-high":      {3.00, 15.00},
		"gemini-4-hydrogen-low":   {1.50, 7.50},
		"gemini-3.8-flash-medium": {0.75, 3.75},
	}
	for id, want := range priced {
		p, ok := orchicon.CatalogModelCost("google", id)
		if !ok || p == nil {
			t.Fatalf("CatalogModelCost(\"google\", %q) = not found — an API-key Antigravity turn on this "+
				"model would record cost 0", id)
		}
		if p.InputPerM != want.in || p.OutputPerM != want.out {
			t.Errorf("google/%s pricing = $%v in / $%v out, want $%v / $%v", id, p.InputPerM, p.OutputPerM, want.in, want.out)
		}
		if p.InputPerM <= 0 || p.OutputPerM <= 0 {
			t.Errorf("google/%s pricing is not positive: %+v", id, *p)
		}
		if !strings.Contains(p.Source, "ai.google.dev/gemini-api/docs/pricing") {
			t.Errorf("google/%s pricing source = %q, want it to cite the fetched pricing page", id, p.Source)
		}
		if !strings.Contains(p.Source, "2026-11-27") {
			t.Errorf("google/%s pricing source = %q, want the fetch date", id, p.Source)
		}
	}
}

// TestGoogleUnverifiableModelsStayUnpriced is the other half of the rule: an id with no public list
// price must NOT be priced. Pinning it keeps a future editor from "helpfully" inventing a number.
func TestGoogleUnverifiableModelsStayUnpriced(t *testing.T) {
	// The Orchestrator family has no per-token list price on the Gemini API page; agy's claude-* and
	// gpt-oss ids are not Gemini API models at all.
	for _, id := range []string{
		"gemini-pro-orchestrator", "gemini-pro-orchestrator-neon", "gemini-pro-orchestrator-argon",
		"claude-opus-5-5-high", "claude-sonnet-5-5-low", "gpt-oss-120b-medium",
	} {
		if p, ok := orchicon.CatalogModelCost("google", id); ok || p != nil {
			t.Errorf("google/%s resolved to pricing %+v — no public list price exists for it, so it must "+
				"stay unpriced (the cost explorer then shows the BillingDisclaimer)", id, p)
		}
	}
}

// TestGooglePricingRowsStayOutOfPickerListings pins `visible:false`: the `google` model tier is answered
// by the adapters/providers path (live agy handshake or the vendored snapshot). A visible catalog row
// would compete with it and show the operator a second, possibly staler, list.
func TestGooglePricingRowsStayOutOfPickerListings(t *testing.T) {
	if models := orchicon.CatalogModelsForProvider("google"); len(models) != 0 {
		t.Errorf("CatalogModelsForProvider(\"google\") returned %d rows; these rows exist for PRICING "+
			"only and must be visible:false so they do not leak into a picker listing", len(models))
	}
	// The pricing lookup itself must NOT filter on Visible, or the rows would be unreachable.
	if _, ok := orchicon.CatalogModelCost("google", "gemini-4-carbon-high"); !ok {
		t.Error("CatalogModelCost does not see the google rows — the pricing lookup must ignore Visible")
	}
}
