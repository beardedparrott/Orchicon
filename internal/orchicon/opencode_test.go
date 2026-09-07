package orchicon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- OpenCode model-aware routing (D2) --------------------------------------

func TestOpenCodeRouteFor(t *testing.T) {
	cases := []struct {
		provider, model string
		want            opencodeRoute
	}{
		// Zen chat-route models.
		{"opencode", "deepseek-v4-flash", opencodeRouteChat},
		{"opencode", "minimax-m2.5", opencodeRouteChat},
		{"opencode", "kimi-k2.5", opencodeRouteChat},
		{"opencode", "big-pickle", opencodeRouteChat},
		// Zen responses-route models.
		{"opencode", "muse-spark-1.3-contributor-free", opencodeRouteResponses},
		{"opencode", "gpt-5.1-codex", opencodeRouteResponses},
		// Zen messages-route (Claude).
		{"opencode", "claude-sonnet-4", opencodeRouteMessages},
		// Go chat-route models.
		{"opencode-go", "deepseek-v4-flash", opencodeRouteChat},
		// Go messages-route (MiniMax/Qwen).
		{"opencode-go", "minimax-m2.7", opencodeRouteMessages},
		{"opencode-go", "qwen3.6-plus", opencodeRouteMessages},
		// Go responses-route (Grok / GPT-5.6-Luna / Muse-Spark).
		{"opencode-go", "grok-4", opencodeRouteResponses},
		{"opencode-go", "gpt-5.6-luna", opencodeRouteResponses},
		{"opencode-go", "muse-spark-1.3-contributor-free", opencodeRouteResponses},
	}
	for _, c := range cases {
		if got := opencodeRouteFor(c.provider, c.model); got != c.want {
			t.Errorf("%s/%s route = %s, want %s", c.provider, c.model, got, c.want)
		}
	}
}

func TestOpenCodeRouteErrorNamesModelAndRoute(t *testing.T) {
	err := opencodeRouteError("opencode", "muse-spark-1.3-contributor-free", opencodeRouteResponses)
	msg := err.Error()
	if !strings.Contains(msg, "muse-spark-1.3-contributor-free") {
		t.Fatalf("error must name the model: %s", msg)
	}
	if !strings.Contains(msg, "/responses") {
		t.Fatalf("error must name the required route: %s", msg)
	}
	if !strings.Contains(msg, "opencode.ai/docs/zen") {
		t.Fatalf("error must cite the doc: %s", msg)
	}
}

func TestOpenCodeClientChatRouteStreams(t *testing.T) {
	var gotPath, gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("user-agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`, `[DONE]`)))
	}))
	t.Cleanup(srv.Close)

	c := &opencodeClient{provider: "opencode", baseURL: srv.URL, apiKey: "k", http: srv.Client()}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "deepseek-v4-flash", SessionID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts)
	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotSession != "conv-1" {
		t.Fatalf("x-opencode-session = %q, want conv-1", gotSession)
	}
	if !strings.HasPrefix(gotUA, "orchicon/") {
		t.Fatalf("user-agent = %q, want orchicon/…", gotUA)
	}
}

// The responses-route model now streams on the /responses wire (D2): the
// httptest asserts the path, session header, UA, and the delta→Finish+usage
// decode. This replaces the old loud-fail assertion.
func TestOpenCodeClientResponsesRouteStreams(t *testing.T) {
	var gotPath, gotSession, gotUA, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("user-agent")
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"type":"response.output_text.delta","delta":"Hi"}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":34,"input_tokens_details":{"cached_tokens":8}}}}`,
			`[DONE]`,
		)))
	}))
	t.Cleanup(srv.Close)

	c := &opencodeClient{provider: "opencode", baseURL: srv.URL, apiKey: "k", http: srv.Client()}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "muse-spark-1.3-contributor-free", SessionID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if gotSession != "conv-1" {
		t.Fatalf("x-opencode-session = %q, want conv-1", gotSession)
	}
	if !strings.HasPrefix(gotUA, "orchicon/") {
		t.Fatalf("user-agent = %q, want orchicon/…", gotUA)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %#v, want TextDelta + Finish", evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hi" {
		t.Fatalf("event 0 = %#v, want TextDelta Hi", evs[0])
	}
	fin, ok := evs[1].(Finish)
	if !ok {
		t.Fatalf("event 1 = %#v, want Finish", evs[1])
	}
	if fin.StopReason != StopStop {
		t.Fatalf("stop = %q, want stop", fin.StopReason)
	}
	u := fin.Usage
	if u.InputTokens != 4 || u.OutputTokens != 34 || u.CacheReadTokens != 8 {
		t.Fatalf("usage = %#v, want in=4 (fresh: 12−8) out=34 cacheRead=8", u)
	}
}

// Route auto-detect: an unlisted model the static table maps to chat but the
// server rejects with 404 on /chat/completions is retried once on /responses
// and succeeds; the second turn for the same model goes straight to
// /responses (cache hit — request count proves no second probe).
func TestOpenCodeClientRouteAutoDetectFallback(t *testing.T) {
	var chatHits, respHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			chatHits++
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"wrong wire"}}`))
		case "/responses":
			respHits++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse(
				`{"type":"response.output_text.delta","delta":"ok"}`,
				`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
				`[DONE]`,
			)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &opencodeClient{provider: "opencode", baseURL: srv.URL, apiKey: "k", http: srv.Client()}
	// First turn: chat 404 → fallback to responses → success.
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "unlisted-model"})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("first turn stream: %v", err)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "ok" {
		t.Fatalf("first turn event 0 = %#v", evs[0])
	}
	if chatHits != 1 || respHits != 1 {
		t.Fatalf("first turn hits chat=%d responses=%d, want 1/1", chatHits, respHits)
	}
	// Second turn: cache hit — straight to /responses, no chat probe.
	ts2, err := c.StreamTurn(context.Background(), TurnRequest{Model: "unlisted-model"})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	drainStream(t, ts2)
	if chatHits != 1 || respHits != 2 {
		t.Fatalf("second turn hits chat=%d responses=%d, want 1/2 (no re-probe)", chatHits, respHits)
	}
}

// Sticky bit with error-triggered flip: prime the cache to responses, then
// serve 404 on responses + success on chat — the fallback fires and the cache
// now points at chat.
func TestOpenCodeClientStickyRouteFlip(t *testing.T) {
	var chatHits, respHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			respHits++
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"wrong wire"}}`))
		case "/chat/completions":
			chatHits++
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`, `[DONE]`)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &opencodeClient{provider: "opencode", baseURL: srv.URL, apiKey: "k", http: srv.Client()}
	// Prime the cache to responses.
	c.cacheRoute("flip-model", opencodeRouteResponses)
	// First turn: cached responses → 404 → fallback to chat → success.
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "flip-model"})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	drainStream(t, ts)
	if respHits != 1 || chatHits != 1 {
		t.Fatalf("first turn hits responses=%d chat=%d, want 1/1", respHits, chatHits)
	}
	// Cache now points at chat.
	if got := c.cachedRoute("flip-model"); got != opencodeRouteChat {
		t.Fatalf("cached route = %s, want chat", got)
	}
	// Second turn: cache hit — straight to chat, no responses probe.
	ts2, err := c.StreamTurn(context.Background(), TurnRequest{Model: "flip-model"})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	drainStream(t, ts2)
	if respHits != 1 || chatHits != 2 {
		t.Fatalf("second turn hits responses=%d chat=%d, want 1/2", respHits, chatHits)
	}
}

// No failover on auth/rate/transient: 401/403/429/5xx never trigger a route
// retry or a cache flip.
func TestOpenCodeClientNoFailoverOnAuthRateTransient(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500} {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}))
		c := &opencodeClient{provider: "opencode", baseURL: srv.URL, apiKey: "k", http: srv.Client(), retry: RetryPolicy{MaxAttempts: 1}}
		_, err := c.StreamTurn(context.Background(), TurnRequest{Model: "deepseek-v4-flash"})
		if err == nil {
			t.Fatalf("status %d: want error", code)
		}
		if se, ok := err.(*StatusError); !ok || se.StatusCode != code {
			t.Fatalf("status %d: err = %v, want StatusError %d", code, err, code)
		}
		if hits != 1 {
			t.Fatalf("status %d: hits = %d, want 1 (no route retry)", code, hits)
		}
		// Cache must not have flipped (no cache entry for this model).
		if _, ok := c.routeCache.Load(c.provider + "|deepseek-v4-flash"); ok {
			t.Fatalf("status %d: cache flipped despite non-routing error", code)
		}
		srv.Close()
	}
}

// Messages-route models still fail loudly (Claude/Qwen follow-up).
func TestOpenCodeClientMessagesRouteFailsLoudly(t *testing.T) {
	c := &opencodeClient{provider: "opencode", baseURL: "https://opencode.ai/zen/v1"}
	_, err := c.StreamTurn(context.Background(), TurnRequest{Model: "claude-sonnet-4"})
	if err == nil {
		t.Fatal("messages-route model must fail loudly")
	}
	if !strings.Contains(err.Error(), "claude-sonnet-4") || !strings.Contains(err.Error(), "/messages") {
		t.Fatalf("error must name model + route: %v", err)
	}
}

// --- Session header + user-agent on the compat client (D1) -------------------

func TestOpenAICompatSessionHeaderAndUserAgent(t *testing.T) {
	var gotSession, gotUA, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("user-agent")
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `[DONE]`)))
	}))
	t.Cleanup(srv.Close)

	c := &OpenAICompatClient{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client(), ProviderID: "opencode"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m", SessionID: "exec-1"})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts)
	if gotSession != "exec-1" {
		t.Fatalf("x-opencode-session = %q, want exec-1", gotSession)
	}
	if !strings.HasPrefix(gotUA, "orchicon/") {
		t.Fatalf("user-agent = %q, want orchicon/…", gotUA)
	}
	if gotAuth != "Bearer k" {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

// --- Ollama auth (OLLAMA_API_KEY, Bearer on all transports) -----------------

func TestOllamaCloudBearerOnAllTransports(t *testing.T) {
	var gotAuth, gotUA string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		gotUA = r.Header.Get("user-agent")
		switch r.URL.Path {
		case "/api/chat":
			_, _ = w.Write([]byte(`{"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}` + "\n"))
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"model_info":{}}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &OllamaClient{Host: srv.URL, APIKey: "cloud-token", NumCtxDefault: 4096}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts)
	if gotPath != "/api/chat" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer cloud-token" {
		t.Fatalf("native /api/chat auth = %q, want Bearer cloud-token", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "orchicon/") {
		t.Fatalf("user-agent = %q", gotUA)
	}

	// Metadata transports carry the same Bearer.
	_, _ = c.tags(context.Background())
	if gotAuth != "Bearer cloud-token" {
		t.Fatalf("tags auth = %q", gotAuth)
	}
	_ = c.EffectiveContext(context.Background(), "m")
	if gotAuth != "Bearer cloud-token" {
		t.Fatalf("ps auth = %q", gotAuth)
	}
}

func TestOllamaLocalNoTokenPathUnchanged(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `[DONE]`)))
	}))
	t.Cleanup(srv.Close)

	// No APIKey → no Authorization header (local no-token path unchanged).
	c := &OllamaClient{Host: srv.URL}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts)
	if gotAuth != "" {
		t.Fatalf("local no-token path must send no Authorization, got %q", gotAuth)
	}
}

// --- Credential resolver: AuthOptional (Ollama) ------------------------------

func TestCredentialResolverAuthOptional(t *testing.T) {
	r := NewCredentialResolver(nil, nil)
	// AuthOptional with no stored/env credential resolves to "" (no error).
	p := Profile{ID: "ollama", AuthEnv: "OLLAMA_API_KEY", AuthOptional: true}
	v, err := r.Resolve(context.Background(), "t", p)
	if err != nil {
		t.Fatalf("AuthOptional missing credential must not error: %v", err)
	}
	if v != "" {
		t.Fatalf("resolved = %q, want empty", v)
	}
	// Non-optional still errors.
	p2 := Profile{ID: "openai", AuthEnv: "OPENAI_API_KEY"}
	if _, err := r.Resolve(context.Background(), "t", p2); err == nil {
		t.Fatal("non-optional missing credential must error")
	}
}
