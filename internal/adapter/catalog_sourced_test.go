package adapter

import "testing"

// The model-tier classification the server publishes to BOTH pickers:
// catalog-sourced kinds resolve their models from the providers sourcing view
// (the vendored catalog), so they must list claude and orchicon and must NOT
// list opencode (whose models are the CLI's discovery, not the catalog's).
func TestCatalogSourcedAdapterKinds(t *testing.T) {
	got := CatalogSourcedAdapterKinds()
	set := make(map[string]bool, len(got))
	for _, k := range got {
		set[k] = true
	}
	if !set[KindOrchicon] {
		t.Errorf("CatalogSourcedAdapterKinds() = %v, want %q", got, KindOrchicon)
	}
	if !set[KindClaude] {
		t.Errorf("CatalogSourcedAdapterKinds() = %v, want %q (its models are the anthropic catalog's, not opencode's)", got, KindClaude)
	}
	if set[KindOpencode] {
		t.Errorf("CatalogSourcedAdapterKinds() = %v, must NOT include %q (its models come from CLI discovery)", got, KindOpencode)
	}
}

// The provider tier of the claude kind is already correct and must stay that
// way: claude → ["anthropic"], from the registry (ADR-0003 D3).
func TestClaudeAdapterScopesToAnthropic(t *testing.T) {
	c := NewBuiltinProviderCatalog()
	if !c.IsKnownAdapter(KindClaude) {
		t.Fatalf("the built-in catalog does not know adapter kind %q", KindClaude)
	}
	got := c.Providers(KindClaude)
	if len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("Providers(%q) = %v, want [\"anthropic\"]", KindClaude, got)
	}
}
