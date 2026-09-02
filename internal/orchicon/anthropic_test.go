package orchicon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- shared test helpers -----------------------------------------------------

// sse renders data-only SSE frames (OpenAI style).
func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return b.String()
}

// sseEvent renders an event-tagged SSE frame (Anthropic / legacy style).
func sseEvent(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// drainStream collects every event until the stream ends. A mid-stream
// StreamError event is captured alongside its terminal error.
func drainStream(t *testing.T, ts TurnStream) ([]Event, error) {
	t.Helper()
	defer ts.Close()
	var evs []Event
	for {
		ev, ok, err := ts.Next(context.Background())
		if ev != nil {
			evs = append(evs, ev)
		}
		if err != nil {
			return evs, err
		}
		if !ok {
			return evs, nil
		}
	}
}

func strPtr(s string) *string { return &s }

func fltPtr(f float64) *float64 { return &f }

func boolPtr(b bool) *bool { return &b }

// testCtx is the context for live smokes (bounded).
func testCtx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = cancel // smokes are single tiny calls; cancellation owned by drainStream Close
	return ctx
}

// captureServer records the last request (path + headers + body) and
// replies with respBody.
func captureServer(t *testing.T, status int, contentType, respBody string) (*httptest.Server, *capturedRequest, *int) {
	t.Helper()
	cap := &capturedRequest{}
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		cap.path = r.URL.Path
		cap.header = r.Header.Clone()
		cap.body = string(b)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, cap, &hits
}

type capturedRequest struct {
	path   string
	header http.Header
	body   string
}

// --- Anthropic Messages wire -------------------------------------------------

func TestAnthropicStreamHappyPath(t *testing.T) {
	body := sseEvent("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)

	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "claude-sonnet-4", MaxTokens: 100})
	if err != nil {
		t.Fatalf("StreamTurn: %v", err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %#v", len(evs), evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hello" {
		t.Fatalf("event 0 = %#v, want TextDelta Hello", evs[0])
	}
	fin, ok := evs[1].(Finish)
	if !ok {
		t.Fatalf("event 1 = %#v, want Finish", evs[1])
	}
	if fin.StopReason != StopStop {
		t.Fatalf("stop reason = %q, want stop", fin.StopReason)
	}
	u := fin.Usage
	if u.InputTokens != 10 || u.OutputTokens != 7 || u.CacheReadTokens != 5 || u.CacheWriteTokens != 3 {
		t.Fatalf("usage = %#v, want in=10 out=7 cacheRead=5 cacheWrite=3", u)
	}
	// Next after Finish terminates cleanly.
	if _, ok, _ := ts.Next(context.Background()); ok {
		t.Fatal("stream not terminated after Finish")
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	body := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)

	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "claude-sonnet-4", MaxTokens: 100})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if _, ok := evs[0].(ToolCallStart); !ok {
		t.Fatalf("event 0 = %#v, want ToolCallStart", evs[0])
	}
	if _, ok := evs[1].(ToolCallDelta); !ok {
		t.Fatalf("event 1 = %#v, want ToolCallDelta", evs[1])
	}
	tc, ok := evs[3].(ToolCall)
	if !ok {
		t.Fatalf("event 3 = %#v, want ToolCall", evs[3])
	}
	if tc.ToolCallID != "toolu_1" || tc.Name != "get_weather" || tc.ArgsJSON != `{"city":"SF"}` {
		t.Fatalf("tool call = %#v", tc)
	}
	if fin := evs[4].(Finish); fin.StopReason != StopToolUse {
		t.Fatalf("stop = %q, want tool_use", fin.StopReason)
	}
}

func TestAnthropicToolInputInlinedAtBlockStart(t *testing.T) {
	// Some gateways inline the full tool input at content_block_start.
	body := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"fn","input":{"a":1}}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 10})
	evs, _ := drainStream(t, ts)
	// Start → ToolCall (inlined input) → Finish.
	tc, ok := evs[1].(ToolCall)
	if !ok || tc.ArgsJSON != `{"a":1}` {
		t.Fatalf("tool call = %#v, want args {\"a\":1}", evs[1])
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	body := sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 10})
	evs, err := drainStream(t, ts)
	if err == nil {
		t.Fatal("want error from error event")
	}
	if _, ok := evs[len(evs)-1].(StreamError); !ok {
		t.Fatalf("last event = %#v, want StreamError", evs[len(evs)-1])
	}
}

func TestAnthropicMidStreamDisconnectFails(t *testing.T) {
	// Stream ends without message_stop → clean failure (D11).
	body := sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 10})
	_, err := drainStream(t, ts)
	if err == nil || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("want clean mid-stream failure, got %v", err)
	}
}

func TestAnthropicPreStreamRetry(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseEvent("message_stop", `{"type":"message_stop"}`)))
	}))
	t.Cleanup(srv.Close)
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k", Retry: RetryPolicy{MaxAttempts: 2, Sleep: func(time.Duration) {}}}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 10})
	if err != nil {
		t.Fatalf("retry should recover 500: %v", err)
	}
	ts.Close()
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

func TestAnthropicCacheBreakpoints(t *testing.T) {
	srv, cap, _ := captureServer(t, 200, "text/event-stream", sseEvent("message_stop", `{"type":"message_stop"}`))
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "k"}
	req := TurnRequest{
		Model: "claude-sonnet-4", MaxTokens: 10,
		System: []SystemBlock{{Text: "s1"}, {Text: "s2", Cache: true}},
		Tools:  []ToolDef{{Name: "t1", ParamsJSON: `{"type":"object"}`}, {Name: "t2", ParamsJSON: `{"type":"object"}`}},
	}
	ts, err := c.StreamTurn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	ts.Close()

	var parsed struct {
		System []struct {
			Text         string         `json:"text"`
			CacheControl *anthCacheCtrl `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			Name         string         `json:"name"`
			CacheControl *anthCacheCtrl `json:"cache_control"`
		} `json:"tools"`
		MaxTokens int64 `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(cap.body), &parsed); err != nil {
		t.Fatal(err)
	}
	// Default policy system+tools: explicit Cache block + LAST system block
	// and LAST tool carry breakpoints.
	if parsed.System[0].CacheControl != nil {
		t.Fatal("first system block must not carry breakpoint")
	}
	if parsed.System[1].CacheControl == nil {
		t.Fatal("Cache-flagged system block must carry breakpoint")
	}
	if parsed.Tools[0].CacheControl != nil || parsed.Tools[1].CacheControl == nil {
		t.Fatal("only the last tool definition carries the breakpoint")
	}
	if parsed.MaxTokens != 10 {
		t.Fatalf("max_tokens = %d", parsed.MaxTokens)
	}

	// CacheControlNone → no breakpoints anywhere.
	req.CacheControl = CacheControlNone
	srv2, cap2, _ := captureServer(t, 200, "text/event-stream", sseEvent("message_stop", `{"type":"message_stop"}`))
	c2 := &AnthropicClient{BaseURL: srv2.URL, APIKey: "k"}
	ts2, _ := c2.StreamTurn(context.Background(), req)
	ts2.Close()
	if strings.Contains(cap2.body, "cache_control") {
		t.Fatal("none policy must not emit cache_control")
	}
}

func TestAnthropicAuthAndHeaders(t *testing.T) {
	srv, cap, _ := captureServer(t, 200, "text/event-stream", sseEvent("message_stop", `{"type":"message_stop"}`))
	c := &AnthropicClient{BaseURL: srv.URL, APIKey: "secret", AuthStyle: "bearer", ExtraHeaders: map[string]string{"x-cmd-zdr": "1"}}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 5})
	ts.Close()
	if got := cap.header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("authorization = %q", got)
	}
	if cap.header.Get("X-Api-Key") != "" {
		t.Fatal("bearer style must not send x-api-key")
	}
	if got := cap.header.Get("Anthropic-Version"); got != anthropicVersion {
		t.Fatalf("anthropic-version = %q", got)
	}
	if got := cap.header.Get("X-Cmd-Zdr"); got != "1" {
		t.Fatalf("x-cmd-zdr = %q", got)
	}
}

func TestAnthropicHistoryMarshal(t *testing.T) {
	hist := []Message{
		{Role: RoleUser, Content: []Content{{Text: strPtr("hi")}}},
		{Role: RoleAssistant, Content: []Content{
			{ToolUse: &ContentToolUse{ToolCallID: "tu1", Name: "fn", ArgsJSON: `{"x":1}`}},
		}},
		{Role: RoleTool, Content: []Content{
			{ToolResult: &ContentToolResult{ToolCallID: "tu1", Content: "result", IsError: true}},
		}},
	}
	req := TurnRequest{Model: "m", Messages: hist}
	wire := buildAnthropicRequest(req, "")
	b, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	ms := parsed.Messages
	// Anthropic has no tool role — tool_result rides a USER message.
	if len(ms) != 3 || ms[0].Role != "user" || ms[1].Role != "assistant" || ms[2].Role != "user" {
		t.Fatalf("messages = %#v", ms)
	}
	if ms[1].Content[0].Type != "tool_use" || ms[1].Content[0].ID != "tu1" {
		t.Fatalf("assistant tool_use = %#v", ms[1].Content[0])
	}
	// tool_result rides a USER message with is_error + bare-string content.
	if ms[2].Content[0].Type != "tool_result" || !ms[2].Content[0].IsError || string(ms[2].Content[0].Content) != `"result"` {
		t.Fatalf("tool_result = %#v", ms[2].Content[0])
	}

	// Consecutive tool messages merge into ONE user message.
	merged := buildAnthropicRequest(TurnRequest{Model: "m", Messages: []Message{
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "a", Content: "x"}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "b", Content: "y"}}}},
	}}, "")
	mb, _ := json.Marshal(merged)
	var mparsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(mb, &mparsed)
	if len(mparsed.Messages) != 1 || mparsed.Messages[0].Role != "user" || len(mparsed.Messages[0].Content) != 2 {
		t.Fatalf("tool merge = %#v", mparsed.Messages)
	}
}

func TestIsUpgradeRequired403(t *testing.T) {
	cases := []struct {
		code int
		body string
		want bool
	}{
		{403, "", true},
		{403, `{"error":{"code":"upgrade_required"}}`, true},
		{403, `some Upgrade_Required text`, true},
		{403, `{"error":"forbidden"}`, false},
		{401, "", false},
		{500, "", false},
	}
	for _, c := range cases {
		if got := isUpgradeRequired403(c.code, []byte(c.body)); got != c.want {
			t.Fatalf("isUpgradeRequired403(%d, %q) = %v", c.code, c.body, got)
		}
	}
}

// The Anthropic wire must stay strictly role-alternating for ANY normalized
// history, including the realistic shapes compaction (middle eviction) can
// produce: a plain-text user turn whose surrounding tool rounds were evicted
// lands directly after the goal user message, and an assistant text+tool_use
// turn trimmed to bare text can sit next to another assistant text. The
// marshaler coalesces consecutive same-role turns (content appended in order —
// never dropped). This is the QA-iteration-2 regression for the compaction
// role-alternation contract (bug C's generalization).
func TestAnthropicHistoryMarshalRoleAlternationCoalesces(t *testing.T) {
	hist := []Message{
		{Role: RoleUser, Content: []Content{{Text: strPtr("Goal")}}},
		{Role: RoleUser, Content: []Content{{Text: strPtr("Interrupt after goal")}}}, // coalesce
		{Role: RoleAssistant, Content: []Content{{Text: strPtr("alpha")}}},
		{Role: RoleAssistant, Content: []Content{{Text: strPtr("beta")}}}, // coalesce
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c1", Content: "out"}}}},
		{Role: RoleUser, Content: []Content{{Text: strPtr("After tool result")}}}, // coalesce AFTER tool_result
	}
	wire := marshalAnthropicHistory(hist)
	for i := 1; i < len(wire); i++ {
		if wire[i].Role == wire[i-1].Role {
			t.Fatalf("non-alternating roles at %d-%d (%q %q): %#v", i-1, i, wire[i-1].Role, wire[i].Role, wire)
		}
	}
	// [user(Goal+Interrupt)] [assistant(alpha+beta)] [user(tool_result+After tool result)]
	if len(wire) != 3 {
		t.Fatalf("wire = %#v, want 3 coalesced turns", wire)
	}
	if got := wire[0].Content[0].Text; got != "Goal" && got != "Interrupt after goal" {
		t.Fatalf("first user text = %q", got)
	}
	// Text after tool_result: wire[2] holds the tool_result block FIRST then text.
	last := wire[2].Content
	foundText := false
	for _, c := range last {
		if c.Type == "text" {
			foundText = true
			if c.Text != "After tool result" {
				t.Fatalf("tail text = %q", c.Text)
			}
			break
		}
		if c.Type != "tool_result" {
			t.Fatalf("content before tool_result in tail user: %#v", c)
		}
	}
	if !foundText {
		t.Fatalf("'After tool result' text missing from tail user: %#v", last)
	}
}
