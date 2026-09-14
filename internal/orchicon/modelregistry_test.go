package orchicon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mdFixture is a models.dev-shaped body mirroring the VERIFIED real structure:
// a provider block keys its models by the ids ITS OWN api reports. The deepseek
// provider (api https://api.deepseek.com) keys the flash model as
// "deepseek-flash" while DISPLAYING it as "DeepSeek V4.1 Flash" — which is
// exactly the case that looked like it needed an alias and does not. It also
// includes an unknown top-level key (to prove a non-provider block is skipped)
// and a gateway provider whose keys are NAMESPACED (to prove one provider's
// spelling never answers for another).
func mdFixture() string {
	return `{
  "_meta": {"description": "not a provider"},
  "deepseek": {
    "id": "deepseek",
    "api": "https://api.deepseek.com",
    "models": {
      "deepseek-flash": {
        "id": "deepseek-flash",
        "name": "DeepSeek V4.1 Flash",
        "family": "deepseek-flash",
        "tool_call": true,
        "reasoning_options": [{"type":"effort","values":["low","high"]}],
        "limit": {"context": 1000000, "output": 384000},
        "cost": {"input": 0.1, "output": 0.4, "cache_read": 0.003}
      }
    }
  },
  "openrouter": {
    "id": "openrouter",
    "models": {
      "deepseek/deepseek-v4-flash": {
        "id": "deepseek/deepseek-v4-flash",
        "limit": {"context": 128000, "output": 32000}
      }
    }
  }
}`
}

// parseRegistry maps models.dev onto the catalog's own key format, so every
// downstream consumer (lookup, picker, composer, compaction) is identical.
func TestParseRegistryMapsTheModelsDevShape(t *testing.T) {
	m := parseRegistry([]byte(mdFixture()), "registry:models.dev")
	if len(m) != 2 {
		t.Fatalf("parsed %d entries, want 2 (the _meta block must be skipped)", len(m))
	}

	got, ok := m["deepseek/deepseek-flash"]
	if !ok {
		t.Fatalf("missing the deepseek entry; keys = %v", keysOf(m))
	}
	if got.Context != 1000000 {
		t.Errorf("context = %d, want 1000000", got.Context)
	}
	if got.MaxOutput != 384000 {
		t.Errorf("max_output = %d, want 384000", got.MaxOutput)
	}
	if got.Tools == nil || !*got.Tools {
		t.Errorf("tools = %v, want true (tool_call)", got.Tools)
	}
	if len(got.ReasoningEfforts) != 2 || got.ReasoningEfforts[1] != "high" {
		t.Errorf("reasoning_efforts = %v, want the effort ladder", got.ReasoningEfforts)
	}
	if got.Pricing == nil || got.Pricing.InputPerM != 0.1 || got.Pricing.OutputPerM != 0.4 {
		t.Errorf("pricing = %+v, want input 0.1 / output 0.4 (USD per million)", got.Pricing)
	}
	if got.Provenance != "registry:models.dev" {
		t.Errorf("provenance = %q, want the registry attribution", got.Provenance)
	}
}

// The provider's own id spelling resolves by EXACT match — no alias, no
// heuristics. DeepSeek's `deepseek-flash` is a real models.dev key (displayed as
// "DeepSeek V4.1 Flash"); naming it explicitly here pins that the resolution is
// the registry's own, and that an id the registry does not list stays unresolved
// rather than matching by family or prefix (a family holds several versions, so
// picking one would be a guess wearing a citation).
func TestRegistryLookupIsExactMatchOnly(t *testing.T) {
	models := parseRegistry([]byte(mdFixture()), "registry:models.dev")

	// The provider reports the bare id `deepseek-flash`. Resolves verbatim.
	got, ok := registryLookup(models, "deepseek", "deepseek-flash")
	if !ok {
		t.Fatal("deepseek-flash did not resolve — it must resolve by exact match")
	}
	if got.Context != 1000000 {
		t.Fatalf("context = %d, want the registry's own 1000000", got.Context)
	}
	if got.Provenance != "registry:models.dev" {
		t.Errorf("provenance = %q, want the registry attribution so the number is traceable", got.Provenance)
	}

	// An id the registry does not list, and that no alias names, stays
	// UNRESOLVED — never a family/prefix guess.
	for _, id := range []string{"deepseek-flash-0423", "deepseek", "flash", "deepseek-v4.1-flash"} {
		if _, ok := registryLookup(models, "deepseek", id); ok {
			t.Errorf("id %q resolved — the lookup must be exact-match (+ curated alias) only", id)
		}
	}

	// A different provider's block never answers for this one.
	if _, ok := registryLookup(models, "deepseek", "deepseek/deepseek-v4-flash"); ok {
		t.Error("a namespaced id must not resolve under the bare deepseek provider")
	}
}

// applyRegistryMeta fills only EMPTY fields: the provider's own probe is the
// authority on its own models and must never be overwritten by a registry.
func TestApplyRegistryMetaNeverOverwritesTheProbe(t *testing.T) {
	truth := true

	// A model the probe fully described keeps every value, and claims no external
	// source (otherwise every row would say "models.dev" whether or not it was used).
	probed := ModelInfo{ID: "m", Context: 200000, MaxOutput: 8192, Tools: &truth, Provenance: "probe"}
	if applyRegistryMeta(&probed, ModelInfo{Context: 1000000, MaxOutput: 384000, Provenance: "registry:models.dev"}) {
		t.Error("applyRegistryMeta reported a change on a fully-probed model")
	}
	if probed.Context != 200000 || probed.Provenance != "probe" {
		t.Errorf("probed model was overwritten: ctx=%d provenance=%q", probed.Context, probed.Provenance)
	}

	// A model the probe could not describe takes the registry's values, and is
	// stamped with the registry as their origin.
	sparse := ModelInfo{ID: "m", Provenance: "probe"}
	if !applyRegistryMeta(&sparse, ModelInfo{Context: 1000000, Provenance: "registry:models.dev"}) {
		t.Fatal("applyRegistryMeta should have filled an empty model")
	}
	if sparse.Context != 1000000 {
		t.Errorf("context = %d, want the registry value", sparse.Context)
	}
	if sparse.Provenance != "registry:models.dev" {
		t.Errorf("provenance = %q, want the registry attribution so the number is traceable", sparse.Provenance)
	}
}

// A 200 fetch is cached to disk; the next load revalidates with If-None-Match and
// a 304 keeps the cached body. This is what keeps a 4.6 MB registry from being
// re-downloaded on every control-plane start.
func TestRegistryRevalidatesWithIfNoneMatch(t *testing.T) {
	var hits, notModified int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.Header.Get("If-None-Match") == `"v1"` {
			atomic.AddInt64(&notModified, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(mdFixture()))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	s := NewSourcingService(nil, srv.Client())
	s.RegistryURL = srv.URL
	s.CacheDir = dir

	// First load: a 200, and the body lands in the cache.
	m, ok := s.RegistryModel(context.Background(), "deepseek", "deepseek-flash")
	if !ok || m.Context != 1000000 {
		t.Fatalf("first load: ok=%v context=%d", ok, m.Context)
	}
	if _, err := os.Stat(filepath.Join(dir, registryCacheName)); err != nil {
		t.Errorf("the fetched body was not cached: %v", err)
	}
	if etag, err := os.ReadFile(filepath.Join(dir, registryETagName)); err != nil || strings.TrimSpace(string(etag)) != `"v1"` {
		t.Errorf("the ETag was not cached: %q err=%v", etag, err)
	}

	// Force a revalidation and prove the second load is a 304 that reuses the
	// cached body (so the value survives with no new payload).
	s.RegistryTTL = time.Nanosecond
	m2, ok := s.RegistryModel(context.Background(), "deepseek", "deepseek-flash")
	if !ok || m2.Context != 1000000 {
		t.Fatalf("revalidated load: ok=%v context=%d", ok, m2.Context)
	}
	if n := atomic.LoadInt64(&notModified); n == 0 {
		t.Error("the revalidation sent no If-None-Match — a 4.6 MB registry would be re-downloaded every start")
	}
	if n := atomic.LoadInt64(&hits); n < 2 {
		t.Errorf("server saw %d requests, want a revalidation", n)
	}
}

// Fail SOFT: an unreachable registry with no cache resolves nothing, so the
// vendored catalog stands exactly as it did before the registry existed. A plane
// with no internet must behave identically to today.
func TestRegistryFailureFallsBackWithoutBreaking(t *testing.T) {
	s := NewSourcingService(nil, &http.Client{Timeout: time.Second})
	s.RegistryURL = "http://127.0.0.1:1/unreachable.json"
	s.CacheDir = t.TempDir()

	if _, ok := s.RegistryModel(context.Background(), "deepseek", "deepseek-flash"); ok {
		t.Fatal("an unreachable registry with no cache must resolve nothing")
	}
	if got := s.RegistryDebug(); !strings.Contains(got, "disabled or unavailable") {
		t.Errorf("RegistryDebug = %q, want it to report the registry unavailable", got)
	}
}

// Disabling the registry makes no request at all — the air-gapped escape hatch.
func TestRegistryDisabledMakesNoRequest(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	for _, off := range []string{RegistryDisabled, "  " + RegistryDisabled} {
		s := NewSourcingService(nil, srv.Client())
		s.RegistryURL = off
		s.CacheDir = t.TempDir()
		if _, ok := s.RegistryModel(context.Background(), "deepseek", "deepseek-flash"); ok {
			t.Errorf("RegistryURL=%q resolved a model, want the registry off", off)
		}
	}
	if n := atomic.LoadInt64(&hits); n != 0 {
		t.Errorf("the disabled registry still issued %d request(s)", n)
	}
}

func keysOf(m map[string]ModelInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
