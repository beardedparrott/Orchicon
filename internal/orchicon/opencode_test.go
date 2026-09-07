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

func TestOpenCodeClientResponsesRouteFailsLoudly(t *testing.T) {
	c := &opencodeClient{provider: "opencode", baseURL: "https://opencode.ai/zen/v1"}
	_, err := c.StreamTurn(context.Background(), TurnRequest{Model: "muse-spark-1.3-contributor-free"})
	if err == nil {
		t.Fatal("responses-route model must fail loudly")
	}
	if !strings.Contains(err.Error(), "muse-spark-1.3-contributor-free") || !strings.Contains(err.Error(), "/responses") {
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
