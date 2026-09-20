package orchicon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// The two provider payloads below are the operator's VERBATIM report for
// "BUG: Ask Orchicon model switch fails — dangling tool_call replayed to the
// new provider (400)". They are the acceptance signal: an assistant message
// with tool_calls is replayed without its matching tool results.
const (
	fixtureProviderErrorNoToolOutput = `{"model":"muse-spark-1.3-contributor-free","error":{"param":"input","type":"invalid_request_error",` +
		`"message":"Error from provider (Console): Upstream request failed: [invalid_request_error] ` +
		`No tool output found for function call call_gskf7fpm."}}`

	fixtureProviderErrorUnansweredToolCalls = `{"error":{"message":"An assistant message with 'tool_calls' must be followed by tool messages ` +
		`responding to each 'tool_call_id'. (insufficient tool messages following tool_calls message)",` +
		`"type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
)

func strptr(s string) *string { return &s }

// danglingToolCallHistory is a session history poisoned by a turn that died
// between issuing a tool call and receiving its result (token exhaustion /
// abort / provider error) — the exact accumulated state in the report.
func danglingToolCallHistory() []Message {
	return []Message{
		{Role: RoleUser, Content: []Content{{Text: strptr("run the thing")}}},
		{Role: RoleAssistant, Content: []Content{
			{Text: strptr("on it")},
			{ToolUse: &ContentToolUse{ToolCallID: "call_gskf7fpm", Name: "bash", ArgsJSON: `{"cmd":"ls"}`}},
		}},
	}
}

func toolResultIDs(messages []Message) map[string]bool {
	out := map[string]bool{}
	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID != "" {
				out[c.ToolResult.ToolCallID] = true
			}
		}
	}
	return out
}

// TestReproInterruptedTurnLeavesDanglingToolCall is the reproduction: the
// history an interrupted turn leaves is NOT something a provider accepts (it
// is what produced both 400 payloads above), and the sanitizer — which runs at
// the replay boundary before every provider send — makes it well-formed.
func TestReproInterruptedTurnLeavesDanglingToolCall(t *testing.T) {
	history := danglingToolCallHistory()
	if !hasDanglingToolCalls(history) {
		t.Fatal("reproduction failed: the interrupted-turn history is not detected as dangling")
	}
	// The dangling call id in the history is the one the provider names in its
	// rejection, so the fixture is the right regression signal.
	if !strings.Contains(fixtureProviderErrorNoToolOutput, "call_gskf7fpm") {
		t.Fatal("fixture no longer names the dangling call id")
	}

	repaired := sanitizeChatHistory(history)
	if hasDanglingToolCalls(repaired) {
		t.Fatal("sanitized history still has a tool call with no result")
	}
	if !toolResultIDs(repaired)["call_gskf7fpm"] {
		t.Fatal("sanitized history lost the tool call instead of recording an aborted result")
	}
	// Idempotent: the replay boundary runs on every turn.
	if again := sanitizeChatHistory(repaired); hasDanglingToolCalls(again) || len(again) != len(repaired) {
		t.Fatalf("sanitize is not idempotent: %d -> %d messages", len(repaired), len(again))
	}
}

func TestSanitizeChatHistoryMultipleDanglingCalls(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []Content{{Text: strptr("do three things")}}},
		{Role: RoleAssistant, Content: []Content{
			{ToolUse: &ContentToolUse{ToolCallID: "call_a", Name: "bash", ArgsJSON: "{}"}},
			{ToolUse: &ContentToolUse{ToolCallID: "call_b", Name: "read", ArgsJSON: "{}"}},
			{ToolUse: &ContentToolUse{ToolCallID: "call_c", Name: "grep", ArgsJSON: "{}"}},
		}},
		// One of the three DID return before the turn died.
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_b", Content: "ok"}}}},
	}
	repaired := sanitizeChatHistory(history)
	if hasDanglingToolCalls(repaired) {
		t.Fatal("multiple dangling calls were not repaired")
	}
	set := toolResultIDs(repaired)
	for _, id := range []string{"call_a", "call_b", "call_c"} {
		if !set[id] {
			t.Fatalf("tool call %s has no result after sanitize", id)
		}
	}
	// Exactly the two unresolved calls got aborted results.
	aborted := 0
	for _, m := range repaired {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.Content == danglingToolResultOutput {
				aborted++
				if !c.ToolResult.IsError {
					t.Fatal("an aborted tool result must be marked as an error")
				}
			}
		}
	}
	if aborted != 2 {
		t.Fatalf("got %d aborted results, want 2", aborted)
	}
}

func TestSanitizeChatHistoryLeavesHealthyHistoryAlone(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []Content{{Text: strptr("q")}}},
		{Role: RoleAssistant, Content: []Content{{Text: strptr("a")}}},
		{Role: RoleUser, Content: []Content{{Text: strptr("q2")}}},
		{Role: RoleAssistant, Content: []Content{
			{ToolUse: &ContentToolUse{ToolCallID: "call_ok", Name: "bash", ArgsJSON: "{}"}},
		}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_ok", Content: "done"}}}},
	}
	repaired := sanitizeChatHistory(history)
	if len(repaired) != len(history) {
		t.Fatalf("a well-formed history changed shape: %d -> %d", len(history), len(repaired))
	}
	for i := range repaired {
		if repaired[i].Role != history[i].Role || len(repaired[i].Content) != len(history[i].Content) {
			t.Fatalf("message %d changed: %+v -> %+v", i, history[i], repaired[i])
		}
	}
}

func TestSanitizeChatHistoryDropsUnaddressableToolUse(t *testing.T) {
	history := []Message{
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{Name: "bash", ArgsJSON: "{}"}}}},

		{Role: RoleAssistant, Content: []Content{
			{Text: strptr("kept")},
			{ToolUse: &ContentToolUse{Name: "grep", ArgsJSON: "{}"}},
		}},
	}
	repaired := sanitizeChatHistory(history)
	if len(repaired) != 1 {
		t.Fatalf("got %d messages, want 1 (the id-less call-only message is dropped)", len(repaired))
	}
	if len(repaired[0].Content) != 1 || repaired[0].Content[0].Text == nil || *repaired[0].Content[0].Text != "kept" {
		t.Fatalf("unexpected surviving content: %+v", repaired[0])
	}
	if hasDanglingToolCalls(repaired) {
		t.Fatal("id-less tool uses survived the sanitizer")
	}
}

// TestChatTurnReplayRepairsDanglingSessionHistory asserts the provider-bound
// window: a session poisoned by an interrupted turn is repaired at the replay
// boundary, so the request the provider receives pairs every tool call — and
// the repair is persisted, so the session heals permanently.
func TestChatTurnReplayRepairsDanglingSessionHistory(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "ok"}, Finish{StopReason: StopStop}}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-dangling", "ask-orchicon:conv-dangling")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = danglingToolCallHistory()
	b.mu.Unlock()

	bus, _ := b.Subscribe(ctx, "conv-dangling")
	if err := b.SendTurnMessage(ctx, "conv-dangling", sid, "system", "orchicon/ollama/deepseek-v4-flash", "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	drainBus(t, bus)

	req := prov.lastRequest()
	if hasDanglingToolCalls(req.Messages) {
		t.Fatal("the provider window still carries an unanswered tool call (the 400 shape)")
	}
	if !toolResultIDs(req.Messages)["call_gskf7fpm"] {
		t.Fatal("the provider window does not record the aborted tool call")
	}

	b.mu.Lock()
	stored := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if hasDanglingToolCalls(stored) {
		t.Fatal("the repaired history was not persisted back onto the session")
	}
}

// TestChatTurnDanglingRejectionSurfacedVerbatimThenNextSendSucceeds asserts
// the operator-visible half of the bug: the provider 400 reaches the caller in
// the provider's own words, and the conversation does NOT wedge — the next
// send on the same session dispatches on repaired, accepted history.
func TestChatTurnDanglingRejectionSurfacedVerbatimThenNextSendSucceeds(t *testing.T) {
	prov := &chatTestProvider{preErr: errors.New(fixtureProviderErrorNoToolOutput)}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, _ := b.CreateConversationSession(ctx, "conv-400", "ask-orchicon:conv-400")

	if _, err := b.Subscribe(ctx, "conv-400"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	err := b.SendTurnMessage(ctx, "conv-400", sid, "system", "orchicon/ollama/deepseek-v4-flash", "hi")
	if err == nil {
		t.Fatal("the provider rejection was swallowed")
	}
	if !strings.Contains(err.Error(), "No tool output found for function call call_gskf7fpm.") {
		t.Fatalf("error %q does not carry the provider payload verbatim", err.Error())
	}
	// A pre-stream rejection never starts the drain goroutine, so its bus
	// stays open — the collector fails the turn and re-subscribes.

	// Repair path: the same session is reused, and the next send is accepted.
	prov.mu.Lock()
	prov.preErr = nil
	prov.mu.Unlock()
	bus2, _ := b.Subscribe(ctx, "conv-400")
	if err := b.SendTurnMessage(ctx, "conv-400", sid, "system", "orchicon/ollama/deepseek-v4-flash", "again"); err != nil {
		t.Fatalf("the conversation wedged after the 400: %v", err)
	}
	events := drainBus(t, bus2)
	var sawIdle bool
	for _, e := range events {
		if e.Kind == "idle" {
			sawIdle = true
		}
	}
	if !sawIdle {
		t.Fatal("the repaired turn did not complete")
	}
	if hasDanglingToolCalls(prov.lastRequest().Messages) {
		t.Fatal("the retried request still carries a dangling tool call")
	}
}

// TestDanglingFixturePayloadsAreTheTwoProviderShapes pins the two exact
// operator payloads as the regression fixtures for this bug.
func TestDanglingFixturePayloadsAreTheTwoProviderShapes(t *testing.T) {
	for _, payload := range []string{fixtureProviderErrorNoToolOutput, fixtureProviderErrorUnansweredToolCalls} {
		if !json.Valid([]byte(payload)) {
			t.Fatalf("fixture is not valid JSON: %s", payload)
		}
	}
	if !strings.Contains(fixtureProviderErrorUnansweredToolCalls, "must be followed by tool messages") {
		t.Fatal("second fixture lost its wording")
	}
}
