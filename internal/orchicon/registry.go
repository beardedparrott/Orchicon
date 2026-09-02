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

// profile resolves the Profile row for a provider id (built-in first,
// then the tenant-custom loader).
func (r *Registry) profile(ctx context.Context, tenantID, providerID string) (Profile, error) {
	if p, ok := BuiltinProfile(providerID); ok {
		return p, nil
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
			// directive).
			r.warnLog("sourcing: probe failed for provider %q — no models available to runtime clients (check endpoint/token)", p.ID)
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
			ModelsFn: modelsFn,
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
