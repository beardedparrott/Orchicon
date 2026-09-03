package adapter

import (
	"sort"
	"strings"
)

// ProviderRegistry resolves the known providers per adapter kind. It is
// the data source for BOTH legacy 2-segment inference (ParseModelRef) and
// the adapter-scoped provider tier of the model picker (ADR-0003 D3).
//
// The per-adapter provider list = the adapter's built-in provider
// profiles ∪ the tenant-created custom providers bound to that adapter
// (Settings → Adapters). The provider-layer task owns the tenant custom
// provider table; this interface is the contract it satisfies. Until that
// lands, implementations resolve built-in profiles only (custom providers
// are inert — a "local-models/..." ref parses once the provider exists
// and is rejected with a Settings → Adapters pointer before that).
type ProviderRegistry interface {
	// IsKnownAdapter reports whether kind is a recognized adapter kind.
	IsKnownAdapter(kind string) bool
	// IsKnownProvider reports whether provider is a known provider of the
	// given adapter kind (built-in profile OR tenant custom provider).
	IsKnownProvider(adapterKind, provider string) bool
	// Providers returns the known providers of an adapter kind (built-in ∪
	// tenant custom), sorted. The default adapter kind is assumed when
	// adapterKind is empty.
	Providers(adapterKind string) []string
}

// BuiltinProviderCatalog is the static built-in half of the registry: the
// adapter kinds and provider profiles the platform ships (ADR-0003 D3).
// The server seeds it from the model discoverer's provider ids ∪ the
// default provider profiles (aigateway), then registers it as the
// validation registry. The tenant custom-provider source (provider-layer
// task) will merge in via the same ProviderRegistry interface.
type BuiltinProviderCatalog struct {
	adapterKinds map[string]struct{}
	providers    map[string]map[string]struct{}
}

// NewBuiltinProviderCatalog returns a catalog pre-seeded with the
// platform's built-in adapter kinds and their built-in provider profiles
// (the ADR-0003 pinned examples). The opencode kind is seeded with the
// v0.1 provider profiles; the server extends it with the live model
// discoverer's provider ids via AddAdapterKind (union, dedup).
//
// The orchicon kind (native bridge, ADR-0004) serves EVERY built-in
// provider profile (internal/orchicon profile table) plus the operator's
// local models — the picker's orchicon provider tier mirrors this (all
// enabled entries from Settings → Adapters). Provider ids are the
// canonical profile ids (commandcode, not "command-code").
func NewBuiltinProviderCatalog() *BuiltinProviderCatalog {
	c := &BuiltinProviderCatalog{
		adapterKinds: make(map[string]struct{}),
		providers:    make(map[string]map[string]struct{}),
	}
	c.AddAdapterKind(DefaultAdapterKind,
		"anthropic", "openai", "local", "opencode", "opencode-go")
	c.AddAdapterKind("claude", "anthropic")
	c.AddAdapterKind("orchicon",
		"anthropic", "openai", "openrouter", "opencode", "opencode-go",
		"commandcode", "ollama", "local-models")
	return c
}

// BuiltinAdapterKinds returns the built-in catalog's adapter kinds as a
// set — the fallback cut for ref canonicalization when the dispatcher's
// live kinds are unavailable (migrations run before the server registers
// anything live).
func BuiltinAdapterKinds() map[string]struct{} {
	c := NewBuiltinProviderCatalog()
	out := make(map[string]struct{}, len(c.adapterKinds))
	for k := range c.adapterKinds {
		out[k] = struct{}{}
	}
	return out
}

// ProviderKindExtender is the optional seam for registries that can be
// extended with provider ids per adapter kind without mutating the
// shared instance. The worker service's tenant-custom merge uses it so
// composition works with ANY installed registry (built-in catalog,
// CLI-aware registry, or a custom implementation that declines the seam).
type ProviderKindExtender interface {
	// Clone returns an independent copy that AddAdapterKind may mutate.
	Clone() ProviderRegistry
}

// AddAdapterKind registers an adapter kind and (re)adds its built-in
// provider profiles. Adding an existing provider id is a no-op (union).
func (c *BuiltinProviderCatalog) AddAdapterKind(kind string, providers ...string) {
	c.adapterKinds[kind] = struct{}{}
	set := c.providers[kind]
	if set == nil {
		set = make(map[string]struct{})
		c.providers[kind] = set
	}
	for _, p := range providers {
		if p = strings.TrimSpace(p); p != "" {
			set[p] = struct{}{}
		}
	}
}

// AdapterKinds returns the registered adapter kinds, sorted. It satisfies
// the optional AdapterKindLister the worker service's explicit-adapter-input
// fallback uses (ADR-0005 D2) when the Dispatcher kinds are not wired.
func (c *BuiltinProviderCatalog) AdapterKinds() []string {
	out := make([]string, 0, len(c.adapterKinds))
	for k := range c.adapterKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsKnownAdapter implements ProviderRegistry.
func (c *BuiltinProviderCatalog) IsKnownAdapter(kind string) bool {
	_, ok := c.adapterKinds[kind]
	return ok
}

// IsKnownProvider implements ProviderRegistry.
func (c *BuiltinProviderCatalog) IsKnownProvider(adapterKind, provider string) bool {
	set := c.providers[adapterKind]
	if set == nil {
		return false
	}
	_, ok := set[provider]
	return ok
}

// Clone implements ProviderKindExtender: an independent deep copy that
// AddAdapterKind may mutate (the tenant-custom merge is copy-on-write).
func (c *BuiltinProviderCatalog) Clone() ProviderRegistry {
	out := &BuiltinProviderCatalog{
		adapterKinds: make(map[string]struct{}, len(c.adapterKinds)),
		providers:    make(map[string]map[string]struct{}, len(c.providers)),
	}
	for k := range c.adapterKinds {
		out.adapterKinds[k] = struct{}{}
	}
	for kind, set := range c.providers {
		m := make(map[string]struct{}, len(set))
		for p := range set {
			m[p] = struct{}{}
		}
		out.providers[kind] = m
	}
	return out
}

// Providers implements ProviderRegistry (sorted, stable).
func (c *BuiltinProviderCatalog) Providers(adapterKind string) []string {
	if adapterKind == "" {
		adapterKind = DefaultAdapterKind
	}
	set := c.providers[adapterKind]
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
