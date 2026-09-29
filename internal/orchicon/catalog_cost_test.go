package orchicon

import "testing"

// catalog_cost_test.go pins the SINGLE shared catalog-backed cost lookup
// (CatalogModelCost) that closes Gap 4: the server's pricing resolver must be
// able to answer for an anthropic model with NO opencode discoverer, from the
// offline vendored catalog. The model-picker sourcing child consumes this same
// function — do not add a second lookup.

// TestCatalogModelCostAnthropicSonnet4 pins the authored rates so a pricing
// regression is caught at the source.
func TestCatalogModelCostAnthropicSonnet4(t *testing.T) {
	p, ok := CatalogModelCost("anthropic", "claude-sonnet-4")
	if !ok || p == nil {
		t.Fatalf("CatalogModelCost(anthropic, claude-sonnet-4) = %v, %v; want the authored pricing", p, ok)
	}
	if p.InputPerM != 3.0 || p.OutputPerM != 15.0 || p.CacheReadPerM != 0.3 || p.CacheWritePerM != 3.75 {
		t.Fatalf("sonnet-4 pricing = %+v, want input=3 output=15 cache_read=0.3 cache_write=3.75", p)
	}
}

// TestCatalogModelCostAliasAware asserts the alias path resolves to the SAME
// entry, so a caller passing a bare alias ("sonnet-4") prices correctly.
func TestCatalogModelCostAliasAware(t *testing.T) {
	want, ok := CatalogModelCost("anthropic", "claude-sonnet-4")
	if !ok {
		t.Fatal("canonical claude-sonnet-4 missing from the catalog")
	}
	for _, alias := range []string{"sonnet-4", "claude-sonnet-4"} {
		got, ok := CatalogModelCost("anthropic", alias)
		if !ok || got == nil {
			t.Fatalf("CatalogModelCost(anthropic, %q) missed; want the aliased entry", alias)
		}
		if *got != *want {
			t.Fatalf("alias %q pricing = %+v, want %+v", alias, got, want)
		}
	}
}

// TestCatalogModelCostHaiku4 pins the cheapest tier the live smoke uses.
func TestCatalogModelCostHaiku4(t *testing.T) {
	p, ok := CatalogModelCost("anthropic", "claude-haiku-4")
	if !ok || p == nil {
		t.Fatalf("CatalogModelCost(anthropic, claude-haiku-4) = %v, %v", p, ok)
	}
	if p.CacheReadPerM <= 0 || p.CacheWritePerM <= 0 {
		t.Fatalf("haiku-4 must carry cache-aware pricing, got %+v", p)
	}
}

// TestCatalogModelCostFailsClosed is the AC's fail-CLOSED contract: a genuine
// catalog miss (unknown model, or a model under the wrong provider) returns
// (nil, false) so the caller keeps the adapter-reported cost — never a
// fabricated price.
func TestCatalogModelCostFailsClosed(t *testing.T) {
	cases := []struct{ provider, model string }{
		{"anthropic", "claude-nonexistent-9"},
		{"anthropic", ""},
		{"", "claude-sonnet-4"},
		{"opencode", "claude-sonnet-4"},
	}
	for _, c := range cases {
		if p, ok := CatalogModelCost(c.provider, c.model); ok || p != nil {
			t.Errorf("CatalogModelCost(%q, %q) = %v, %v; want (nil, false) fail-closed", c.provider, c.model, p, ok)
		}
	}
}
