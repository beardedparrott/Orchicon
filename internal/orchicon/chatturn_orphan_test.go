package orchicon

// The MIRROR of the dangling-tool-call bug (chatturn_dangling_test.go): an
// ORPHANED tool result — a `tool` message whose preceding assistant `tool_calls`
// message was cropped away. Providers reject it with:
//
//	Messages with role 'tool' must be a response to a preceding message with
//	'tool_calls'
//
// Operator report (verbatim), after pressing the new compact button on Ask
// conversation 01M2C8VXFQY5ZE26PYBSNKA2CA and then sending a message:
//
//	conversation session send: orchicon bridge: start Ask turn: provider status
//	400 400 Bad Request: {"error":{"message":"Messages with role 'tool' must be a
//	response to a preceding message with 'tool_calls'", ...}}
//
// Unlike the dangling-call shape, nothing can be invented to repair it (there is
// no call id to attach a synthesized result to), so the ONLY correct repair is
// to drop the orphaned result. Because the native transport re-sends the whole
// history every turn, an un-dropped orphan wedges the conversation permanently.

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tenant"
)

// fixtureProviderErrorOrphanedToolResult is the operator's verbatim payload for
// the compact-button regression (the third tool-pairing rejection shape).
const fixtureProviderErrorOrphanedToolResult = `{"error":{"message":"Messages with role 'tool' must be a response to a preceding ` +
	`message with 'tool_calls'","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`

// hasOrphanedToolResults reports whether a provider-bound history contains a
// tool-role message whose result answers a call that no PRECEDING assistant
// message declares. It is the mirror of hasDanglingToolCalls and is the other
// half of the replay-boundary invariant (see sanitizeChatHistory).
func hasOrphanedToolResults(messages []Message) bool {
	declared := map[string]bool{}
	for _, m := range messages {
		switch m.Role {
		case RoleAssistant:
			for _, c := range m.Content {
				if c.ToolUse != nil && c.ToolUse.ToolCallID != "" {
					declared[c.ToolUse.ToolCallID] = true
				}
			}
		case RoleTool:
			for _, c := range m.Content {
				if c.ToolResult == nil || c.ToolResult.ToolCallID == "" || !declared[c.ToolResult.ToolCallID] {
					return true
				}
			}
		}
	}
	return false
}

// assertHistoryToolPairsWellFormed asserts the FULL replay-boundary invariant,
// both directions: every declared call answered, and every result answering a
// declared call. A history that fails either half is one a provider rejects.
func assertHistoryToolPairsWellFormed(t *testing.T, messages []Message) {
	t.Helper()
	if hasDanglingToolCalls(messages) {
		t.Fatalf("history has a tool call with no result (providers reject this): %s", describeHistory(messages))
	}
	if hasOrphanedToolResults(messages) {
		t.Fatalf("history has a tool result answering no preceding call (providers reject this): %s", describeHistory(messages))
	}
}

// describeHistory renders the role/call-id sequence so a failure names the shape
// rather than just a count.
func describeHistory(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		if len(m.Content) == 0 {
			parts = append(parts, string(m.Role))
			continue
		}
		for _, c := range m.Content {
			switch {
			case c.ToolUse != nil:
				parts = append(parts, string(m.Role)+"("+c.ToolUse.ToolCallID+")")
			case c.ToolResult != nil:
				parts = append(parts, string(m.Role)+"("+c.ToolResult.ToolCallID+")")
			default:
				parts = append(parts, string(m.Role))
			}
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func historyContainsToolResult(messages []Message, id string) bool {
	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID == id {
				return true
			}
		}
	}
	return false
}

func historyContainsText(messages []Message, want string) bool {
	for _, m := range messages {
		for _, c := range m.Content {
			if c.Text != nil && strings.Contains(*c.Text, want) {
				return true
			}
		}
	}
	return false
}

// compactedOrphanedHistory is the history the compact button left on Ask
// conversation 01M2C8VXFQY5ZE26PYBSNKA2CA, read VERBATIM off that session's
// persisted file (/var/lib/orchicon/ask-history/orchicon-ask_<id>.json): the
// summary, then a tool result whose declaring assistant was collapsed away,
// then the kept tail, then the operator's send that 400'd.
func compactedOrphanedHistory() []Message {
	return []Message{
		{Role: RoleAssistant, Content: []Content{{Text: strptr(
			"[Earlier conversation compacted to save context. Summary of everything before this point:]\n\n" +
				"# Conversation Summary\n\n## 1. Goal\n\nFinish the Orchicon TUI re-architecture + GUI parity branch.")}}},
		// ORPHAN: answers call_cropped, declared by the assistant message that the
		// compaction's tail slice collapsed away.
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
			ToolCallID: "call_cropped", Content: "internal/tui/app.go contents…",
		}}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{
			ToolCallID: "call_t1", Name: "batch_read", ArgsJSON: `{"paths":["internal/tui/app.go"]}`,
		}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
			ToolCallID: "call_t1", Content: "package tui\n",
		}}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{
			ToolCallID: "call_t2", Name: "bash", ArgsJSON: `{"command":"git status --short"}`,
		}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{
			ToolCallID: "call_t2", Content: "?? tuimockup/",
		}}}},
		{Role: RoleAssistant, Content: []Content{{Text: strptr("Both files are clean; nothing staged.")}}},
		{Role: RoleUser, Content: []Content{{Text: strptr("I just tried the new compact button…")}}},
	}
}

// TestSanitizeChatHistoryDropsOrphanedToolResult is the reproduction and the
// fix: the compacted history is rejected as-is, and the sanitizer — which runs at
// the replay boundary before every provider send — drops the orphan while
// keeping every intact round and the summary.
func TestSanitizeChatHistoryDropsOrphanedToolResult(t *testing.T) {
	history := compactedOrphanedHistory()

	// Reproduction: the poisoned session really is un-sendable as it stands.
	if !hasOrphanedToolResults(history) {
		t.Fatal("reproduction failed: the compacted history is not detected as orphaned")
	}

	repaired := sanitizeChatHistory(history)
	assertHistoryToolPairsWellFormed(t, repaired)

	// The orphan is GONE (not tolerated) — no call can be invented for it.
	if historyContainsToolResult(repaired, "call_cropped") {
		t.Fatalf("the orphaned result survived the sanitizer: %s", describeHistory(repaired))
	}
	// Every intact round and the summary survive: the repair drops the orphan,
	// not the conversation.
	for _, id := range []string{"call_t1", "call_t2"} {
		if !historyContainsToolResult(repaired, id) {
			t.Fatalf("healthy round %s was dropped with the orphan: %s", id, describeHistory(repaired))
		}
	}
	if !historyContainsText(repaired, "Earlier conversation compacted") {
		t.Fatal("the compact summary was dropped")
	}

	// Idempotent: the replay boundary runs on EVERY turn, so a repaired history
	// must be a fixed point.
	if again := sanitizeChatHistory(repaired); hasOrphanedToolResults(again) || len(again) != len(repaired) {
		t.Fatalf("sanitize is not idempotent: %d -> %d messages", len(repaired), len(again))
	}
}

// TestSanitizeChatHistoryDropsLeadingOrphanedToolResult pins the shape the
// compaction tail alignment clamps against: a history whose FIRST message is a
// tool result (nothing precedes it at all, so no walk-back can save it).
func TestSanitizeChatHistoryDropsLeadingOrphanedToolResult(t *testing.T) {
	history := []Message{
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_gone", Content: "orphan"}}}},
		{Role: RoleUser, Content: []Content{{Text: strptr("continue")}}},
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call_ok", Name: "bash", ArgsJSON: "{}"}}}},
		{Role: RoleTool, Content: []Content{{ToolResult: &ContentToolResult{ToolCallID: "call_ok", Content: "done"}}}},
	}
	repaired := sanitizeChatHistory(history)
	assertHistoryToolPairsWellFormed(t, repaired)
	if historyContainsToolResult(repaired, "call_gone") {
		t.Fatalf("the leading orphan survived: %s", describeHistory(repaired))
	}
	if !historyContainsToolResult(repaired, "call_ok") {
		t.Fatalf("the intact round was dropped: %s", describeHistory(repaired))
	}
}

// TestSanitizeChatHistoryDropsOrphanedResultInsideToolMessage covers a tool
// message that carries several results of which only some are orphaned: the
// declared ones must survive, and a message left with nothing goes away.
func TestSanitizeChatHistoryDropsOrphanedResultInsideToolMessage(t *testing.T) {
	history := []Message{
		{Role: RoleAssistant, Content: []Content{{ToolUse: &ContentToolUse{ToolCallID: "call_a", Name: "bash", ArgsJSON: "{}"}}}},
		{Role: RoleTool, Content: []Content{
			{ToolResult: &ContentToolResult{ToolCallID: "call_a", Content: "kept"}},
			{ToolResult: &ContentToolResult{ToolCallID: "call_orphan", Content: "dropped"}},
		}},
		{Role: RoleTool, Content: []Content{
			{ToolResult: &ContentToolResult{ToolCallID: "call_also_orphan", Content: "dropped"}},
		}},
	}
	repaired := sanitizeChatHistory(history)
	assertHistoryToolPairsWellFormed(t, repaired)
	if !historyContainsToolResult(repaired, "call_a") {
		t.Fatal("the declared result was dropped")
	}
	for _, id := range []string{"call_orphan", "call_also_orphan"} {
		if historyContainsToolResult(repaired, id) {
			t.Fatalf("orphaned result %s survived: %s", id, describeHistory(repaired))
		}
	}
	// The all-orphan message is gone entirely (not left as an empty tool message).
	for _, m := range repaired {
		if m.Role == RoleTool && len(m.Content) == 0 {
			t.Fatalf("an emptied tool message was left behind: %s", describeHistory(repaired))
		}
	}
}

// TestChatTurnReplayHealsCompactedOrphanedHistory asserts the operator-visible
// recovery: a conversation wedged by the compact button becomes sendable again
// ON THE SAME SESSION, with no manual surgery — the repair happens at the replay
// boundary and is persisted, so the session heals permanently rather than being
// repaired on every turn.
func TestChatTurnReplayHealsCompactedOrphanedHistory(t *testing.T) {
	prov := &chatTestProvider{events: []Event{TextDelta{Text: "ok"}, Finish{StopReason: StopStop}}}
	b := newChatBridge(t, prov)
	ctx := tenant.WithID(context.Background(), "tnt_test")
	sid, err := b.CreateConversationSession(ctx, "conv-compacted", "ask-orchicon:conv-compacted")
	if err != nil {
		t.Fatalf("CreateConversationSession: %v", err)
	}
	b.mu.Lock()
	b.chatHistory[sid] = compactedOrphanedHistory()
	b.mu.Unlock()

	bus, _ := b.Subscribe(ctx, "conv-compacted")
	if err := b.SendTurnMessage(ctx, "conv-compacted", sid, "system", "orchicon/deepseek/deepseek-flash", "continue"); err != nil {
		t.Fatalf("send: %v", err)
	}
	drainBus(t, bus)

	// What the provider received is well-formed in both directions, and the
	// orphan is not in it.
	req := prov.lastRequest()
	assertHistoryToolPairsWellFormed(t, req.Messages)
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.ToolResult != nil && c.ToolResult.ToolCallID == "call_cropped" {
				t.Fatalf("the orphaned result was replayed to the provider: %s", describeHistory(req.Messages))
			}
		}
	}

	// The repair is PERSISTED back onto the session.
	b.mu.Lock()
	stored := append([]Message(nil), b.chatHistory[sid]...)
	b.mu.Unlock()
	if hasOrphanedToolResults(stored) {
		t.Fatalf("the repaired history was not persisted onto the session: %s", describeHistory(stored))
	}

	// The conversation is intact — summary and rounds kept.
	if !historyContainsText(stored, "Earlier conversation compacted") {
		t.Fatal("the compact summary was dropped by the repair")
	}
	for _, id := range []string{"call_t1", "call_t2"} {
		if !historyContainsToolResult(stored, id) {
			t.Fatalf("healthy round %s was lost: %s", id, describeHistory(stored))
		}
	}

	// Healing, asserted as a property: the NEXT send dispatches on clean history
	// too, so the wedge does not come back on the following turn.
	prov.mu.Lock()
	prov.preErr = nil
	prov.mu.Unlock()
	bus2, _ := b.Subscribe(ctx, "conv-compacted")
	if err := b.SendTurnMessage(ctx, "conv-compacted", sid, "system", "orchicon/deepseek/deepseek-flash", "again"); err != nil {
		t.Fatalf("the conversation re-wedged after the repair turn: %v", err)
	}
	drainBus(t, bus2)
	assertHistoryToolPairsWellFormed(t, prov.lastRequest().Messages)
}
