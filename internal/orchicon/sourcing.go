package orchicon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// SourcingService (D9): model sourcing per provider. LIVE TRUTH ONLY
// (operator directive: no synthesized model lists — a failing connection
// must show failure, never a list of models that "seemingly exist"):
//  1. GET {baseURL}/models probe (per-wire URL derivation; TTL-cached;
//     non-fatal — a failed probe yields NO models, visibly degraded),
//  2. manual model entries (operator-added, always served).
//
// The vendored catalog is metadata enrichment only: probed entries are
// enriched by id match (context/output/tools/pricing) — the catalog NEVER
// contributes model ids. Probed ⊕ manual merge deduped (manual wins),
// filtered by visibility.
type SourcingService struct {
	Log  *slog.Logger
	HTTP *http.Client

	// ProbeTTL is the /models probe cache TTL (default 5 min).
	ProbeTTL time.Duration

	mu       sync.Mutex
	probeTTL time.Duration
	cache    map[string]probeEntry // provider id → probed models
}

type probeEntry struct {
	models    []ModelInfo
	fetchedAt time.Time
	failed    bool
}

// NewSourcingService wires the service. log may be nil.
func NewSourcingService(log *slog.Logger, httpc *http.Client) *SourcingService {
	return &SourcingService{Log: log, HTTP: httpc, cache: map[string]probeEntry{}}
}

// ListResult is one ListModels result with degraded status.
type ListResult struct {
	Models   []ModelInfo
	Degraded bool // probe failed — NO models served (manual entries still list)
}

// ListModels resolves models for a provider profile.
//
// Bearer (optional): a resolved credential to send on the /models probe
// (the CredentialResolver's secret-first, env-fallback output). The zero
// value ("") sends no Authorization header (Ollama, public endpoints, and
// the registry's own models fn — which has no request tenant at build
// time). The probe cache key folds the bearer's hash in, so a credential
// rotation invalidates stale unauthenticated entries.
func (s *SourcingService) ListModels(ctx context.Context, p Profile, bearer ...string) ListResult {
	auth := ""
	if len(bearer) > 0 {
		auth = bearer[0]
	}
	key := probeKey(p.ID, auth, p.BaseURL)

	var out []ModelInfo
	degraded := false

	// 1. The vendored catalog is NOT a model SOURCE — live truth only
	// (operator directive: synthesized lists confuse users during
	// connection issues). The catalog serves as metadata enrichment:
	// after a successful probe, entries are enriched by ID match with
	// catalog context/pricing — never adding ids the endpoint did not
	// report. Manual entries remain the operator's explicit model list.

	// 2. probe — THE source for built-ins and customs alike (with manual
	// entries merged). For built-ins: whenever the operator has
	// personalized the provider (base-URL override or stored/env
	// credential) OR always (every built-in has a documented endpoint;
	// a failed probe is non-fatal and yields an EMPTY list — never the
	// vendored snapshot, per the no-synthesized-models directive).
	// Override endpoints stay authoritative for their model list.
	probeWanted := true
	switch {
	case p.Custom && p.Kind == ProfileKindAnthropic:
		// Custom anthropic-wire profiles have no listable endpoint shape.
		probeWanted = false
	}
	if probeWanted {
		probed, ok := s.probe(ctx, key, p, auth)
		if !ok {
			degraded = true
			s.warn("sourcing: probe failed for provider %s — degraded, NO models served (no synthesized fallback); fix the endpoint/token or add manual entries", p.ID)
		} else {
			out = append(out, probed...)
			// Catalog = metadata enrichment by id match (context/output/
			// tools/pricing), never new ids. GetModelForProvider is
			// alias-aware: live ids that differ from catalog keys (e.g.
			// commandcode's deepseek/deepseek-v4-flash under a newer id
			// spelling) still enrich when the catalog aliases them.
			for i := range out {
				c, ok := GetModelForProvider(p.ID, out[i].ID)
				if !ok {
					continue
				}
				if out[i].Context <= 0 && c.Context > 0 {
					out[i].Context = c.Context
				}
				if out[i].MaxOutput <= 0 && c.MaxOutput > 0 {
					out[i].MaxOutput = c.MaxOutput
				}
				if out[i].Tools == nil && c.Tools != nil {
					t := *c.Tools
					out[i].Tools = &t
				}
				if len(out[i].ReasoningEfforts) == 0 && len(c.ReasoningEfforts) > 0 {
					out[i].ReasoningEfforts = c.ReasoningEfforts
				}
				if out[i].Pricing == nil && c.Pricing != nil {
					out[i].Pricing = c.Pricing
				}
			}
		}
	}

	// 3. manual entries (manual wins on dedupe, D9).
	out = dedupeManualFirst(out, p.ManualModels)

	// visibility: hidden-model toggles + Visible flag.
	hidden := p.hiddenSet()
	filtered := make([]ModelInfo, 0, len(out))
	for _, m := range out {
		if hidden[m.ID] || !m.Visible {
			continue
		}
		if m.Context <= 0 {
			s.warn("sourcing: model %s/%s has no context hint — picker WARN; compaction must not guess", p.ID, m.ID)
		}
		filtered = append(filtered, m)
	}
	return ListResult{Models: filtered, Degraded: degraded}
}

// warn logs when a logger is wired.
func (s *SourcingService) warn(format string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(fmt.Sprintf(format, args...))
	}
}

// dedupeManualFirst merges probed/catalog entries with manual entries;
// on id conflict the manual entry wins (operator intent beats probe).
func dedupeManualFirst(auto []ModelInfo, manual []ModelInfo) []ModelInfo {
	manualByID := make(map[string]ModelInfo, len(manual))
	for _, m := range manual {
		mc := m
		mc.Provenance = "manual"
		manualByID[m.ID] = mc
	}
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(auto)+len(manual))
	for _, m := range auto {
		if seen[m.ID] {
			continue
		}
		if mm, ok := manualByID[m.ID]; ok {
			seen[m.ID] = true
			out = append(out, mm)
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	for _, m := range manual {
		if !seen[m.ID] {
			out = append(out, manualByID[m.ID])
		}
	}
	return out
}

// probe GETs {baseURL}/models with the TTL cache (keyed by provider id ⊕
// bearer hash so credential rotations refetch).
func (s *SourcingService) probe(ctx context.Context, key string, p Profile, auth string) ([]ModelInfo, bool) {
	ttl := s.ProbeTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string]probeEntry{}
	}
	if e, ok := s.cache[key]; ok && time.Since(e.fetchedAt) < ttl {
		s.mu.Unlock()
		if e.failed {
			return nil, false
		}
		return e.models, true
	}
	s.mu.Unlock()

	models, ok := s.fetchModels(ctx, p, auth)
	s.mu.Lock()
	s.cache[key] = perProbeEntry(models, ok, time.Now())
	s.mu.Unlock()
	return models, ok
}

func perProbeEntry(models []ModelInfo, ok bool, now time.Time) probeEntry {
	return probeEntry{models: models, fetchedAt: now, failed: !ok}
}

// modelsURL derives the /models listing endpoint for a profile, per wire
// type — each provider's chat client derives its paths from the base, and
// the probe must derive its listing path the same way:
//   - commandcode: base is the bare origin (chat derives
//     /provider/v1/chat/completions and /provider/v1/messages) → probe
//     /provider/v1/models (their documented models endpoint).
//   - anthropic: base is the bare origin ("no /v1 — path adds it",
//     anthropic.go) → probe /v1/models (their documented Models API).
//   - ollama: base is the host root → probe /v1/models (the OpenAI-compat
//     listing; native discovery rides /api/tags in the OllamaClient).
//   - every other OpenAI-compatible profile: the base IS the version root
//     (e.g. https://api.openai.com/v1, https://opencode.ai/zen/go/v1) →
//     append /models directly.
func modelsURL(p Profile) string {
	base := strings.TrimRight(p.BaseURL, "/")
	switch p.Kind {
	case ProfileKindCommandCode:
		return base + "/provider/v1/models"
	case ProfileKindAnthropic, ProfileKindOllama:
		return base + "/v1/models"
	default:
		return base + "/models"
	}
}

// fetchModels performs the GET {baseURL}/models probe, tolerating servers
// that return unusable metadata (missing context/pricing fields).
func (s *SourcingService) fetchModels(ctx context.Context, p Profile, auth string) ([]ModelInfo, bool) {
	httpc := s.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL(p), nil)
	if err != nil {
		return nil, false
	}
	// Credential: the resolver-resolved bearer first (tenant secret or env
	// — the value the real client calls will use), then the env fallback.
	// A missing credential never fails the probe — it stays non-fatal.
	// Wire-specific auth headers: anthropic uses x-api-key (+ the
	// required anthropic-version header); everything else Bearer.
	switch {
	case p.Kind == ProfileKindAnthropic:
		req.Header.Set("anthropic-version", anthropicVersion)
		if auth != "" {
			req.Header.Set("x-api-key", auth)
		}
	case auth != "":
		req.Header.Set("authorization", "Bearer "+auth)
	case p.AuthEnv != "":
		if v := os.Getenv(p.AuthEnv); v != "" {
			req.Header.Set("authorization", "Bearer "+v)
		}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		s.warn("sourcing: probe %s unreachable: %v (from inside the control-plane container, localhost/0.0.0.0 are the container — use the docker bridge IP / host LAN IP; base must include the version root, e.g. …/v1)", modelsURL(p), err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.warn("sourcing: probe %s → HTTP %d (401 = token problem, 404 = wrong base shape — base must include the version root …/v1, HTML body = wrong endpoint)", modelsURL(p), resp.StatusCode)
		return nil, false
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
			// OpenAI-compat shape: context_length (vLLM et al) or
			// max_model_len (other compat servers).
			ContextLength int64 `json:"context_length"`
			MaxModelLen   int64 `json:"max_model_len"`
			// Anthropic Models API shape: max_input_tokens (+ max_tokens
			// for output). Extra struct fields for fields a payload lacks
			// are harmless (decode to zero); anthropic's `data[].id`
			// matches the OpenAI field name so one decode serves both.
			MaxInputTokens int64 `json:"max_input_tokens"`
			// llama.cpp server (/v1/models) reports the model's trained
			// context under meta.n_ctx_train (and, on some builds, as a
			// top-level n_ctx_train). Without these the local llama-serve
			// probe resolved Context=0 → "model window unknown" and the
			// window-trigger compaction stayed disarmed.
			NCtxTrain int64 `json:"n_ctx_train"`
			Meta      *struct {
				NCtxTrain int64 `json:"n_ctx_train"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := decodeBody(resp.Body, &body); err != nil {
		return nil, false
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID == "" {
			continue
		}
		m := ModelInfo{ID: d.ID, Visible: true, Provenance: "probe"}
		switch {
		case d.ContextLength > 0:
			m.Context = d.ContextLength
		case d.MaxModelLen > 0:
			m.Context = d.MaxModelLen
		case d.MaxInputTokens > 0:
			m.Context = d.MaxInputTokens
		case d.Meta != nil && d.Meta.NCtxTrain > 0:
			m.Context = d.Meta.NCtxTrain
		case d.NCtxTrain > 0:
			m.Context = d.NCtxTrain
		}
		out = append(out, m)
	}
	return out, len(out) > 0
}

func decodeBody(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	return dec.Decode(dst)
}

// probeKey derives the cache key: provider id ⊕ bearer-hash ⊕ base-URL
// hash. The bearer folds in so a credential rotation is never served a
// stale unauthenticated entry; the base URL folds in so a repaired /
// overridden URL probes fresh instead of hitting the failed entry cached
// for the previous URL (self-healing probes different candidates under
// the same provider id).
func probeKey(providerID, bearer, baseURL string) string {
	h := sha256.New()
	h.Write([]byte(bearer))
	h.Write([]byte{0})
	h.Write([]byte(baseURL))
	sum := h.Sum(nil)
	if bearer == "" && baseURL == "" {
		return providerID
	}
	return providerID + "#" + hex.EncodeToString(sum[:8])
}

// InvalidateProbe drops the probe cache for a provider (settings change).
// Every credential-variant key is dropped (the bearer is not known here).
func (s *SourcingService) InvalidateProbe(providerID string) {
	s.mu.Lock()
	for k := range s.cache {
		if k == providerID || strings.HasPrefix(k, providerID+"#") {
			delete(s.cache, k)
		}
	}
	s.mu.Unlock()
}
