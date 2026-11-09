package adapter

import (
	"sort"
	"strings"
)

// ProviderRegistry resolves the known providers per adapter kind.
type ProviderRegistry interface {
	// IsKnownAdapter reports whether kind is a recognized adapter kind.
	IsKnownAdapter(kind string) bool
	// IsKnownProvider reports whether provider is a known provider of the
	// given adapter kind (built-in profile OR tenant custom provider).
	IsKnownProvider(adapterKind, provider string) bool
	// Providers returns the known providers of an adapter kind (built-in ∪ tenant custom), sorted.
	Providers(adapterKind string) []string
}

// BuiltinProviderCatalog is the static built-in half of the registry: the adapter kinds and
// provider profiles the platform ships (ADR-0003 D3).
type BuiltinProviderCatalog struct {
	adapterKinds map[string]struct{}
	providers    map[string]map[string]struct{}
}

// NewBuiltinProviderCatalog returns a catalog pre-seeded with the platform's built-in adapter kinds
// and their built-in provider profiles (the ADR-0003 pinned examples).
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

// BuiltinAdapterKinds returns the built-in catalog's adapter kinds as a set — the fallback cut for
// ref canonicalization when the dispatcher's live kinds are unavailable (migrations run before.
func BuiltinAdapterKinds() map[string]struct{} {
	c := NewBuiltinProviderCatalog()
	out := make(map[string]struct{}, len(c.adapterKinds))
	for k := range c.adapterKinds {
		out[k] = struct{}{}
	}
	return out
}

// CatalogSourcedAdapterKinds returns the adapter kinds whose MODEL tier resolves from the
// providers-sourcing path (the vendored catalog ⊕ probe ⊕ manual view served.
func CatalogSourcedAdapterKinds() []string { return []string{KindOrchicon, KindClaude} }

// ProviderKindExtender is the optional seam for registries that can be extended with provider ids
// per adapter kind without mutating the shared instance.
type ProviderKindExtender interface {
	ProviderRegistry
	// Clone returns an independent copy; AddAdapterKind on the copy never
	// mutates the original's state.
	Clone() ProviderKindExtender
	// AddAdapterKind registers an adapter kind and unions provider ids
	// into it (existing ids are a no-op).
	AddAdapterKind(kind string, providers ...string)
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

// AdapterKinds returns the registered adapter kinds, sorted.
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
func (c *BuiltinProviderCatalog) Clone() ProviderKindExtender {
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
