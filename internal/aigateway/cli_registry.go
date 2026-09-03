package aigateway

import (
	"context"
	"sort"

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

// Clone implements adapter.ProviderKindExtender: an independent copy with
// the same static registry and discoverer (the discoverer is shared
// state by design — the clone's AddAdapterKind never mutates the
// original's static registry).
func (r *CLIProviderRegistry) Clone() adapter.ProviderRegistry {
	return &CLIProviderRegistry{static: r.static, disc: r.disc}
}

// IsKnownAdapter implements ProviderRegistry. The static catalog decides;
// CLI ids extend the DEFAULT kind's namespace, which the static catalog
// already knows (the opencode kind) — so no extra adapter kinds exist.
func (r *CLIProviderRegistry) IsKnownAdapter(kind string) bool {
	return r.static.IsKnownAdapter(kind)
}

// IsKnownProvider implements ProviderRegistry: the static catalog first,
// then the live CLI ids under the default kind.
func (r *CLIProviderRegistry) IsKnownProvider(adapterKind, provider string) bool {
	if r.static.IsKnownProvider(adapterKind, provider) {
		return true
	}
	for _, id := range r.providerIDs(context.Background()) {
		if id == provider {
			return true
		}
	}
	return false
}

// Providers implements ProviderRegistry: the static set first, CLI ids
// appended (deduped, sorted) — mirroring the picker's union derivation.
func (r *CLIProviderRegistry) Providers(adapterKind string) []string {
	out := r.static.Providers(adapterKind)
	seen := map[string]struct{}{}
	for _, p := range out {
		seen[p] = struct{}{}
	}
	var extra []string
	for _, id := range r.providerIDs(context.Background()) {
		if _, dup := seen[id]; !dup {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}