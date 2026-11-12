package aigateway

import (
	"context"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// CLIProviderRegistry is the validation registry for legacy 2-segment model refs (e.g.
// opencode/deepseek/deepseek-v4-flash).
type CLIProviderRegistry struct {
	static adapter.ProviderRegistry
	disc   *ModelDiscoverer

	// extra is the additive layer applied on CLONES only (Clone + the merge seam): kind → additional
	// provider ids unioned over the static registry.
	extra map[string]map[string]struct{}
}

// NewCLIProviderRegistry wraps a static registry with live CLI provider discovery.
func NewCLIProviderRegistry(static adapter.ProviderRegistry, disc *ModelDiscoverer) *CLIProviderRegistry {
	if static == nil {
		static = adapter.NewBuiltinProviderCatalog()
	}
	return &CLIProviderRegistry{
		static: static,
		disc:   disc,
	}
}

// providerIDs returns the distinct providerID values from discovery.
func (r *CLIProviderRegistry) providerIDs(ctx context.Context) []string {
	if r.disc == nil {
		return nil
	}
	models, err := r.disc.ListModels(ctx, "")
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, m := range models {
		id := m.GetProviderId()
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// ensure adapter.ProviderRegistry is satisfied.
var _ adapter.ProviderRegistry = (*CLIProviderRegistry)(nil)
var _ adapter.ProviderKindExtender = (*CLIProviderRegistry)(nil)

// Clone implements adapter.ProviderKindExtender: an independent copy that shares the static
// registry and discoverer (read-only) and starts with an empty additive layer.
func (r *CLIProviderRegistry) Clone() adapter.ProviderKindExtender {
	return &CLIProviderRegistry{static: r.static, disc: r.disc}
}

// AddAdapterKind implements adapter.ProviderKindExtender: unions provider ids into the clone's
// additive layer (a shared instance is never mutated.
func (r *CLIProviderRegistry) AddAdapterKind(kind string, providers ...string) {
	if r.extra == nil {
		r.extra = make(map[string]map[string]struct{})
	}
	set := r.extra[kind]
	if set == nil {
		set = make(map[string]struct{})
		r.extra[kind] = set
	}
	for _, p := range providers {
		if p = strings.TrimSpace(p); p != "" {
			set[p] = struct{}{}
		}
	}
}

// extraIDs returns the additive provider ids for a kind (nil when none).
func (r *CLIProviderRegistry) extraIDs(kind string) []string {
	set := r.extra[kind]
	if set == nil {
		return nil
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// IsKnownAdapter implements ProviderRegistry.
func (r *CLIProviderRegistry) IsKnownAdapter(kind string) bool {
	return r.static.IsKnownAdapter(kind)
}

// IsKnownProvider implements ProviderRegistry: the static catalog first, then the live CLI ids
// under the default kind, then the additive layer (clones carrying tenant customs).
func (r *CLIProviderRegistry) IsKnownProvider(adapterKind, provider string) bool {
	if r.static.IsKnownProvider(adapterKind, provider) {
		return true
	}
	if set := r.extra[adapterKind]; set != nil {
		if _, ok := set[provider]; ok {
			return true
		}
	}
	// CLI ids validate under the DEFAULT kind only (see Providers): under any other kind they are not a
	// provider that kind can resolve, so accepting them would validate a ref that fails at dispatch.
	if !hasCLINamespace(adapterKind) {
		return false
	}
	for _, id := range r.providerIDs(context.Background()) {
		if id == provider {
			return true
		}
	}
	return false
}

// cliNamespaceKind resolves the adapter kind whose provider namespace the live CLI ids belong to.
func cliNamespaceKind(kind string) string {
	if k := strings.TrimSpace(kind); k != "" {
		return k
	}
	return adapter.DefaultAdapterKind
}

// hasCLINamespace reports whether CLI-discovered provider ids may extend this kind's provider set.
// ONLY the default (opencode) kind, because those ids ARE the opencode CLI's namespace.
func hasCLINamespace(adapterKind string) bool {
	return cliNamespaceKind(adapterKind) == adapter.DefaultAdapterKind
}

// Providers implements ProviderRegistry: the static set, the kind's additive layer (tenant
// customs), then — for the DEFAULT kind ONLY — the live CLI ids (deduped, sorted).
func (r *CLIProviderRegistry) Providers(adapterKind string) []string {
	out := r.static.Providers(adapterKind)
	seen := map[string]struct{}{}
	for _, p := range out {
		seen[p] = struct{}{}
	}
	var extra []string
	add := func(p string) {
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			extra = append(extra, p)
		}
	}
	for _, p := range r.extraIDs(adapterKind) {
		add(p)
	}
	if hasCLINamespace(adapterKind) {
		for _, id := range r.providerIDs(context.Background()) {
			add(id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}
