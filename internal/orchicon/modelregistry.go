package orchicon

// modelregistry.go — the LIVE model-metadata registry.
//
// Distinct from registry.go, which resolves provider PROFILES into clients
// (ADR-0003 D10). This file resolves model METADATA (context/output/tools/pricing).
//
// The vendored catalog (catalog.json, the file that calls itself "a mini
// models.dev") is an AUTHORED SNAPSHOT: it drifts, and a hand-typed number in it
// is indistinguishable from a verified one. This adds a live source behind the
// same lookup, so a model's context window comes from a maintained registry
// instead of from whoever last edited the snapshot — and so the number never has
// to be typed by hand at all.
//
// Resolution order, each step gated on the previous yielding nothing:
//
//	1. manual entry         (the operator's explicit declaration, D9 — wins on dedupe)
//	2. live provider probe  (the provider knows its own models)
//	3. THIS registry        (maintained third-party metadata)
//	4. vendored catalog     (offline snapshot)
//
// Constraints carried over verbatim from the catalog's contract:
//
//   - METADATA ENRICHMENT BY ID MATCH ONLY. The registry never introduces a
//     model id: it enriches ids the provider actually reported. Listing stays
//     driven by the probe, so no model is ever synthesized.
//   - Fail SOFT. A plane with no internet must behave exactly as it did before
//     this existed. A registry outage must never fail or block a model list —
//     no cache and no network simply falls through to the vendored catalog.
//   - 0 stays 0. A window nobody can supply remains unknown; the picker WARNs
//     and compaction must not guess (D8).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// RegistryEnvURL overrides the registry endpoint. An empty value uses the
	// default; RegistryDisabled turns the registry off entirely.
	RegistryEnvURL = "ORCHICON_MODEL_REGISTRY_URL"
	// RegistryDisabled is the RegistryURL value (explicit field or env) that
	// disables the registry — the escape hatch for air-gapped or restricted
	// installs that must make no outbound request at all.
	RegistryDisabled = "-"
	// DefaultRegistryURL is models.dev: keyed provider/model (the SAME shape as
	// the vendored catalog and catalogRefKey), no auth, open source
	// (github.com/sst/models.dev).
	DefaultRegistryURL = "https://models.dev/api.json"
	// defaultDataDir mirrors config.DataDir's default (ORCHICON_DATA_DIR). Named
	// locally rather than imported so the substrate keeps no config dependency.
	defaultDataDir = "/var/lib/orchicon"
	// DataDirEnv mirrors config's env name for the same reason.
	DataDirEnv = "ORCHICON_DATA_DIR"

	// DefaultRegistryTTL is how long a fetched body is served before it is
	// revalidated. models.dev answers with `cache-control: max-age=0,
	// must-revalidate` plus a strong ETag, so the server is explicitly asking to
	// be polled cheaply: a revalidation is a 304 with an empty body.
	DefaultRegistryTTL = 6 * time.Hour
	// registryTimeout bounds the fetch so a black-holed endpoint can never stall
	// a model list.
	registryTimeout = 10 * time.Second
	// registryMaxBytes caps the body. The live payload is ~4.6 MB plain / ~380 KB
	// gzipped; this is headroom against a hostile or broken response, not a
	// target.
	registryMaxBytes = 32 << 20
	// registryCacheName / registryETagName are the disk-cache files under
	// CacheDir, so a restart serves the registry without a network round trip.
	registryCacheName = "model-registry.json"
	registryETagName  = "model-registry.etag"
)

// registryAliases maps a PROVIDER'S OWN model id (as its /models endpoint reports
// it) onto the registry key describing the same model, for ids the registry does
// not list under that spelling.
//
// ID MAPPING ONLY — never a value. The distinction is the whole point: "this id
// and that id name the same model" is a factual claim anyone can check against
// the registry page, whereas inventing a context number is not. Every number
// still comes from the registry entry itself.
//
// It is EMPTY today, and that is a FINDING rather than an oversight. The case
// that looked like it needed one — DeepSeek's bare `deepseek-flash`, which
// models.dev DISPLAYS as "DeepSeek V4.1 Flash" — turned out to be a real key of
// the `deepseek` provider verbatim. Verified against the live payload, provider
// deepseek (api https://api.deepseek.com) exposes exactly four models:
//
//	key="deepseek-flash"                name="DeepSeek V4.1 Flash"     ctx=1000000
//	key="deepseek-v4-flash"             name="DeepSeek V4 Flash"      ctx=1000000
//	key="deepseek-v4-flash-vision-exp"  name="DeepSeek V4 Flash Vision Exp" ctx=1000000
//	key="deepseek-v4-pro"               name="DeepSeek V4 Pro"        ctx=1000000
//
// So exact match resolves it and an alias would have BROKEN a working lookup by
// redirecting it to `deepseek/deepseek-v4.1-flash` — a key that exists only
// under OTHER providers (openrouter, vercel, …), not under `deepseek`. The
// mechanism stays so a genuinely unmatched id can be curated here later.
var registryAliases = map[string]string{}

// registryCache holds the service-scoped fetch state. A struct rather than loose
// fields so this file owns its own locking discipline (SourcingService's mu
// guards the PROBE cache; the registry must not contend with it).
type registryCache struct {
	mu   sync.Mutex
	body map[string]ModelInfo
	etag string
	at   time.Time
}

// mdModel is the models.dev model shape (only the fields consumed here).
type mdModel struct {
	ID               string `json:"id"`
	ToolCall         bool   `json:"tool_call"`
	Reasoning        bool   `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	// Cost is USD per MILLION tokens — the same unit the catalog and Pricing
	// use, so no scaling is applied on the way in.
	Cost *struct {
		Input     float64 `json:"input"`
		Output    float64 `json:"output"`
		CacheRead float64 `json:"cache_read"`
	} `json:"cost"`
}

// mdProvider is one models.dev provider block.
type mdProvider struct {
	ID     string             `json:"id"`
	Models map[string]mdModel `json:"models"`
}

// registryURL resolves the effective endpoint. Empty result = disabled.
func (s *SourcingService) registryURL() string {
	raw := s.RegistryURL
	if raw == "" {
		raw = os.Getenv(RegistryEnvURL)
	}
	raw = strings.TrimSpace(raw)
	switch raw {
	case RegistryDisabled:
		return ""
	case "":
		return DefaultRegistryURL
	}
	return raw
}

// registryCacheDir resolves where the body + ETag are cached.
func (s *SourcingService) registryCacheDir() string {
	if d := strings.TrimSpace(s.CacheDir); d != "" {
		return d
	}
	if d := strings.TrimSpace(os.Getenv(DataDirEnv)); d != "" {
		return d
	}
	return defaultDataDir
}

func (s *SourcingService) registryTTL() time.Duration {
	if s.RegistryTTL > 0 {
		return s.RegistryTTL
	}
	return DefaultRegistryTTL
}

// registryProvenance names the source for traceability: the operator must be able
// to tell a registry-sourced number from a probe-sourced or snapshot one.
func registryProvenance(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "registry"
	}
	return "registry:" + u.Hostname()
}

// registryModels returns the current registry model map (keyed "provider/model"),
// loading or revalidating as needed. Returns nil when the registry is disabled or
// nothing is available — callers then fall through to the vendored catalog.
//
// The fetch happens OUTSIDE the lock (mirroring probe()): a slow or black-holed
// endpoint may cause a duplicate concurrent fetch, but it can never block another
// provider's model list.
func (s *SourcingService) registryModels(ctx context.Context) map[string]ModelInfo {
	endpoint := s.registryURL()
	if endpoint == "" {
		return nil
	}

	s.reg.mu.Lock()
	if s.reg.body != nil && time.Since(s.reg.at) < s.registryTTL() {
		models := s.reg.body
		s.reg.mu.Unlock()
		return models
	}
	prev := s.reg.body
	s.reg.mu.Unlock()

	models, etag, ok := s.fetchRegistry(ctx, endpoint, s.registryCacheDir(), prev)

	s.reg.mu.Lock()
	defer s.reg.mu.Unlock()
	if !ok {
		// FAIL SOFT: keep serving whatever we already had (memory, else disk, else
		// nothing). Never clear a working map because one fetch failed.
		return s.reg.body
	}
	s.reg.body = models
	s.reg.etag = etag
	s.reg.at = time.Now()
	return models
}

// fetchRegistry performs the GET (+ If-None-Match revalidation) and owns the disk
// cache. It is deliberately total: every failure path returns a usable answer or
// ok=false, never an error the caller must handle.
func (s *SourcingService) fetchRegistry(ctx context.Context, endpoint, dir string, prev map[string]ModelInfo) (map[string]ModelInfo, string, bool) {
	bodyPath := filepath.Join(dir, registryCacheName)
	etagPath := filepath.Join(dir, registryETagName)

	diskBody, diskETagRaw := readCacheFile(bodyPath), readCacheFile(etagPath)
	haveDisk := len(diskBody) > 0
	diskETag := strings.TrimSpace(string(diskETagRaw))

	httpc := s.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}

	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}
	req.Header.Set("Accept", "application/json")
	// Conditional request: the server advertises max-age=0 + a strong ETag, so an
	// unchanged registry answers 304 and costs no body at all.
	if haveDisk && diskETag != "" {
		req.Header.Set("If-None-Match", diskETag)
	}

	resp, err := httpc.Do(req)
	if err != nil {
		s.warn("sourcing: model registry fetch failed for %s — serving the cached/offline metadata (not fatal)", endpoint)
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		// Unchanged: reuse the cached body.
		if haveDisk {
			return parseRegistry(diskBody, registryProvenance(endpoint)), diskETag, true
		}
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}
	if resp.StatusCode != http.StatusOK {
		s.warn("sourcing: model registry returned HTTP %d — serving the cached/offline metadata", resp.StatusCode)
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxBytes))
	if err != nil || len(raw) == 0 {
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}
	newETag := resp.Header.Get("ETag")

	// Persist for the next boot (best effort: a read-only data dir must not fail
	// the fetch).
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(bodyPath, raw, 0o644)
		if newETag != "" {
			_ = os.WriteFile(etagPath, []byte(newETag), 0o644)
		}
	}

	models := parseRegistry(raw, registryProvenance(endpoint))
	if len(models) == 0 {
		// Parsed to nothing (shape change, or a truncated body): treat as failure
		// so a working map is never replaced with an empty one.
		s.warn("sourcing: model registry body parsed to zero models — keeping the previous metadata")
		return registryFallback(prev, diskBody, diskETag, haveDisk)
	}
	return models, newETag, true
}

// registryFallback picks the best available answer when the network path fails:
// the in-memory map, else the cached body, else nothing (ok=false → the vendored
// catalog stands).
func registryFallback(prev map[string]ModelInfo, diskBody []byte, diskETag string, haveDisk bool) (map[string]ModelInfo, string, bool) {
	if prev != nil {
		return prev, diskETag, true
	}
	if haveDisk {
		// The endpoint is unavailable to re-derive the host from, so the cached
		// body is attributed generically.
		return parseRegistry(diskBody, "registry"), diskETag, true
	}
	return nil, "", false
}

func readCacheFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// parseRegistry converts a models.dev body into the catalog's own key format
// ("provider/model" → ModelInfo), so the lookup and every downstream consumer are
// identical to the vendored path.
func parseRegistry(raw []byte, provenance string) map[string]ModelInfo {
	blobs := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &blobs); err != nil {
		return nil
	}
	out := make(map[string]ModelInfo, 4096)
	for providerID, provRaw := range blobs {
		var p mdProvider
		if err := json.Unmarshal(provRaw, &p); err != nil {
			continue
		}
		if len(p.Models) == 0 {
			continue // not a provider block (skips any non-provider top-level key)
		}
		for modelKey, m := range p.Models {
			id := m.ID
			if id == "" {
				id = modelKey
			}
			mi := ModelInfo{
				ID:         id,
				Context:    m.Limit.Context,
				MaxOutput:  m.Limit.Output,
				Visible:    true,
				Provenance: provenance,
			}
			if m.ToolCall {
				t := true
				mi.Tools = &t
			}
			mi.ReasoningEfforts = effortValues(m.ReasoningOptions)
			if m.Cost != nil {
				mi.Pricing = &Pricing{
					InputPerM:     m.Cost.Input,
					OutputPerM:    m.Cost.Output,
					CacheReadPerM: m.Cost.CacheRead,
					Currency:      "USD",
					Source:        provenance,
				}
			}
			out[catalogRefKey(providerID, modelKey)] = mi
		}
	}
	return out
}

// effortValues extracts the effort ladder from models.dev's reasoning_options,
// which is a list of option kinds (toggle | effort | budget_tokens). Only an
// explicit effort ladder maps onto ModelInfo.ReasoningEfforts.
func effortValues(opts []struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}) []string {
	for _, o := range opts {
		if o.Type == "effort" && len(o.Values) > 0 {
			return o.Values
		}
	}
	return nil
}

// registryLookup is the pure lookup: alias-aware, exact-match only. It performs no
// I/O and no heuristics — an id the registry does not list under its own spelling
// or a curated alias stays unresolved, deliberately. Family matching was rejected:
// a family holds several versions, so picking one would be a guess wearing a
// citation.
func registryLookup(models map[string]ModelInfo, provider, id string) (ModelInfo, bool) {
	if len(models) == 0 {
		return ModelInfo{}, false
	}
	key := catalogRefKey(provider, id)
	if alias, ok := registryAliases[key]; ok {
		key = alias
	}
	m, ok := models[key]
	return m, ok
}

// applyRegistryMeta fills a model's EMPTY metadata fields from a registry entry.
//
// It never overwrites a value the provider's own probe reported (the probe is the
// authority on its own models) and never clears one. Provenance is stamped ONLY
// when a field actually came from here, so a row whose metadata is entirely
// provider-reported never claims the registry as its source.
//
// Returns true when at least one field was taken.
func applyRegistryMeta(m *ModelInfo, r ModelInfo) bool {
	used := false
	if m.Context <= 0 && r.Context > 0 {
		m.Context = r.Context
		used = true
	}
	if m.MaxOutput <= 0 && r.MaxOutput > 0 {
		m.MaxOutput = r.MaxOutput
		used = true
	}
	if m.Tools == nil && r.Tools != nil {
		t := *r.Tools
		m.Tools = &t
		used = true
	}
	if len(m.ReasoningEfforts) == 0 && len(r.ReasoningEfforts) > 0 {
		m.ReasoningEfforts = r.ReasoningEfforts
		used = true
	}
	if m.Pricing == nil && r.Pricing != nil {
		m.Pricing = r.Pricing
		used = true
	}
	if used {
		m.Provenance = r.Provenance
	}
	return used
}

// RegistryModel exposes one registry lookup (tests / diagnostics). ok=false when
// the registry is disabled, unavailable, or has no entry for the id.
func (s *SourcingService) RegistryModel(ctx context.Context, provider, id string) (ModelInfo, bool) {
	return registryLookup(s.registryModels(ctx), provider, id)
}

// RegistryDebug renders the registry's current state (diagnostics).
func (s *SourcingService) RegistryDebug() string {
	models := s.registryModels(context.Background())
	if models == nil {
		return "registry: disabled or unavailable"
	}
	return fmt.Sprintf("registry: %d models", len(models))
}
