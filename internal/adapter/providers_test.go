package adapter

import (
	"reflect"
	"testing"
)

func TestBuiltinProviderCatalog(t *testing.T) {
	c := NewBuiltinProviderCatalog()
	if !c.IsKnownAdapter(DefaultAdapterKind) {
		t.Errorf("IsKnownAdapter(opencode) = false, want true")
	}
	if !c.IsKnownAdapter("claude") || !c.IsKnownAdapter("orchicon") {
		t.Errorf("built-in adapter kinds claude/orchicon not registered")
	}
	if c.IsKnownAdapter("mystery") {
		t.Errorf("IsKnownAdapter(mystery) = true, want false")
	}
	if !c.IsKnownProvider("opencode", "anthropic") {
		t.Errorf("IsKnownProvider(opencode, anthropic) = false, want true (built-in)")
	}
	if c.IsKnownProvider("opencode", "local-models") {
		t.Errorf("IsKnownProvider(opencode, local-models) = true before AddAdapterKind, want false")
	}
	c.AddAdapterKind(DefaultAdapterKind, "local-models")
	if !c.IsKnownProvider("opencode", "local-models") {
		t.Errorf("IsKnownProvider(opencode, local-models) = false after AddAdapterKind, want true (tenant custom)")
	}
	if !c.IsKnownProvider("claude", "anthropic") {
		t.Errorf("IsKnownProvider(claude, anthropic) = false, want true")
	}
}

func TestProvidersSorted(t *testing.T) {
	c := NewBuiltinProviderCatalog()
	got := c.Providers("opencode")
	want := []string{"anthropic", "local", "openai", "opencode", "opencode-go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Providers(opencode) = %v, want %v (sorted)", got, want)
	}
}
