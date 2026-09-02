package orchicon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Model sourcing (D9): probe (live truth) → manual, catalog=metadata-only

func TestSourcingProbeFailureYieldsNoModels(t *testing.T) {
	// Built-in profile, default endpoint, unreachable: no synthesized
	// fallback — an empty, degraded list (operator directive).
	p, _ := BuiltinProfile("anthropic")
	p.BaseURL = "http://127.0.0.1:1" // unreachable
	s := NewSourcingService(nil, nil)
	res := s.ListModels(context.Background(), p)
	if !res.Degraded {
		t.Fatal("unreachable endpoint must be visibly degraded")
	}
	if len(res.Models) != 0 {
		t.Fatalf("no synthesized fallback allowed: %d models", len(res.Models))
	}
}

func TestSourcingProbeServesLiveList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-sonnet-4"},{"id":"claude-opus-4"}]}`))
	}))
	t.Cleanup(srv.Close)
	p, _ := BuiltinProfile("anthropic")
	p.BaseURL = srv.URL
	s := NewSourcingService(nil, nil)
	res := s.ListModels(context.Background(), p)
	if res.Degraded {
		t.Fatal("live probe must not degrade")
	}
	// Live ids served; catalog ENRICHES the known one (context hint), never
	// adds unknown ids.
	byID := map[string]ModelInfo{}
	for _, m := range res.Models {
		byID[m.ID] = m
	}
	if len(res.Models) != 2 || byID["claude-sonnet-4"].Context == 0 {
		t.Fatalf("live list + enrichment = %#v", res.Models)
	}
}

func TestSourcingProbeCustomEntries(t *testing.T) {
	reqs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs++
		w.Header().Set("Content-Type", "application/json")
		// vLLM-style: context_length per model.
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"meta-llama/Llama-3.2-3B","context_length":131072},{"id":"qwen/qwen3-8b","max_model_len":32768}]}`))
	}))
	t.Cleanup(srv.Close)

	p := Profile{ID: "my-vllm", Kind: ProfileKindCustom, Custom: true, BaseURL: srv.URL, Visible: true}
	s := NewSourcingService(nil, nil)
	res := s.ListModels(context.Background(), p)
	if res.Degraded || len(res.Models) != 2 {
		t.Fatalf("probe result = %#v deg=%v", res.Models, res.Degraded)
	}
	if res.Models[0].Context != 131072 || res.Models[1].Context != 32768 {
		t.Fatalf("context hints = %d/%d", res.Models[0].Context, res.Models[1].Context)
	}
	// TTL cache: second call does not refetch.
	s.ListModels(context.Background(), p)
	if reqs != 1 {
		t.Fatalf("probe not cached: %d requests", reqs)
	}
	// InvalidateProbe forces a refetch.
	s.InvalidateProbe("my-vllm")
	s.ListModels(context.Background(), p)
	if reqs != 2 {
		t.Fatalf("invalidate broken: %d", reqs)
	}
}

func TestSourcingProbeFailureIsDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // server does not expose /models
	}))
	t.Cleanup(srv.Close)

	var logs []string
	log := slog.New(slogMock{lines: &logs})
	p := Profile{ID: "my-llama", Kind: ProfileKindCustom, Custom: true, BaseURL: srv.URL, Visible: true,
		ManualModels: []ModelInfo{{ID: "local-model", Context: 8192, Visible: true}}}
	s := NewSourcingService(log, nil)
	res := s.ListModels(context.Background(), p)
	if !res.Degraded {
		t.Fatal("probe failure must be visibly degraded")
	}
	if len(res.Models) != 1 || res.Models[0].ID != "local-model" {
		t.Fatalf("manual entries must survive probe failure: %#v", res.Models)
	}
	// The diagnosable per-attempt log (HTTP status line) fires BEFORE the
	// degraded directive line — scan all lines for the directive contract
	// (degraded concept + no-fallback directive), not just the first.
	found := false
	for _, l := range logs {
		if strings.Contains(l, "degraded") && strings.Contains(l, "NO models served") {
			found = true
		}
	}
	if !found {
		t.Fatalf("degradation must be logged with the no-fallback directive: %v", logs)
	}
}

func TestSourcingMergeManualWinsDeduped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"shared","context_length":1024},{"id":"probed-only","context_length":2048}]}`))
	}))
	t.Cleanup(srv.Close)
	p := Profile{ID: "x", Kind: ProfileKindCustom, Custom: true, BaseURL: srv.URL, Visible: true,
		ManualModels: []ModelInfo{{ID: "shared", Context: 999999, Visible: true}, {ID: "manual-only", Context: 512, Visible: true}}}
	s := NewSourcingService(nil, nil)
	res := s.ListModels(context.Background(), p)
	byID := map[string]ModelInfo{}
	for _, m := range res.Models {
		if byID[m.ID].ID != "" {
			t.Fatalf("duplicate id %s", m.ID)
		}
		byID[m.ID] = m
	}
	if byID["shared"].Context != 999999 || byID["shared"].Provenance != "manual" {
		t.Fatalf("manual must win: %#v", byID["shared"])
	}
	if byID["probed-only"].Context != 2048 {
		t.Fatal("probed entry missing")
	}
	if byID["manual-only"].Context != 512 {
		t.Fatal("manual-only entry missing")
	}
}

func TestSourcingVisibilityToggles(t *testing.T) {
	// Visibility must filter even a manual-only list (probe unreachable):
	// hidden ids never surface regardless of source.
	p := Profile{ID: "vis", Kind: ProfileKindCustom, Custom: true, BaseURL: "http://127.0.0.1:1", Visible: true,
		ManualModels: []ModelInfo{{ID: "claude-opus-4", Context: 8192, Visible: true}, {ID: "keep-me", Context: 4096, Visible: true}}}
	p.HiddenModels = []string{"claude-opus-4"}
	s := NewSourcingService(nil, nil)
	res := s.ListModels(context.Background(), p)
	for _, m := range res.Models {
		if m.ID == "claude-opus-4" {
			t.Fatal("hidden model must be filtered")
		}
	}
	if len(res.Models) != 1 || res.Models[0].ID != "keep-me" {
		t.Fatalf("other manual models must remain: %#v", res.Models)
	}
}

func TestSourcingWarnsNoContextHint(t *testing.T) {
	var logs []string
	log := slog.New(slogMock{lines: &logs})

	// A manual entry WITHOUT a context hint triggers the WARN.
	p2 := Profile{ID: "y", Kind: ProfileKindCustom, Custom: true, BaseURL: "http://x", Visible: true,
		ManualModels: []ModelInfo{{ID: "no-hint", Visible: true}}}
	s2 := NewSourcingService(log, nil)
	s2.ListModels(context.Background(), p2)
	found := false
	for _, l := range logs {
		if strings.Contains(l, "no-hint") && strings.Contains(l, "no context hint") {
			found = true
		}
	}
	if !found {
		t.Fatalf("WARN for missing context hint required: %v", logs)
	}
}

// slogMock captures slog records.
type slogMock struct{ lines *[]string }

func (m slogMock) Enabled(context.Context, slog.Level) bool { return true }
func (m slogMock) Handle(_ context.Context, r slog.Record) error {
	*m.lines = append(*m.lines, r.Message)
	return nil
}
func (m slogMock) WithAttrs(attrs []slog.Attr) slog.Handler { return m }
func (m slogMock) WithGroup(name string) slog.Handler       { return m }

var _ = sync.Mutex{}
var _ = time.Now

var _ = fmt.Sprintf
