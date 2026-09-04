package orchicon

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Registry (ADR-0003 D10) resolves provider profiles into concrete clients:
// profile (built-in or tenant custom) → credential resolution → client
// construction. Per-tenant instances are cached and invalidated on settings
// change. No RPC/proto here — Go APIs consumed by the settings-UI task, the
// native-adapter task, and the pickers.
type Registry struct {
	creds *CredentialResolver
	src   *SourcingService
	httpc *http.Client

	// builtinOverrides loads the tenant's provider-settings overrides for a
	// BUILT-IN provider id (the settings-UI service registers it). Without
	// it a built-in id always resolves to the built-in default profile and
	// tenant overrides (base URL, num_ctx, hidden models) are silently
	// ignored at chat time (observed: an ollama CLOUD override in Settings
	// → Adapters → Providers while chat kept dialing localhost:11434).
	builtinOverrides func(ctx context.Context, tenantID, providerID string) (Profile, bool, error)

	mu      sync.Mutex
	cache   map[string]Provider // tenantID|providerID → live client
	warnLog func(string, ...any)
}

// NewRegistry wires the registry. log may be nil (warnings dropped).
func NewRegistry(creds *CredentialResolver, src *SourcingService, httpc *http.Client, warn func(string, ...any)) *Registry {
	if httpc == nil {
		httpc = defaultHTTPClient() // per-provider connect/total timeouts (D11)
	}
	return &Registry{creds: creds, src: src, httpc: httpc, cache: map[string]Provider{}, warnLog: warn}
}

// SetBuiltinOverridesLoader installs the tenant built-in provider override
// loader (the providers settings service). Once registered, Get resolves a
// BUILT-IN provider as built-in default ⊕ stored tenant overrides via the
// service's EffectiveProfile (same mapping the RPC views use) — falling
// back to the pure built-in table when no row is stored. Before this hook
// existed, every built-in chat client used the built-in defaults regardless
// of operator overrides.
func (r *Registry) SetBuiltinOverridesLoader(fn func(ctx context.Context, tenantID, providerID string) (Profile, bool, error)) {
	r.mu.Lock()
	r.builtinOverrides = fn
	r.mu.Unlock()
}

// Invalidate drops the cached provider instance for one (tenant, provider)
// pair — the settings-UI task calls it on provider/secret/profile change.
func (r *Registry) Invalidate(tenantID, providerID string) {
	r.mu.Lock()
	delete(r.cache, tenantID+"|"+providerID)
	r.mu.Unlock()
	if r.src != nil {
		r.src.InvalidateProbe(providerID)
	}
	if r.creds != nil {
		r.creds.Invalidate()
	}
}

// Get resolves one provider for a tenant. Built-in ids resolve through the
// built-in profile table; custom ids load through the custom-profile loader
// (registered by the settings-UI task via SetCustomProfileLoader).
func (r *Registry) Get(ctx context.Context, tenantID, providerID string) (Provider, error) {
	key := tenantID + "|" + providerID
	r.mu.Lock()
	if p, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return p, nil
	}
	r.mu.Unlock()

	p, err := r.profile(ctx, tenantID, providerID)
	if err != nil {
		return nil, err
	}
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}

	prov, err := r.build(ctx, tenantID, p)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[key] = prov
	r.mu.Unlock()
	return prov, nil
}

// profile resolves the Profile row for a provider id: built-in ids resolve
// through the built-in table ⊕ the tenant override loader (when the
// settings service has registered one), custom ids load through the
// custom-profile loader (registered by the settings-UI task via
// SetCustomProfileLoader).
func (r *Registry) profile(ctx context.Context, tenantID, providerID string) (Profile, error) {
	if _, builtin := BuiltinProfile(providerID); builtin {
		r.mu.Lock()
		loader := r.builtinOverrides
		r.mu.Unlock()
		if loader != nil {
			// Resolve the tenant-effective profile for the built-in id. The
			// loader returns (profile, found, err): found=false covers both
			// "no stored row" (pure built-in default) and "provider
			// disabled/deleted" (also fall back to the built-in default —
			// dispatch gates decide enabled-ness upstream, not the registry;
			// disabling mid-flight must not break in-flight sessions that
			// already resolved a client).
			if p, found, err := loader(ctx, tenantID, providerID); err == nil && found {
				return p, nil
			} else if err != nil && r.warnLog != nil {
				r.warnLog("registry: built-in overrides lookup failed for %q — using built-in defaults", providerID)
			}
		}
		if base, ok := BuiltinProfile(providerID); ok {
			return base, nil
		}
		return Profile{}, fmt.Errorf("registry: unknown built-in provider %q", providerID)
	}
	customs, err := loadCustomProfiles(ctx, tenantID)
	if err != nil {
		return Profile{}, fmt.Errorf("registry: load custom profiles: %w", err)
	}
	for _, p := range customs {
		if p.ID == providerID {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("registry: unknown provider %q", providerID)
}

// build constructs the concrete client for a resolved profile.
func (r *Registry) build(ctx context.Context, tenantID string, p Profile) (Provider, error) {
	modelsFn := func(ctx context.Context) ([]ModelInfo, error) {
		// The runtime probe carries the resolved credential (secret →
		// env) — the sourcing probe must authenticate exactly like the
		// chat calls (live truth only; an unauthenticated probe would
		// 401 and serve an empty list). Auth-optional profiles resolve
		// to "" (Ollama, local servers).
		bearer, _ := r.creds.Resolve(ctx, tenantID, p) //nolint:errcheck // probe stays non-fatal without a credential
		res := r.src.ListModels(ctx, p, bearer)
		if res.Degraded {
			// Runtime model metadata stays NON-FATAL: chat never needs the
			// models list, and compaction "must not guess" already handles
			// missing hints — failing a worker turn because /models
			// hiccuped would be worse than an empty list. The honest
			// "works or it doesn't" signal lives at the UI surface
			// (settings eyeball + picker: empty list + degraded state).
			// NO synthesized fallback is served either way (operator
			// directive). warnLog is nil-safe: registries built without a
			// logger (tests) must not panic.
			if r.warnLog != nil {
				r.warnLog("sourcing: probe failed for provider %q — no models available to runtime clients (check endpoint/token)", p.ID)
			}
		}
		return res.Models, nil
	}
	warn := r.warnLog
	switch p.Kind {
	case ProfileKindAnthropic:
		key, err := r.creds.Resolve(ctx, tenantID, p)
		if err != nil {
			return nil, err
		}
		return &AnthropicClient{
			BaseURL: p.BaseURL, APIKey: key, HTTP: r.httpc,
			ModelsFn: modelsFn, CacheTTL: anthropicCacheTTLOptIn(),
		}, nil

	case ProfileKindOpenAICompat, ProfileKindCustom:
		key, err := r.creds.Resolve(ctx, tenantID, p)
		if err != nil {
			return nil, err
		}
		auth := "bearer"
		if key == "" {
			auth = "none"
		}
		return &OpenAICompatClient{
			BaseURL: p.BaseURL, APIKey: key, Quirks: p.Quirks, AuthStyle: auth,
			HTTP: r.httpc, ProviderID: p.ID, ModelsFn: modelsFn,
		}, nil

	case ProfileKindOllama:
		return &OllamaClient{
			Host: p.BaseURL, HTTP: r.httpc, ModelsFn: modelsFn, Warnf: warn,
			NumCtxDefault: p.NumCtxDefault,
		}, nil

	case ProfileKindCommandCode:
		key, err := r.creds.Resolve(ctx, tenantID, p)
		if err != nil {
			return nil, err
		}
		return &CommandCodeClient{
			BaseURL: p.BaseURL, APIKey: key, HTTP: r.httpc, ModelsFn: modelsFn,
		}, nil

	default:
		return nil, fmt.Errorf("registry: profile %q has unsupported kind %q", p.ID, p.Kind)
	}
}

// defaultHTTPClient applies the per-provider connect/total timeouts (D11).
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute, // streams are long-lived; per-attempt cap
	}
}
