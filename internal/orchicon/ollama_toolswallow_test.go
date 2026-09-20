package orchicon

// Regression tests for the 2026-09-10 ollama native tool-swallow incident
// (work item 01M21PZE3QQEZ35418SXCQZZRQ root cause): ollama reports a
// turn's tool calls in message.tool_calls and ends the turn with
// done:true, done_reason:"stop". The old decoder returned the FIRST tool
// call of the tool-calls loop before the Done branch was ever reached, so
// (a) batched tool calls were dropped, and (b) on a
// done-carrying-tool-calls chunk the Finish (stop reason + usage) never
// surfaced — the next Next() failed with "stream ended without done
// chunk". Separately, a loop-level guard now promotes StopStop turns
// carrying pending tool calls to StopToolUse (the 2026-09-09
// responses.go fix, made transport-independent), so counted-but-unexecuted
// calls can never spin the count-and-drop budget_abort loop again.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ndjsonLines joins raw NDJSON chunk lines.
func ndjsonLines(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// newNativeTestClient points an OllamaClient (num_ctx forced → native
// /api/chat route) at an httptest server serving the given NDJSON body.
func newNativeTestClient(t *testing.T, body []byte) (*OllamaClient, TurnStream) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	ts, err := c.StreamTurn(context.Background(), TurnRequest{Model: "m", MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	return c, ts
}

// wantToolCall asserts ev is the complete ToolCall with the given name.
func wantToolCall(t *testing.T, ev Event, name string) ToolCall {
	t.Helper()
	tc, ok := ev.(ToolCall)
	if !ok {
		t.Fatalf("want ToolCall, got %#v", ev)
	}
	if tc.Name != name {
		t.Fatalf("tool call name = %q, want %q (event %#v)", tc.Name, name, tc)
	}
	return tc
}

// AC (decoder-level, 1/3): a tool-call chunk followed by a separate done
// chunk must surface the ToolCall AND the Finish — today this already
// worked when the chunks are separate; the test pins the invariant.
func TestOllamaNativeToolCallChunkThenDone(t *testing.T) {
	_, ts := newNativeTestClient(t, ndjsonLines(
		`{"message":{"content":"Resuming, reading the file…"}}`,
		`{"message":{"tool_calls":[{"function":{"name":"read","arguments":{"path":"recovery.md"}}}]}}`,
		`{"done":true,"done_reason":"stop","prompt_eval_count":100,"eval_count":50}`,
	))
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("native stream: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3 (Text, ToolCall, Finish): %#v", len(evs), evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "Resuming, reading the file…" {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	tc := wantToolCall(t, evs[1], "read")
	if tc.ArgsJSON != `{"path":"recovery.md"}` {
		t.Fatalf("tool args = %s", tc.ArgsJSON)
	}
	fin, ok := evs[2].(Finish)
	if !ok || fin.StopReason != StopStop {
		t.Fatalf("event 2 = %#v, want Finish(stop)", evs[2])
	}
	if fin.Usage.InputTokens != 100 || fin.Usage.OutputTokens != 50 {
		t.Fatalf("finish usage = %#v", fin.Usage)
	}
}

// AC (decoder-level, 2/3): the final NDJSON chunk carries BOTH
// message.tool_calls AND done:true — the tool calls AND the Finish (stop
// reason + usage) must ALL surface, with no "stream ended without done
// chunk" false terminal. This was the incident's primary wire shape.
func TestOllamaNativeDoneChunkCarriesToolCalls(t *testing.T) {
	_, ts := newNativeTestClient(t, ndjsonLines(
		`{"message":{"content":"I'm resuming a recovered session."}}`,
		`{"message":{"content":"Let me read the recovery file…","tool_calls":[{"function":{"name":"read","arguments":{"path":".orchicon/recovery"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":7,"eval_count":13}`,
	))
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("native stream: %v (old bug: done-with-tool-calls chunk skipped the Done branch)", err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4 (Text, tail Text, ToolCall, Finish): %#v", len(evs), evs)
	}
	if td, ok := evs[0].(TextDelta); !ok || td.Text != "I'm resuming a recovered session." {
		t.Fatalf("event 0 = %#v", evs[0])
	}
	if td, ok := evs[1].(TextDelta); !ok || td.Text != "Let me read the recovery file…" {
		t.Fatalf("event 1 = %#v, want the done chunk's tail content BEFORE the tool call", evs[1])
	}
	wantToolCall(t, evs[2], "read")
	fin, ok := evs[3].(Finish)
	if !ok {
		t.Fatalf("event 3 = %#v, want Finish", evs[3])
	}
	if fin.StopReason != StopStop {
		t.Fatalf("finish stop reason = %q, want stop", fin.StopReason)
	}
	if fin.Usage.InputTokens != 7 || fin.Usage.OutputTokens != 13 {
		t.Fatalf("finish usage = %#v, want {7 13}", fin.Usage)
	}
}

// AC (decoder-level, 3/3): ollama batches MULTIPLE tool calls into one
// chunk — every one must surface, in wire order, with unique indices and
// ids (the old first-only early return silently lost all but the first).
func TestOllamaNativeMultiToolCallsInOneChunk(t *testing.T) {
	_, ts := newNativeTestClient(t, ndjsonLines(
		`{"message":{"tool_calls":[`+
			`{"function":{"name":"read","arguments":{"path":"a.go"}}},`+
			`{"function":{"name":"grep","arguments":{"pattern":"TODO"}}},`+
			`{"function":{"name":"bash","arguments":{"command":"ls"}}}`+
			`]},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":9}`,
	))
	evs, err := drainStream(t, ts)
	if err != nil {
		t.Fatalf("native stream: %v", err)
	}
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4 (3 ToolCalls + Finish): %#v", len(evs), evs)
	}
	wantNames := []string{"read", "grep", "bash"}
	for i, name := range wantNames {
		tc := wantToolCall(t, evs[i], name)
		if tc.Index != i {
			t.Fatalf("tool call %d index = %d, want %d", i, tc.Index, i)
		}
	}
	fin, ok := evs[3].(Finish)
	if !ok || fin.StopReason != StopStop || fin.Usage.InputTokens != 3 || fin.Usage.OutputTokens != 9 {
		t.Fatalf("event 3 = %#v, want Finish(stop,{3,9})", evs[3])
	}
	// Post-finish Next: the stream reports clean EOF.
	if ev, ok, err := ts.Next(context.Background()); ok || ev != nil || err != nil {
		t.Fatalf("post-finish Next = (%#v, %v, %v), want clean end", ev, ok, err)
	}
}

// AC (loop-level): a StopStop turn with pending tool calls executes the
// tools, feeds the results back into history, and loops — the calls are
// never silently dropped. The guard is transport-independent (mirrors the
// 2026-09-09 responses.go finalize fix at the loop level).
func TestLoopStopStopWithPendingToolCallsExecutesTools(t *testing.T) {
	tools := newMockTools()
	tools.results["read"] = "file contents"
	prov := &mockProvider{turns: []scriptedTurn{
		// The transport bug shape: text + a complete tool call event, but
		// the Finish claims StopStop (a decoder that did not promote).
		{events: []Event{TextDelta{Text: "Reading the recovery file."}, ToolCall{Index: 0, ToolCallID: "call_1", Name: "read", ArgsJSON: `{"path":"recovery.md"}`}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 10, OutputTokens: 5}},
		// The follow-up turn: marker delivered → the session settles.
		{events: []Event{TextDelta{Text: markerText("done reading")}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 12, OutputTokens: 6}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, toolCalls, _, _, _, results := cb.snapshot()
	if len(toolCalls) != 1 || !strings.Contains(toolCalls[0], `read({"path":"recovery.md"})`) {
		t.Fatalf("OnToolCall = %v, want exactly the read call executed", toolCalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success", results)
	}
	// The provider saw the tool round: turn 2's history carries the
	// assistant tool_use message (its Text block precedes the ToolUse
	// block — appendAssistantToolUse appends text first) and the tool
	// result (results fed back).
	if prov.requestCount() != 2 {
		t.Fatalf("provider turns = %d, want 2", prov.requestCount())
	}
	req := prov.lastRequest()
	if len(req.Messages) < 3 {
		t.Fatalf("turn-2 history has %d messages, want >= 3 (goal, assistant tool_use, tool result)", len(req.Messages))
	}
	asst := req.Messages[len(req.Messages)-2]
	hasUse := false
	for _, c := range asst.Content {
		if c.ToolUse != nil {
			hasUse = true
		}
	}
	if asst.Role != RoleAssistant || len(asst.Content) < 2 || asst.Content[0].Text == nil || !hasUse {
		t.Fatalf("message before tool result is role=%q, want assistant text+tool_use (got %+v)", asst.Role, req.Messages)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != RoleTool || len(last.Content) == 0 || last.Content[0].ToolResult == nil ||
		!strings.Contains(last.Content[0].ToolResult.Content, "file contents") {
		t.Fatalf("tool result message = %+v, want the executed tool's result in history", last)
	}
}

// AC (failure-signature regression): a session whose EVERY turn announces
// text + emits one tool call + ends StopStop must make REAL progress —
// the calls execute — instead of re-announcing until
// budget_abort:tool_call_count. This is the incident's transcript shape.
func TestLoopAnnounceAndToolCallStopStopMakesProgress(t *testing.T) {
	tools := newMockTools()
	prov := &mockProvider{turns: []scriptedTurn{
		{events: []Event{TextDelta{Text: "I'm resuming a recovered session, let me read the recovery file…"}, ToolCall{Index: 0, ToolCallID: "c1", Name: "read", ArgsJSON: `{"path":"recovery.md"}`}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: "I'm resuming a recovered session, let me read the recovery file…"}, ToolCall{Index: 0, ToolCallID: "c2", Name: "read", ArgsJSON: `{"path":"recovery.md"}`}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 100, OutputTokens: 10}},
		{events: []Event{TextDelta{Text: markerText("work completed")}}, finish: StopStop, bare: true, usage: Usage{InputTokens: 50, OutputTokens: 20}},
	}}
	s := qaSession(t, prov, tools)
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, toolCalls, _, _, _, results := cb.snapshot()
	if len(toolCalls) != 2 {
		t.Fatalf("OnToolCall = %v, want BOTH announce-then-call turns executed (progress, not count-and-drop)", toolCalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success (never budget_abort:tool_call_count)", results)
	}
	if prov.requestCount() != 3 {
		t.Fatalf("provider turns = %d, want 3 (each StopStop-with-calls turn advanced the loop)", prov.requestCount())
	}
}

// AC (end-to-end, ollama native path): a model that announces + issues one
// tool call per turn makes real progress through the FULL native decoder
// + loop stack — httptest serves real ollama NDJSON; the loop executes
// the tool, feeds the result back, and the session completes.
func TestOllamaNativeEndToEndAnnounceToolCallPerTurnProgresses(t *testing.T) {
	var calls int64
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.NotFound(w, r)
			return
		}
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		bodies = append(bodies, string(b))
		calls++
		w.Header().Set("Content-Type", "application/x-ndjson")
		switch calls {
		case 1: // incident turn shape: announce + tool call, done on the SAME chunk
			_, _ = w.Write(ndjsonLines(
				`{"message":{"content":"I'm resuming a recovered session, let me read the recovery file…","tool_calls":[{"function":{"name":"read","arguments":{"path":"recovery.md"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":90,"eval_count":40}`,
			))
		case 2: // after the tool result: real progress (the file was read)
			_, _ = w.Write(ndjsonLines(
				`{"message":{"content":"Read the recovery file — proceeding with the task."}}`,
				`{"message":{"content":" ORCHICON WORKER SUMMARY: success — recovery complete"},"done":true,"done_reason":"stop","prompt_eval_count":120,"eval_count":60}`,
			))
		default:
			t.Errorf("unexpected provider turn %d", calls)
		}
	}))
	t.Cleanup(srv.Close)

	c := &OllamaClient{Host: srv.URL, NumCtxDefault: 8192}
	tools := newMockTools()
	tools.results["read"] = "recovery file contents"
	s, err := NewSession(SessionConfig{
		ExecRow:    testExecRow("exec_ollama_e2e"),
		Manifest:   testManifest("orchicon/ollama/deepseek-v4-flash"),
		ProjectDir: t.TempDir(),
		Provider:   c,
		Tools:      tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordedCallback{}
	if err := s.Run(context.Background(), cb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _, toolCalls, _, _, _, results := cb.snapshot()
	if len(toolCalls) != 1 || !strings.Contains(toolCalls[0], `read({"path":"recovery.md"})`) {
		t.Fatalf("OnToolCall = %v, want the native tool call executed", toolCalls)
	}
	if len(results) != 1 || !results[0].succeeded {
		t.Fatalf("OnResult = %+v, want success", results)
	}
	if calls != 2 {
		t.Fatalf("provider turns = %d, want 2 (turn 1 executed its tool, turn 2 settled)", calls)
	}
	// Turn 2's request must carry the assistant tool_use + tool result
	// (the model saw the result of its native tool call — no count-and-drop).
	req2 := bodies[1]
	if !strings.Contains(req2, `"tool_calls"`) || !strings.Contains(req2, "recovery file contents") {
		t.Fatalf("turn-2 request missing tool_use/tool result feedback: %s", req2)
	}
}
