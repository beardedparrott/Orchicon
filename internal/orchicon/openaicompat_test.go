package orchicon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- OpenAI Chat Completions wire --------------------------------------------

func TestOpenAIStreamTrailingUsageChunk(t *testing.T) {
	// finish_reason arrives on the last CONTENT chunk; real usage arrives on
	// a trailing usage-only chunk (choices: []). Finalizing on finish_reason
	// would zero costs — the fixture pins the drain invariant.
	body := sse(
		`{"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34,"prompt_tokens_details":{"cached_tokens":8}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &OpenAICompatClient{BaseURL: srv.URL, APIKey: "k", ProviderID: "openai"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %#v", evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hi" {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	fin, ok := evs[1].(Finish)
	if !ok {
		t.Fatalf("event 1 = %#v, want Finish", evs[1])
	}
	if fin.StopReason != StopStop {
		t.Fatalf("stop = %q", fin.StopReason)
	}
	u := fin.Usage
	if u.InputTokens != 12 || u.OutputTokens != 34 || u.CacheReadTokens != 8 {
		t.Fatalf("usage = %#v, want in=12 out=34 cacheRead=8 (cached_tokens)", u)
	}
}

func TestOpenAIStreamToolCallFragments(t *testing.T) {
	body := sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &OpenAICompatClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatal(err)
	}
	// Order: ToolCallStart → ToolCallDelta×2 → ToolCall → Finish.
	if _, ok := evs[0].(ToolCallStart); !ok {
		t.Fatalf("event 0 = %#v, want ToolCallStart", evs[0])
	}
	d0, _ := evs[1].(ToolCallDelta)
	d1, _ := evs[2].(ToolCallDelta)
	if d0.ArgsJSONDelta+d1.ArgsJSONDelta != `{"city":"SF"}` {
		t.Fatalf("deltas = %q + %q", d0.ArgsJSONDelta, d1.ArgsJSONDelta)
	}
	tc, ok := evs[3].(ToolCall)
	if !ok || tc.ToolCallID != "call_1" || tc.Name != "get_weather" || tc.ArgsJSON != `{"city":"SF"}` {
		t.Fatalf("event 3 = %#v", evs[3])
	}
	if fin, ok := evs[4].(Finish); !ok || fin.StopReason != StopToolUse {
		t.Fatalf("event 4 = %#v", evs[4])
	}
}

func TestOpenAIStreamErrorChunk(t *testing.T) {
	body := sse(`{"error":{"message":"boom","type":"server_error"}}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &OpenAICompatClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	_, err := drainStream(t, ts)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want provider error, got %v", err)
	}
}

func TestOpenAIStreamMalformedArgsFlushedAsEmptyObject(t *testing.T) {
	body := sse(
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"fn","arguments":"not-json"}}]}}]}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &OpenAICompatClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, _ := drainStream(t, ts)
	var tc *ToolCall
	for _, ev := range evs {
		if t2, ok := ev.(ToolCall); ok {
			tc = &t2
		}
	}
	if tc == nil || tc.ArgsJSON != "{}" {
		t.Fatalf("want sanitized tool call, got %#v", evs)
	}
}

func TestOpenAIRequestShaping(t *testing.T) {
	req := TurnRequest{
		Model: "m", MaxTokens: 99, ReasoningEffort: "high",
		System: []SystemBlock{{Text: "be brief"}},
		Tools:  []ToolDef{{Name: "fn", ParamsJSON: `{"type":"object"}`}},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: strPtr("q")}}},
			{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "c1", Name: "fn", ArgsJSON: `{}`}}}},
			{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c1", Content: "res"}}}},
		},
	}
	q := Quirks{StreamOptionsIncludeUsage: true, SupportsToolCalls: true, SupportsReasoningEffort: true, SupportsDeveloperRole: true}

	b, err := json.Marshal(buildOpenAIRequest(req, q))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["max_tokens"] != float64(99) {
		t.Fatalf("max_tokens = %v", m["max_tokens"])
	}
	if m["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", m["reasoning_effort"])
	}
	if _, ok := m["stream_options"]; !ok {
		t.Fatal("stream_options missing")
	}
	msgs := m["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "developer" {
		t.Fatal("developer role expected")
	}
	if _, ok := msgs[2].(map[string]any)["tool_calls"]; !ok {
		t.Fatal("assistant tool_calls missing")
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c1" || toolMsg["name"] != "fn" {
		t.Fatalf("tool message = %v", toolMsg)
	}

	// reasoning field quirk → "reasoning" instead of "reasoning_effort".
	q2 := Quirks{SupportsReasoningEffort: true, ReasoningField: "reasoning"}
	b2, _ := json.Marshal(buildOpenAIRequest(req, q2))
	var m2 map[string]any
	_ = json.Unmarshal(b2, &m2)
	if m2["reasoning"] != "high" || m2["reasoning_effort"] != nil {
		t.Fatalf("reasoning field = %v / %v", m2["reasoning"], m2["reasoning_effort"])
	}

	// No tool support → no tools parameter.
	q3 := Quirks{}
	b3, _ := json.Marshal(buildOpenAIRequest(req, q3))
	var m3 map[string]any
	_ = json.Unmarshal(b3, &m3)
	if _, ok := m3["tools"]; ok {
		t.Fatal("tools sent despite quirk")
	}

	// MergeSystemIntoUser → system text prepended to first user message.
	q4 := Quirks{MergeSystemIntoUser: true}
	b4, _ := json.Marshal(buildOpenAIRequest(req, q4))
	var m4 map[string]any
	_ = json.Unmarshal(b4, &m4)
	msgs4 := m4["messages"].([]any)
	first := msgs4[0].(map[string]any)
	if first["role"] != "user" || !strings.Contains(first["content"].(string), "be brief") {
		t.Fatalf("merged system = %v", first)
	}
}

func TestOpenAIImageContentParts(t *testing.T) {
	req := TurnRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: []Content{
		{Text: strPtr("look")},
		{Image: strPtr("data:image/png;base64,AAAA")},
	}}}}
	b, _ := json.Marshal(buildOpenAIRequest(req, Quirks{}))
	if !strings.Contains(string(b), "image_url") || !strings.Contains(string(b), "data:image/png;base64,AAAA") {
		t.Fatalf("image part missing: %s", b)
	}
}

func TestOpenAIAuthNone(t *testing.T) {
	srv, cap, _ := captureServer(t, 200, "text/event-stream", sse(`[DONE]`))
	c := &OpenAICompatClient{BaseURL: srv.URL, AuthStyle: "none"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	ts.Close()
	if cap.header.Get("Authorization") != "" {
		t.Fatal("auth none must not send authorization")
	}
}
