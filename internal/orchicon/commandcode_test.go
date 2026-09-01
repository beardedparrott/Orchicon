package orchicon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Command Code dual-transport ---------------------------------------------

// ccState records what the fake Command Code server was hit with.
type ccState struct {
	anthHits, oaHits, whoamiHits, legacyHits int
	lastLegacyBody                           string
	lastLegacyHeader                         http.Header
	lastAnthHeader                           http.Header
	legacyFailovers                          int // provider-route 403s served
	pinLegacyAfter403                        bool
}

// newCommandCodeTestServer handles every Command Code surface: whoami,
// legacy /alpha/generate, /provider/v1/messages, /provider/v1/chat/completions.
func newCommandCodeTestServer(t *testing.T) (*httptest.Server, *ccState) {
	t.Helper()
	st := &ccState{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/alpha/whoami"):
			st.whoamiHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"plan":"go","account":"acc"}`))
		case strings.HasSuffix(r.URL.Path, "/alpha/generate"):
			st.legacyHits++
			st.lastLegacyBody = string(body)
			st.lastLegacyHeader = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				sseEvent("text-delta", `{"type":"text-delta","delta":"legacy-hello"}`) +
					sseEvent("finish", `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":100,"outputTokens":20,"inputTokenDetails":{"cacheReadTokens":30,"cacheWriteTokens":10,"noCacheTokens":60}}}`)))
		case strings.HasSuffix(r.URL.Path, "/provider/v1/messages"):
			st.anthHits++
			st.lastAnthHeader = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				sseEvent("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`) +
					sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"anth-hello"}}`) +
					sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`) +
					sseEvent("message_stop", `{"type":"message_stop"}`)))
		case strings.HasSuffix(r.URL.Path, "/provider/v1/chat/completions"):
			st.oaHits++
			if st.pinLegacyAfter403 && st.oaHits == 1 {
				st.legacyFailovers++
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"error":{"code":"upgrade_required","message":"plan upgrade required"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				sse(`{"choices":[{"index":0,"delta":{"content":"oa-hello"}}]}`,
					`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`,
					`[DONE]`)))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func newCC(srv *httptest.Server, override string) *CommandCodeClient {
	return &CommandCodeClient{
		BaseURL: srv.URL, APIKey: "cc-key", PlanOverride: override,
		ThreadID: "thread-1",
		env:      func(string) string { return "" },
		Retry:    RetryPolicy{MaxAttempts: 1},
	}
}

func TestCommandCodeRoutingByModelID(t *testing.T) {
	srv, st := newCommandCodeTestServer(t)
	c := newCC(srv, "provider")

	// claude-* → Anthropic wire at /provider/v1/messages.
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "claude-sonnet-4", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := drainStream(t, ts)
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "anth-hello" {
		t.Fatalf("claude route event = %#v", evs[0])
	}
	if st.anthHits != 1 || st.oaHits != 0 {
		t.Fatalf("claude routing: anth=%d oa=%d", st.anthHits, st.oaHits)
	}
	if got := st.lastAnthHeader.Get("Authorization"); got != "Bearer cc-key" {
		t.Fatalf("provider route auth = %q", got)
	}
	if st.lastAnthHeader.Get("X-Api-Key") != "" {
		t.Fatal("anthropic provider route must not send x-api-key")
	}

	// Slashed non-claude id → OpenAI wire at /provider/v1/chat/completions.
	ts2, err := c.StreamTurn(context.Background(), TurnRequest{Model: "deepseek/deepseek-v4-flash", MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	evs2, _ := drainStream(t, ts2)
	if td, ok := evs2[0].(TextDelta); !ok || td.Text != "oa-hello" {
		t.Fatalf("openai route event = %#v", evs2[0])
	}
	if st.oaHits != 1 {
		t.Fatalf("openai hits = %d", st.oaHits)
	}
}

func TestCommandCodeTrailingUsage(t *testing.T) {
	srv, _ := newCommandCodeTestServer(t)
	c := newCC(srv, "provider")
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "z-ai/glm-5.3-flash", MaxTokens: 10})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatal(err)
	}
	fin := evs[len(evs)-1].(Finish)
	u := fin.Usage
	if u.InputTokens != 9 || u.OutputTokens != 4 || u.CacheReadTokens != 2 {
		t.Fatalf("usage = %#v — trailing usage-only chunk must not be lost", u)
	}
}

func TestCommandCodePlanResolution(t *testing.T) {
	srv, st := newCommandCodeTestServer(t)

	// Explicit override wins over env, no whoami call.
	c := newCC(srv, "provider")
	c.env = func(k string) string {
		if k == envCommandCodePlan {
			return "go"
		}
		return ""
	}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
	drainStream(t, ts)
	if st.legacyHits != 0 || st.oaHits != 1 || st.whoamiHits != 0 {
		t.Fatalf("override must win: legacy=%d oa=%d whoami=%d", st.legacyHits, st.oaHits, st.whoamiHits)
	}

	// Env-only (COMMANDCODE_PLAN=go) → legacy, no whoami.
	c2 := newCC(srv, "")
	c2.env = func(k string) string {
		if k == envCommandCodePlan {
			return "go"
		}
		return ""
	}
	ts2, _ := c2.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
	drainStream(t, ts2)
	if st.legacyHits != 1 {
		t.Fatalf("env go must route legacy: %d", st.legacyHits)
	}
	if st.whoamiHits != 0 {
		t.Fatalf("env plan must short-circuit whoami: %d", st.whoamiHits)
	}

	// No override/env → whoami (says go), cached. Two turns → one whoami.
	srv2, st2 := newCommandCodeTestServer(t)
	c3 := newCC(srv2, "")
	for i := 0; i < 2; i++ {
		ts3, err := c3.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
		if err != nil {
			t.Fatal(err)
		}
		drainStream(t, ts3)
	}
	if st2.whoamiHits != 1 {
		t.Fatalf("whoami must be cached per instance: hits=%d", st2.whoamiHits)
	}
	if st2.legacyHits != 2 || st2.oaHits != 0 {
		t.Fatalf("plan=go from whoami must route legacy: legacy=%d oa=%d", st2.legacyHits, st2.oaHits)
	}
}

func TestCommandCode403FlipAndStickiness(t *testing.T) {
	srv, st := newCommandCodeTestServer(t)
	st.pinLegacyAfter403 = true
	c := newCC(srv, "provider")

	// First turn: provider route 403 upgrade_required → pin legacy, retry once.
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "z-ai/glm-5.3-flash", MaxTokens: 5})
	if err != nil {
		t.Fatalf("403 flip must recover: %v", err)
	}
	evs, _ := drainStream(t, ts)
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "legacy-hello" {
		t.Fatalf("flip must serve legacy stream, got %#v", evs)
	}
	if st.legacyFailovers != 1 || st.legacyHits != 1 {
		t.Fatalf("flip counters: 403s=%d legacy=%d", st.legacyFailovers, st.legacyHits)
	}

	// Second turn goes straight to legacy (sticky pin) — no further 403.
	ts2, err := c.StreamTurn(context.Background(), TurnRequest{Model: "z-ai/glm-5.3-flash", MaxTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, ts2)
	if st.legacyFailovers != 1 || st.legacyHits != 2 {
		t.Fatalf("pin must be sticky: 403s=%d legacy=%d", st.legacyFailovers, st.legacyHits)
	}
}

func TestCommandCodeLegacyEnvelope(t *testing.T) {
	srv, st := newCommandCodeTestServer(t)
	c := newCC(srv, "go") // override → legacy directly
	req := TurnRequest{
		Model: "z-ai/glm-5.3-flash", MaxTokens: 77, ReasoningEffort: "medium",
		System: []SystemBlock{{Text: "sys1"}, {Text: "sys2"}},
		Tools:  []ToolDef{{Name: "fn", Description: "d", ParamsJSON: `{"type":"object","properties":{}}`}},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: strPtr("q")}}},
			{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "c9", Name: "fn", ArgsJSON: `{"a":2}`}}}},
			{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c9", Content: "out"}}}},
		},
	}
	ts, err := c.StreamTurn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := drainStream(t, ts)

	// Headers.
	h := st.lastLegacyHeader
	if got := h.Get("Authorization"); got != "Bearer cc-key" {
		t.Fatalf("legacy auth = %q", got)
	}
	for _, k := range []string{"X-Command-Code-Version", "X-Cli-Environment", "X-Project-Slug", "X-Taste-Learning", "X-Co-Flag"} {
		if h.Get(k) == "" {
			t.Fatalf("legacy header %s missing", k)
		}
	}

	// Envelope shape.
	var env struct {
		Config struct {
			WorkingDir  string `json:"workingDir"`
			Date        string `json:"date"`
			Environment string `json:"environment"`
		} `json:"config"`
		Params struct {
			Model           string `json:"model"`
			System          string `json:"system"`
			MaxTokens       int64  `json:"max_tokens"`
			Stream          bool   `json:"stream"`
			ReasoningEffort string `json:"reasoning_effort"`
			Tools           []struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Type       string `json:"type"`
					ToolCallID string `json:"toolCallId"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"params"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal([]byte(st.lastLegacyBody), &env); err != nil {
		t.Fatalf("envelope: %v — body=%s", err, st.lastLegacyBody)
	}
	if env.Config.Environment == "" || env.Config.WorkingDir == "" || env.Config.Date == "" {
		t.Fatalf("config stanza incomplete: %+v", env.Config)
	}
	p := env.Params
	if p.Model != "z-ai/glm-5.3-flash" || !p.Stream || p.MaxTokens != 77 || p.ReasoningEffort != "medium" {
		t.Fatalf("params = %+v", p)
	}
	if p.System != "sys1\n\nsys2" {
		t.Fatalf("system = %q", p.System)
	}
	if len(p.Tools) != 1 || p.Tools[0].Function.Name != "fn" {
		t.Fatalf("tools = %+v", p.Tools)
	}
	if env.ThreadID != "thread-1" {
		t.Fatalf("threadId = %q", env.ThreadID)
	}
	msgs := p.Messages
	if len(msgs) != 3 || msgs[0].Role != "user" || msgs[2].Role != "tool" {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(msgs[2].Content) != 1 || msgs[2].Content[0].ToolCallID != "c9" {
		t.Fatalf("tool-result part = %+v", msgs[2].Content)
	}

	// Stream: legacy text + cache-inclusive totalUsage → noCache input.
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "legacy-hello" {
		t.Fatalf("legacy text = %#v", evs[0])
	}
	fin := evs[len(evs)-1].(Finish)
	u := fin.Usage
	// total=100, cacheRead=30, cacheWrite=10 → noCache input = 60.
	if u.InputTokens != 60 || u.CacheReadTokens != 30 || u.CacheWriteTokens != 10 || u.OutputTokens != 20 {
		t.Fatalf("usage = %#v, want input(noCache)=60 cacheRead=30 cacheWrite=10 out=20", u)
	}
	if u.TotalTokens() != 80 {
		t.Fatalf("total = %d, want 80 (noCache input + output)", u.TotalTokens())
	}
}

func TestCommandCodeZDRHeader(t *testing.T) {
	// CMD_ZDR=1 → x-cmd-zdr: 1 on the legacy route.
	srv, st := newCommandCodeTestServer(t)
	c := newCC(srv, "go")
	c.env = func(k string) string {
		if k == envCommandCodeZDR {
			return "1"
		}
		return ""
	}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
	drainStream(t, ts)
	if got := st.lastLegacyHeader.Get("X-Cmd-Zdr"); got != "1" {
		t.Fatalf("CMD_ZDR=1 must send x-cmd-zdr:1, got %q", got)
	}

	// Without CMD_ZDR the header is never sent.
	srv2, st2 := newCommandCodeTestServer(t)
	c2 := newCC(srv2, "go")
	ts2, _ := c2.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
	drainStream(t, ts2)
	if got := st2.lastLegacyHeader.Get("X-Cmd-Zdr"); got != "" {
		t.Fatalf("x-cmd-zdr must be absent without CMD_ZDR=1, got %q", got)
	}
}

func TestCommandCodeZDROnProviderRoute(t *testing.T) {
	srv, st := newCommandCodeTestServer(t)
	c := newCC(srv, "provider")
	c.env = func(k string) string {
		if k == envCommandCodeZDR {
			return "1"
		}
		return ""
	}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "claude-sonnet-4", MaxTokens: 5})
	drainStream(t, ts)
	if got := st.lastAnthHeader.Get("X-Cmd-Zdr"); got != "1" {
		t.Fatalf("provider anthropic route x-cmd-zdr = %q", got)
	}
}

func TestIsClaudeModel(t *testing.T) {
	for _, m := range []string{"claude-sonnet-4", "claude-opus-4"} {
		if !isClaudeModel(m) {
			t.Fatalf("%s must route to anthropic wire", m)
		}
	}
	for _, m := range []string{"deepseek/deepseek-v4-flash", "z-ai/glm-5.3-flash", "gpt-5", "claudex"} {
		if isClaudeModel(m) {
			t.Fatalf("%s must route to openai wire", m)
		}
	}
}
