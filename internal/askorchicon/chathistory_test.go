package askorchicon

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// The two provider payloads are the operator's VERBATIM report for "BUG: Ask
// Orchicon model switch fails — dangling tool_call replayed to the new
// provider (400)" (acceptance fixtures).
const (
	fixtureProviderErrorNoToolOutput = `{"model":"muse-spark-1.3-contributor-free","error":{"param":"input","type":"invalid_request_error",` +
		`"message":"Error from provider (Console): Upstream request failed: [invalid_request_error] ` +
		`No tool output found for function call call_gskf7fpm."}}`

	fixtureProviderErrorUnansweredToolCalls = `{"error":{"message":"An assistant message with 'tool_calls' must be followed by tool messages ` +
		`responding to each 'tool_call_id'. (insufficient tool messages following tool_calls message)",` +
		`"type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
)

// assertedLedgerWellFormed fails the test when a persisted (tool_calls,
// tool_results) pair holds a call with no matching result — the DB-row-level
// invariant asserted by the acceptance criteria.
func assertLedgerWellFormed(t *testing.T, callsJSON, resultsJSON []byte) {
	t.Helper()
	var calls []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(callsJSON, &calls); err != nil {
		t.Fatalf("tool_calls not decodable: %v (%s)", err, callsJSON)
	}
	var results []struct {
		ToolCallID string `json:"tool_call_id"`
		IsError    bool   `json:"is_error"`
	}
	if err := json.Unmarshal(resultsJSON, &results); err != nil {
		t.Fatalf("tool_results not decodable: %v (%s)", err, resultsJSON)
	}
	answered := map[string]bool{}
	for _, r := range results {
		answered[r.ToolCallID] = true
	}
	for _, c := range calls {
		if !answered[c.ID] {
			t.Fatalf("tool call %q has no matching tool result (the dangling-call 400 shape): calls=%s results=%s", c.ID, callsJSON, resultsJSON)
		}
	}
}

func TestSanitizeAssistantToolLedgerRepairsDanglingCalls(t *testing.T) {
	// Single dangling call.
	calls, results := sanitizeAssistantToolLedger(
		[]byte(`[{"id":"call_gskf7fpm","type":"function","function_name":"bash","arguments":"{}"}]`),
		[]byte("[]"))
	assertLedgerWellFormed(t, calls, results)
	if !strings.Contains(string(results), abortedToolResultOutput) {
		t.Fatalf("single dangling call got no explicit aborted result: %s", results)
	}
	if !strings.Contains(string(results), "call_gskf7fpm") {
		t.Fatalf("aborted result does not name the call: %s", results)
	}

	// Multiple dangling calls, one already answered.
	calls, results = sanitizeAssistantToolLedger(
		[]byte(`[{"id":"tc-1","function_name":"bash"},{"id":"tc-2","function_name":"read"},{"id":"tc-3","function_name":"grep"}]`),
		[]byte(`[{"tool_call_id":"tc-2","output":"ok","is_error":false}]`))
	assertLedgerWellFormed(t, calls, results)
	if strings.Count(string(results), abortedToolResultOutput) != 2 {
		t.Fatalf("want exactly 2 aborted results, got %s", results)
	}
	if strings.Count(string(results), "\"tc-2\"") != 1 {
		t.Fatalf("the resolved call must keep exactly one result: %s", results)
	}

	// The type defaults to function so the column shape stays valid.
	if !strings.Contains(string(calls), `"type":"function"`) {
		t.Fatalf("call type not defaulted: %s", calls)
	}

	// Idempotent: a repaired pair is returned unchanged.
	calls2, results2 := sanitizeAssistantToolLedger(calls, results)
	assertLedgerWellFormed(t, calls2, results2)
	if strings.Count(string(results2), abortedToolResultOutput) != 2 {
		t.Fatalf("sanitize is not idempotent: %s", results2)
	}
}

func TestToolLedgerRepairedSnapshotNeverPersistsDanglingCalls(t *testing.T) {
	l := newToolLedger()
	l.recordStart("bash") // issued, never resolved (the interrupted turn)
	calls, results := l.repairedSnapshot()
	assertLedgerWellFormed(t, calls, results)
	if !l.hasCalls() {
		t.Fatal("hasCalls = false with an issued call — the superseded finalize would skip the row")
	}

	// A second dangling call plus one that resolved.
	l.recordStart("read")
	l.recordResolve(map[string]any{
		"tool":  "bash",
		"state": map[string]any{"status": "completed", "input": map[string]any{"cmd": "ls"}, "output": "file.go"},
	})
	calls, results = l.repairedSnapshot()
	assertLedgerWellFormed(t, calls, results)
	var decoded []struct {
		ToolCallID string `json:"tool_call_id"`
	}
	if err := json.Unmarshal(results, &decoded); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 results (1 real + 1 aborted), got %d: %s", len(decoded), results)
	}
	// The live snapshot stays untouched (in-flight calls are legitimately
	// unresolved while the turn runs).
	liveCalls, liveResults := l.snapshot()
	if strings.Contains(string(liveResults), abortedToolResultOutput) {
		t.Fatalf("the live mirror snapshot must not synthesize aborted results: %s", liveResults)
	}
	_ = liveCalls
}

func TestSanitizeHistoryRowsRepairsAssistantRows(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"model_ref": "orchicon/ollama/m", "session_id": "s1"})
	history := []db.MessageRow{
		{ID: "u1", Role: "user", Content: "do it", ToolCalls: []byte("[]"), ToolResults: []byte("[]")},
		{ID: "a1", Role: "assistant", Metadata: meta,
			ToolCalls:   []byte(`[{"id":"call_gskf7fpm","function_name":"bash"}]`),
			ToolResults: []byte("[]")},
		{ID: "a2", Role: "assistant",
			ToolCalls:   []byte(`[{"id":"tc-9","function_name":"read"}]`),
			ToolResults: []byte(`[{"tool_call_id":"tc-9","output":"ok"}]`)},
	}
	out := sanitizeHistoryRows(history)
	for _, m := range out {
		if m.Role == "assistant" {
			assertLedgerWellFormed(t, m.ToolCalls, m.ToolResults)
		}
	}
	// The user row and the already-resolved row are untouched.
	if string(out[0].ToolCalls) != "[]" {
		t.Fatalf("user row mutated: %s", out[0].ToolCalls)
	}
	if strings.Contains(string(out[2].ToolResults), abortedToolResultOutput) {
		t.Fatalf("a resolved row was rewritten: %s", out[2].ToolResults)
	}
	// The repaired row names the dangling call.
	if !strings.Contains(string(out[1].ToolResults), "call_gskf7fpm") {
		t.Fatalf("dangling call not repaired: %s", out[1].ToolResults)
	}
}

func TestIsDanglingToolCallProviderErrorMatchesOperatorPayloads(t *testing.T) {
	for _, payload := range []string{fixtureProviderErrorNoToolOutput, fixtureProviderErrorUnansweredToolCalls} {
		if !isDanglingToolCallProviderError("conversation session send: orchicon bridge: start Ask turn: provider status 400 400 Bad Request: " + payload) {
			t.Fatalf("payload not recognized as a dangling-tool-call rejection: %s", payload)
		}
	}
	for _, other := range []string{"", "request timed out after 60s", "provider status 429 too many requests", "turn stopped by the user"} {
		if isDanglingToolCallProviderError(other) {
			t.Fatalf("non-dangling error classified as a dangling rejection: %q", other)
		}
	}
}

func TestSessionCreatedUnderDifferentModel(t *testing.T) {
	row := func(model, session string) db.MessageRow {
		meta, _ := json.Marshal(map[string]any{"model_ref": model, "session_id": session})
		return db.MessageRow{ID: db.NewID(), Role: "assistant", Metadata: meta}
	}

	// The stored session was created under the OLD model.
	history := []db.MessageRow{row("orchicon/ollama/new-model", "s-new"), row("orchicon/ollama/old-model", "s-old")}
	if !sessionCreatedUnderDifferentModel("s-old", "orchicon/ollama/new-model", history) {
		t.Fatal("a model change over the stored session was not detected")
	}
	// Same model: keep the session (no gratuitous reset).
	if sessionCreatedUnderDifferentModel("s-new", "orchicon/ollama/new-model", history) {
		t.Fatal("an unchanged model was reported as a change")
	}
	// No session recorded (legacy rows): fall back to the newest model.
	legacy := []db.MessageRow{{
		ID:       db.NewID(),
		Role:     "assistant",
		Metadata: []byte(`{"model_ref":"orchicon/ollama/old-model"}`),
	}}
	if !sessionCreatedUnderDifferentModel("s-whatever", "orchicon/ollama/new-model", legacy) {
		t.Fatal("legacy row without a session id did not trigger the fresh-session guard")
	}
	if sessionCreatedUnderDifferentModel("s1", "", history) {
		t.Fatal("an empty model ref must not trigger the guard")
	}
	if sessionCreatedUnderDifferentModel("", "orchicon/ollama/new-model", nil) {
		t.Fatal("an empty history must not trigger the guard")
	}
}

func TestEmitTurnErrorSurfacesVerbatim(t *testing.T) {
	var got []*apiv1.ChatStreamResponse
	emitTurnError(func(r *apiv1.ChatStreamResponse) { got = append(got, r) }, fixtureProviderErrorNoToolOutput)
	if len(got) != 1 {
		t.Fatalf("want 1 error event, got %d", len(got))
	}
	if msg := got[0].GetError().GetMessage(); msg != fixtureProviderErrorNoToolOutput {
		t.Fatalf("error event not verbatim: %q", msg)
	}
	// Nil callback and empty text are no-ops (a turn dispatched without a
	// stream must not panic).
	emitTurnError(nil, "boom")
	emitTurnError(func(r *apiv1.ChatStreamResponse) { t.Fatal("empty error text must not be emitted") }, "")
}
