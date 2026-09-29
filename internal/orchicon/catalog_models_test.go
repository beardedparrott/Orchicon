package orchicon

import "testing"

// The offline model source the pickers fall back to: the vendored catalog's
// authored anthropic entries. This test is the guard that a plane with NO
// opencode binary and NO network can still list claude models with the context
// window and cache-aware pricing the picker/compaction math needs.
func TestCatalogModelsForProviderServesAnthropicOffline(t *testing.T) {
	models := CatalogModelsForProvider("anthropic")
	if len(models) == 0 {
		t.Fatal("CatalogModelsForProvider(anthropic) is empty — the offline claude picker would render blank")
	}
	byID := make(map[string]ModelInfo, len(models))
	for _, m := range models {
		byID[m.ID] = m
		if !m.Visible {
			t.Errorf("model %q is not visible — catalogListByProvider must serve visible entries only", m.ID)
		}
		if m.Provenance != "catalog" {
			t.Errorf("model %q provenance = %q, want catalog", m.ID, m.Provenance)
		}
	}
	for _, want := range []string{"claude-sonnet-4", "claude-opus-4", "claude-haiku-4"} {
		if _, ok := byID[want]; !ok {
			t.Fatalf("anthropic/%s missing from the offline list: %+v", want, byID)
		}
	}
	son := byID["claude-sonnet-4"]
	if son.Context != 200000 {
		t.Errorf("claude-sonnet-4 context = %d, want 200000 (the picker's compaction hint)", son.Context)
	}
	if son.Pricing == nil {
		t.Fatal("claude-sonnet-4 has no pricing — Gap 4 would not be closed by this data")
	}
	if son.Pricing.CacheReadPerM <= 0 || son.Pricing.CacheReadPerM >= son.Pricing.InputPerM {
		t.Errorf("claude-sonnet-4 cache_read_per_m = %v (input %v), want a distinct cache-read rate", son.Pricing.CacheReadPerM, son.Pricing.InputPerM)
	}

	// A provider the catalog does not cover stays untouched — the caller keeps
	// its live/probe behaviour, never a synthesized list.
	if got := CatalogModelsForProvider("no-such-provider"); len(got) != 0 {
		t.Fatalf("CatalogModelsForProvider(no-such-provider) = %+v, want empty", got)
	}
}
