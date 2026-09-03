package aigateway

import (
	"context"
	"sort"
	"strings"

	"github.com/beardedparrott/orchicon/internal/adapter"
)

// CLIProviderRegistry is the validation registry for legacy 2-segment
// model refs (e.g. opencode/deepseek/deepseek-v4-flash): the static
// builtin catalog UNION the live CLI's distinct providerID values. The
// picker auto-pulls its provider pills from CLI discovery; validation
// must agree with the picker, or every freshly-selected CLI ref fails
// validation ("provider not found") at save time.
//
// CLI-discovered ids validate under the DEFAULT adapter kind only — the
// legacy 2-segment grammar infers the default kind, which is the CLI's
// namespace (the opencode adapter). The static catalog governs every
// other kind untouched.
//
// Deriving the provider ids is in-memory: the discoverer's model list is
// SWR-cached after its first cold load (the ~1.2s subprocess probe never
// runs on the validation hot path), so this registry derives per-call
// from the discoverer rather than keeping its own TTL cache (a cache of
// a cache only adds staleness). A failed or empty discovery degrades to
// the static catalog — validation never breaks saves. Thread-safe (the
// discoverer owns its own locking).
type CLIProviderRegistry struct {
	static adapter.ProviderRegistry
	disc   *ModelDiscoverer

	// extra is the additive layer applied on CLONES only (Clone + the
	// merge seam): kind → additional provider ids unioned over the static
	// registry. The shared instance never carries state — tenant-custom
	// merges are copy-on-write.
	extra map[string]map[string]struct{}
}

// NewCLIProviderRegistry wraps a static registry with live CLI provider
// discovery. disc may be nil (no CLI binary) — the registry then behaves
// exactly like the static catalog.
func NewCLIProviderRegistry(static adapter.ProviderRegistry, disc *ModelDiscoverer) *CLIProviderRegistry {
	if static == nil {
		static = adapter.NewBuiltinProviderCatalog()
	}
	return &CLIProviderRegistry{
		static: static,
		disc:   disc,
	}
}

// providerIDs returns the distinct providerID values from discovery. A
// discovery error (CLI missing, cold load failed, backoff window) yields
// nil — callers degrade to the static catalog.
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

// Clone implements adapter.ProviderKindExtender: an independent copy that
// shares the static registry and discoverer (read-only) and starts with an
// empty additive layer — AddAdapterKind on the clone never mutates the
// original.
func (r *CLIProviderRegistry) Clone() adapter.ProviderKindExtender {
	return &CLIProviderRegistry{static: r.static, disc: r.disc}
}

// AddAdapterKind implements adapter.ProviderKindExtender: unions provider
// ids into the clone's additive layer (a shared instance is never
// mutated; copy-on-write is the caller's contract via Clone).
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

// IsKnownAdapter implements ProviderRegistry. The static catalog decides;
// CLI ids extend the DEFAULT kind's namespace, which the static catalog
// already knows (the opencode kind) — so no extra adapter kinds exist.
func (r *CLIProviderRegistry) IsKnownAdapter(kind string) bool {
	return r.static.IsKnownAdapter(kind)
}

// IsKnownProvider implements ProviderRegistry: the static catalog first,
// then the live CLI ids under the default kind, then the additive layer
// (clones carrying tenant customs).
func (r *CLIProviderRegistry) IsKnownProvider(adapterKind, provider string) bool {
	if r.static.IsKnownProvider(adapterKind, provider) {
		return true
	}
	if set := r.extra[adapterKind]; set != nil {
		if _, ok := set[provider]; ok {
			return true
		}
	}
	for _, id := range r.providerIDs(context.Background()) {
		if id == provider {
			return true
		}
	}
	return false
}

// Providers implements ProviderRegistry: the static set, the additive
// layer, then CLI ids appended (deduped, sorted) — mirroring the picker's
// union derivation.
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
	for _, id := range r.providerIDs(context.Background()) {
		add(id)
	}
	sort.Strings(extra)
	return append(out, extra...)
}
