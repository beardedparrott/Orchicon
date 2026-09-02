package aigateway

import (
	"context"
	"sort"
	"sync"
	"time"

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
// The CLI result is TTL-cached (the discoverer shells out per refresh);
// a failed or empty discovery degrades to the static catalog — validation
// never breaks saves. Thread-safe; refreshes are serialized by mutex.
type CLIProviderRegistry struct {
	static adapter.ProviderRegistry

	mu       sync.Mutex
	disc     *ModelDiscoverer
	cliIDs   []string
	cachedAt time.Time
	ttl      time.Duration
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
		ttl:    10 * time.Minute,
	}
}

// refresh pulls the distinct providerID values from CLI discovery when
// the cache is stale. A failed discovery keeps the previous ids and marks
// the cache fresh anyway (retry after the next TTL) — it never errors to
// callers.
func (r *CLIProviderRegistry) refresh(ctx context.Context) {
	if r.disc == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.cachedAt) < r.ttl {
		return
	}
	models, err := r.disc.ListModels(ctx, "")
	if err != nil {
		r.cachedAt = time.Now()
		return
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
	r.cliIDs = ids
	r.cachedAt = time.Now()
}

// cliIDsFor returns the cached CLI provider ids (refreshing on demand when
// stale). Only the default adapter kind (and its empty-string alias) is
// backed by CLI discovery; every other kind gets none.
func (r *CLIProviderRegistry) cliIDsFor(ctx context.Context, adapterKind string) []string {
	if adapterKind != "" && adapterKind != adapter.DefaultAdapterKind {
		return nil
	}
	r.refresh(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cliIDs
}

// ensure adapter.ProviderRegistry is satisfied.
var _ adapter.ProviderRegistry = (*CLIProviderRegistry)(nil)

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
	for _, id := range r.cliIDsFor(context.Background(), adapterKind) {
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
	for _, id := range r.cliIDsFor(context.Background(), adapterKind) {
		if _, dup := seen[id]; !dup {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}