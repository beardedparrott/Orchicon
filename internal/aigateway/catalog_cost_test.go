package aigateway

import (
	"testing"
)

// The catalog lookup is the opencode-free pricing authority: no discoverer, no
// CLI, no network. It must answer for an anthropic model with its authored
// cache-aware rates.
func TestCatalogCostAnswersForAnthropic(t *testing.T) {
	cost, ok := CatalogCost("anthropic", "claude-sonnet-4")
	if !ok || cost == nil {
		t.Fatalf("CatalogCost(anthropic, claude-sonnet-4) = (%v, %v), want pricing", cost, ok)
	}
	if cost.Input <= 0 || cost.Output <= 0 {
		t.Fatalf("input/output rates = %v/%v, want the authored rates", cost.Input, cost.Output)
	}
	// Cache-awareness is the point of Gap 4: the cache-read rate is a REAL,
	// DISTINCT rate, not a copy of the input rate.
	if cost.CacheRead <= 0 {
		t.Fatalf("cache_read = %v, want the cache-read rate (> 0)", cost.CacheRead)
	}
	if cost.CacheRead >= cost.Input {
		t.Fatalf("cache_read = %v >= input %v, want a distinct cache-read rate", cost.CacheRead, cost.Input)
	}
	if cost.CacheWrite <= 0 {
		t.Fatalf("cache_write = %v, want the cache-write rate (> 0)", cost.CacheWrite)
	}
}

// Alias resolution and ref-shaped providers go through the SAME lookup: a
// 3-segment ref's provider segment is "adapter/provider", which the catalog
// never keys on, so the provider segment is unwrapped first.
func TestCatalogCostResolvesAliasesAndRefShapedProviders(t *testing.T) {
	direct, ok := CatalogCost("anthropic", "claude-sonnet-4")
	if !ok {
		t.Fatal("direct lookup failed")
	}
	for _, c := range []struct{ provider, model string }{
		{"anthropic", "sonnet-4"},               // catalog alias
		{"claude/anthropic", "claude-sonnet-4"}, // ref-shaped provider
	} {
		got, ok := CatalogCost(c.provider, c.model)
		if !ok || got == nil {
			t.Fatalf("CatalogCost(%q, %q) = (%v, %v), want a hit", c.provider, c.model, got, ok)
		}
		if got.Input != direct.Input || got.Output != direct.Output ||
			got.CacheRead != direct.CacheRead || got.CacheWrite != direct.CacheWrite {
			t.Fatalf("CatalogCost(%q, %q) = %+v, want the same pricing as the direct hit %+v", c.provider, c.model, got, direct)
		}
	}
}

// Fail closed — a miss returns (nil, false) so the caller keeps the
// adapter-reported cost. Never a nil-panic, never a zero-cost guess.
func TestCatalogCostFailsClosed(t *testing.T) {
	for _, c := range []struct{ provider, model string }{
		{"anthropic", "no-such-model"},
		{"no-such-provider", "claude-sonnet-4"},
		{"", "claude-sonnet-4"},
		{"anthropic", ""},
		{"", ""},
	} {
		cost, ok := CatalogCost(c.provider, c.model)
		if ok || cost != nil {
			t.Fatalf("CatalogCost(%q, %q) = (%v, %v), want (nil, false)", c.provider, c.model, cost, ok)
		}
	}
}
