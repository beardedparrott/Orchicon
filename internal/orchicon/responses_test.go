package orchicon

import (
	"context"
	"strings"
	"testing"
)

// --- Responses wire decoder unit tests --------------------------------------

func TestResponsesStreamDeltaCompleted(t *testing.T) {
	body := sse(
		`{"type":"response.output_text.delta","delta":"Hello"}`,
		`{"type":"response.output_text.delta","delta":" world"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":34,"input_tokens_details":{"cached_tokens":8}}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k", ProviderID: "opencode"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %#v, want 2 TextDelta + Finish", evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hello" {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != " world" {
		t.Fatalf("event 1 = %#v", evs[1])
	}
	fin, ok := evs[2].(Finish)
	if !ok {
		t.Fatalf("event 2 = %#v, want Finish", evs[2])
	}
	if fin.StopReason != StopStop {
		t.Fatalf("stop = %q, want stop", fin.StopReason)
	}
	u := fin.Usage
	if u.InputTokens != 4 || u.OutputTokens != 34 || u.CacheReadTokens != 8 {
		t.Fatalf("usage = %#v, want in=4 (fresh: 12−8) out=34 cacheRead=8", u)
	}
}

func TestResponsesStreamReasoningDelta(t *testing.T) {
	body := sse(
		`{"type":"response.reasoning_text.delta","delta":"thinking..."}`,
		`{"type":"response.output_text.delta","delta":"answer"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if rd, ok := evs[0].(ReasoningDelta); !ok || rd.Text != "thinking..." {
		t.Fatalf("event 0 = %#v, want ReasoningDelta", evs[0])
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != "answer" {
		t.Fatalf("event 1 = %#v, want TextDelta", evs[1])
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	body := sse(
		`{"type":"response.output_item.added","item_id":"fc_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"SF\"}"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// Order: ToolCallStart → ToolCallDelta×2 → ToolCall → Finish.
	if sc, ok := evs[0].(ToolCallStart); !ok || sc.ToolCallID != "call_1" || sc.Name != "get_weather" {
		t.Fatalf("event 0 = %#v, want ToolCallStart call_1/get_weather", evs[0])
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
		t.Fatalf("event 4 = %#v, want Finish StopToolUse (2026-09-09 tool-swallow fix: a turn that carried tool calls must finish StopToolUse so the leader dispatches+persists+feeds back their results, not StopStop which counted them and dropped them)", evs[4])
	}
}

func TestResponsesStreamFailed(t *testing.T) {
	body := sse(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"boom"}}}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, err := drainStream(t, ts)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want provider error, got %v", err)
	}
	if _, ok := evs[len(evs)-1].(StreamError); !ok {
		t.Fatalf("last event = %#v, want StreamError", evs[len(evs)-1])
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	body := sse(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	_, err := drainStream(t, ts)
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("want incomplete error, got %v", err)
	}
}

// Finish is held until the body is fully drained: response.completed may
// precede the stream's end, and a trailing frame must not be lost.
func TestResponsesStreamHoldsFinishUntilDrained(t *testing.T) {
	body := sse(
		`{"type":"response.output_text.delta","delta":"x"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`{"type":"response.output_text.delta","delta":"y"}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, _ := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	// x, y, then Finish (held until drain).
	if len(evs) != 3 {
		t.Fatalf("events = %#v, want x, y, Finish", evs)
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != "y" {
		t.Fatalf("event 1 = %#v, want TextDelta y (after completed)", evs[1])
	}
	if _, ok := evs[2].(Finish); !ok {
		t.Fatalf("event 2 = %#v, want Finish", evs[2])
	}
}

func TestResponsesRequestShaping(t *testing.T) {	req := TurnRequest{
		Model: "m", MaxTokens: 99, Temperature: fltPtr(0.5),
		System: []SystemBlock{{Text: "be brief"}},
		Tools:  []ToolDef{{Name: "fn", ParamsJSON: `{"type":"object"}`}},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{{Text: strPtr("q")}}},
			{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "c1", Name: "fn", ArgsJSON: `{}`}}}},
			{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "c1", Content: "res"}}}},
		},
	}
	rr := buildResponsesRequest(req)
	if rr.Model != "m" || rr.MaxOutputTokens != 99 || !rr.Stream {
		t.Fatalf("request = %#v", rr)
	}
	if rr.Temperature == nil || *rr.Temperature != 0.5 {
		t.Fatalf("temperature = %v", rr.Temperature)
	}
	if len(rr.Tools) != 1 || rr.Tools[0].Name != "fn" {
		t.Fatalf("tools = %#v", rr.Tools)
	}
	// input: system, user, function_call, function_call_output — function
	// payloads are top-level items (the wire rejects them nested inside a
	// message content array).
	if len(rr.Input) != 4 {
		t.Fatalf("input = %#v, want 4 items", rr.Input)
	}
	if rr.Input[0].Role != "system" {
		t.Fatalf("input[0] = %#v", rr.Input[0])
	}
	if rr.Input[2].Type != "function_call" || rr.Input[2].CallID != "c1" || rr.Input[2].Name != "fn" {
		t.Fatalf("input[2] = %#v, want top-level function_call c1/fn", rr.Input[2])
	}
	if rr.Input[2].Role != "" {
		t.Fatalf("input[2] = %#v, function_call must not carry a role", rr.Input[2])
	}
	if rr.Input[3].Type != "function_call_output" || rr.Input[3].CallID != "c1" {
		t.Fatalf("input[3] = %#v, want top-level function_call_output c1", rr.Input[3])
	}
}

// Assistant text + tool use in one message serialize in order: the text
// as a message item, then the function_call item.
func TestResponsesRequestShapingMixedAssistant(t *testing.T) {
	req := TurnRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				{Text: strPtr("checking")},
				{ToolUse: &ContentToolUse{ToolCallID: "c9", Name: "fn", ArgsJSON: `{"a":1}`}},
			}},
		},
	}
	rr := buildResponsesRequest(req)
	if len(rr.Input) != 2 {
		t.Fatalf("input = %#v, want 2 items", rr.Input)
	}
	if rr.Input[0].Role != "assistant" || rr.Input[0].Content != "checking" {
		t.Fatalf("input[0] = %#v, want assistant text", rr.Input[0])
	}
	if rr.Input[1].Type != "function_call" || rr.Input[1].Arguments != `{"a":1}` {
		t.Fatalf("input[1] = %#v, want function_call with args", rr.Input[1])
	}
}

// Object-shaped deltas ({"delta":{"text":"Hi"}}) must decode like the
// compact string form — otherwise a gateway that emits the object shape
// fails every turn as "bad sse payload".
func TestResponsesStreamObjectDelta(t *testing.T) {
	body := sse(
		`{"type":"response.output_text.delta","delta":{"text":"Hello"}}`,
		`{"type":"response.output_text.delta","delta":{"content":" world"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %#v, want 2 TextDelta + Finish", evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Hello" {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != " world" {
		t.Fatalf("event 1 = %#v", evs[1])
	}
}

// Done-only text (no deltas, text on output_text.done) must still produce
// a reply — otherwise the Ask history commit is skipped (empty reply) and
// the follow-up looks like a first message.
func TestResponsesStreamDoneOnlyText(t *testing.T) {	body := sse(
		`{"type":"response.output_text.done","text":"full answer"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %#v, want TextDelta + Finish", evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "full answer" {
		t.Fatalf("event 0 = %#v, want TextDelta full answer", evs[0])
	}
}

// A replayed output_item.added frame for the same item must not produce a
// second ToolCall: two results for one call_id make the wire reject the
// turn as a duplicate function_call_output.
func TestResponsesStreamReplayedAddedIsDeduped(t *testing.T) {
	body := sse(
		`{"type":"response.output_item.added","item_id":"fc_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"fn"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\":1}"}`,
		`{"type":"response.output_item.added","item_id":"fc_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"fn"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var calls []ToolCall
	for _, e := range evs {
		if tc, ok := e.(ToolCall); ok {
			calls = append(calls, tc)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want exactly one", calls)
	}
	if calls[0].ArgsJSON != `{"a":1}` {
		t.Fatalf("args = %q, want streamed args intact", calls[0].ArgsJSON)
	}
}

// The done item carries the COMPLETE arguments: they replace streamed
// fragments (authoritative), never append — appending yields "{...}{...}",
// which fails JSON validation and silently blanks the call's args.
func TestResponsesStreamDoneArgsReplaceFragments(t *testing.T) {
	body := sse(
		`{"type":"response.output_item.added","item_id":"fc_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"fn"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"a\":"}`,
		`{"type":"response.output_item.done","item_id":"fc_1","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"fn","arguments":"{\"a\":1,\"b\":2}"}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var calls []ToolCall
	for _, e := range evs {
		if tc, ok := e.(ToolCall); ok {
			calls = append(calls, tc)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v, want exactly one", calls)
	}
	if calls[0].ArgsJSON != `{"a":1,"b":2}` {
		t.Fatalf("args = %q, want the done frame's complete args", calls[0].ArgsJSON)
	}
}

// The done frame echoes the COMPLETE output text after the deltas already
// streamed it. Feeding both duplicates the whole reply (and re-splits
// inline reasoning into text) — the done frame must be skipped when deltas
// were seen for the output.
func TestResponsesStreamDoneDoesNotDuplicateDeltas(t *testing.T) {
	body := sse(
		`{"type":"response.output_text.delta","output_index":0,"delta":"Got it"}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":" done."}`,
		`{"type":"response.output_text.done","output_index":0,"text":"Got it done."}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var text string
	for _, e := range evs {
		if td, ok := e.(TextDelta); ok {
			text += td.Text
		}
	}
	if text != "Got it done." {
		t.Fatalf("text = %q, want the deltas only (done must not duplicate)", text)
	}
}

// Inline reasoning deltas that the done frame echoes must not double the
// reasoning body: deltas win, done is skipped.
func TestResponsesStreamDoneDoesNotDuplicateReasoning(t *testing.T) {
	body := sse(
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"think"}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"ing"}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"thinking"}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"answer"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		`[DONE]`,
	)
	srv, _, _ := captureServer(t, 200, "text/event-stream", body)
	c := &ResponsesClient{BaseURL: srv.URL, APIKey: "k"}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var reason string
	for _, e := range evs {
		if rd, ok := e.(ReasoningDelta); ok {
			reason += rd.Text
		}
	}
	if reason != "thinking" {
		t.Fatalf("reasoning = %q, want the deltas only", reason)
	}
}
