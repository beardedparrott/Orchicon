package orchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Ollama native metadata + num_ctx ----------------------------------------

func TestOllamaNativeChatNumCtx(t *testing.T) {
	var reqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		reqBody = string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		// Native API streams NDJSON — one JSON object per line.
		_, _ = w.Write([]byte(
			`{"message":{"content":"Hello"}}` + "\n" +
				`{"message":{"content":" world"}}` + "\n" +
				`{"message":{"tool_calls":[{"function":{"name":"fn","arguments":{"a":1}}}]}}` + "\n" +
				`{"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":22}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "llama3.2", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	evs, err2 := drainStream(t, ts)
	if err2 != nil {
		t.Fatalf("native stream: %v", err2)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hello" {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != " world" {
		t.Fatalf("event 1 = %#v", evs[1])
	}
	tc, ok := evs[2].(ToolCall)
	if !ok || tc.Name != "fn" || tc.ArgsJSON != `{"a":1}` {
		t.Fatalf("tool call = %#v", evs[2])
	}
	fin := evs[3].(Finish)
	if fin.Usage.InputTokens != 11 || fin.Usage.OutputTokens != 22 || fin.StopReason != StopStop {
		t.Fatalf("finish = %#v", fin)
	}

	// num_ctx rides native /api/chat options.
	var sent struct {
		Options struct {
			NumCtx int64 `json:"num_ctx"`
		} `json:"options"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(reqBody), &sent); err != nil {
		t.Fatalf("request: %v (%s)", err, reqBody)
	}
	if sent.Options.NumCtx != 8192 {
		t.Fatalf("num_ctx = %d, want 8192", sent.Options.NumCtx)
	}

	// Per-request override wins over the profile default.
	c2 := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	ts2, _ := c2.StreamTurn(context.Background(), TurnRequest{Model: "m", OllamaNumCtx: 16384})
	drainStream(t, ts2)
	var sent2 map[string]any
	_ = json.Unmarshal([]byte(reqBody), &sent2)
	if got := sent2["options"].(map[string]any)["num_ctx"]; got != float64(16384) {
		t.Fatalf("override num_ctx = %v", got)
	}
}

func TestOllamaWarnsWhenContextBelowModelMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			// TRUE context length from model_info["<arch>.context_length"].
			_, _ = w.Write([]byte(`{"model_info":{"llama3.context_length":131072},"capabilities":["tools","completion"]}`))
		case "/api/chat":
			_, _ = w.Write([]byte(`{"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	var warns []string
	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192, Warnf: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "llama3.2"})
	drainStream(t, ts)
	if len(warns) == 0 || !strings.Contains(warns[0], "silently truncate") || !strings.Contains(warns[0], "131072") {
		t.Fatalf("want truncation warning, got %v", warns)
	}
}

func TestOllamaWarnsNoContextHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `[DONE]`)))
	}))
	t.Cleanup(srv.Close)

	var warns []string
	c := &OllamaClient{Host: srv.URL, Warnf: func(f string, a ...any) { warns = append(warns, fmt.Sprintf(f, a...)) }}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "mystery-model"})
	drainStream(t, ts)
	found := false
	for _, w := range warns {
		if strings.Contains(w, "no context hint") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want no-context-hint warning, got %v", warns)
	}
}

func TestOllamaShowContextLengthAndCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model_info":{"qwen3.context_length":262144},"capabilities":["tools"]}`))
	}))
	t.Cleanup(srv.Close)
	c := &OllamaClient{Host: srv.URL}
	if got := c.contextLength(context.Background(), "qwen3-coder"); got != 262144 {
		t.Fatalf("context length = %d, want 262144", got)
	}
	sh, err := c.show(context.Background(), "qwen3-coder")
	if err != nil {
		t.Fatal(err)
	}
	if !sh.supportsTools() {
		t.Fatal("capabilities must report tools")
	}
	// Cached — second call does not refetch.
	if got := c.contextLength(context.Background(), "qwen3-coder"); got != 262144 {
		t.Fatal("cache broken")
	}
}

func TestOllamaTagsListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:latest","model":"llama3.2","size":2019393189},{"name":"qwen3-coder:latest"}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"model_info":{"llama.context_length":131072}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &OllamaClient{Host: srv.URL}
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v", models)
	}
	if models[0].ID != "llama3.2:latest" || models[0].Context != 131072 {
		t.Fatalf("model 0 = %#v", models[0])
	}
}

func TestOllamaCompatFallbackWithoutNumCtx(t *testing.T) {
	// No num_ctx configured → OpenAI-compat /v1 route.
	hitCompat := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			hitCompat = true
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`, `[DONE]`)))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c := &OllamaClient{Host: srv.URL}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts)
	if !hitCompat {
		t.Fatal("expected /v1/chat/completions fallback")
	}
}

func TestOllamaNativeDisconnectFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"content":"partial"}}` + "\n")) // no done chunk
	}))
	t.Cleanup(srv.Close)
	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 4096}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	_, err := drainStream(t, ts)
	if err == nil || !strings.Contains(err.Error(), "done chunk") {
		t.Fatalf("want clean failure on missing done chunk, got %v", err)
	}
}

// AC (final-chunk parity): a final native chunk carrying BOTH tail content
// AND done:true must surface the tail text BEFORE the Finish — the old
// shape dropped the tail delta and truncated output.
func TestOllamaNativeTailContentOnDoneChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(
			`{"message":{"content":"Hello"}}` + "\n" +
				`{"message":{"content":" tail"},"done":true,"done_reason":"stop","prompt_eval_count":5,"eval_count":9}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "llama3.2", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	evs, err2 := drainStream(t, ts)
	if err2 != nil {
		t.Fatalf("native stream: %v", err2)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3 (Hello, tail, Finish): %#v", len(evs), evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hello" {
		t.Fatalf("event 0 = %#v, want Hello", evs[0])
	}
	td, ok := evs[1].(TextDelta)
	if !ok || td.Text != " tail" {
		t.Fatalf("event 1 = %#v, want the tail TextDelta", evs[1])
	}
	fin, ok := evs[2].(Finish)
	if !ok || fin.StopReason != StopStop {
		t.Fatalf("event 1 = %#v, want Finish(stop)", evs[1])
	}
	// num_predict rides options when MaxTokens is set (output-cap parity).
	c2 := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	if _, err := c2.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 777, OllamaNumCtx: 4096}); err != nil {
		t.Fatal(err)
	}
}
