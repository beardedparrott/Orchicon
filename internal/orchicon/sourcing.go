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

// SourcingService (D9): model sourcing per provider, fallback order
//  1. vendored catalog (built-in profiles),
//  2. GET {baseURL}/models probe (custom compat entries; TTL-cached,
//     non-fatal, visibly degraded on failure),
//  3. manual model entries (operator-added hints).
//
// Probed + manual merge deduped (manual wins), filtered by visibility.
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
	Degraded bool // probe failed — serving manual/catalog only
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
	key := probeKey(p.ID, auth)

	var out []ModelInfo
	degraded := false

	// 1. vendored catalog for built-in profiles.
	if !p.Custom {
		out = append(out, catalogListByProvider(p.ID)...)
	}

	// 2. probe for custom compat entries (TTL-cached, non-fatal), and for
	// built-ins whose effective base URL differs from the built-in table
	// (an env/override value means the operator pointed us at a different
	// endpoint — discover ITS models, not the vendored defaults).
	probeWanted := false
	overrideAuthoritative := false
	switch {
	case p.Custom && p.Kind != ProfileKindAnthropic:
		probeWanted = true
	default:
		if def, isBuiltin := builtinBaseURLs()[p.ID]; isBuiltin && p.BaseURL != def {
			probeWanted = true
			overrideAuthoritative = true
		}
	}
	if probeWanted {
		probed, ok := s.probe(ctx, key, p, auth)
		if !ok {
			degraded = true
			if overrideAuthoritative {
				s.warn("sourcing: probe failed for built-in %s (base-URL override %s) — serving vendored catalog", p.ID, p.BaseURL)
			} else {
				s.warn("sourcing: probe failed for provider %s — serving manual entries only (visibly degraded)", p.ID)
			}
		} else if overrideAuthoritative {
			// The override endpoint is authoritative: serve ITS models
			// instead of the vendored catalog (manual entries merge below).
			out = probed
		} else {
			out = append(out, probed...)
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

// modelsURL derives the /models listing endpoint for a profile. The
// commandcode profile's base is the bare origin (https://api.commandcode.ai)
// — the chat clients derive /provider/v1/chat/completions and
// /provider/v1/messages from it (internal/orchicon/commandcode.go), so the
// probe must derive /provider/v1/models the same way. Every other
// OpenAI-compatible profile treats the base as the version root and appends
// /models directly (e.g. https://opencode.ai/zen/go/v1 → /v1/models).
func modelsURL(p Profile) string {
	base := strings.TrimRight(p.BaseURL, "/")
	if p.Kind == ProfileKindCommandCode {
		return base + "/provider/v1/models"
	}
	return base + "/models"
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
	// Anthropic-wire endpoints authenticate with x-api-key, not Bearer.
	switch {
	case auth != "" && p.Kind == ProfileKindAnthropic:
		req.Header.Set("x-api-key", auth)
	case auth != "":
		req.Header.Set("authorization", "Bearer "+auth)
	case p.AuthEnv != "":
		if v := os.Getenv(p.AuthEnv); v != "" {
			req.Header.Set("authorization", "Bearer "+v)
		}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			MaxModelLen   int64  `json:"max_model_len"`
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
		if d.ContextLength > 0 {
			m.Context = d.ContextLength
		} else if d.MaxModelLen > 0 {
			m.Context = d.MaxModelLen
		}
		out = append(out, m)
	}
	return out, len(out) > 0
}

func decodeBody(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	return dec.Decode(dst)
}

// probeKey derives the cache key: provider id ⊕ bearer-hash suffix (a
// credential rotation must not be served a stale unauthenticated entry).
func probeKey(providerID, bearer string) string {
	if bearer == "" {
		return providerID
	}
	sum := sha256.Sum256([]byte(bearer))
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
