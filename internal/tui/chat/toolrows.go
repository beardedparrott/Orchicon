package chat

// toolrows.go — DURABLE tool calls become transcript rows.
//
// THE DEFECT THIS FIXES. `conversationItems` turned a message's prose, its reasoning parts and its
// recorded ask_user card into items, and then DROPPED its tool calls on the floor. Tool rows therefore
// existed only while the SSE stream was live (ChatStreamResponse_ToolCallStart -> appendChunk), so every
// tool call a turn had ever made VANISHED the moment the turn finalized and the client re-read its page.
//
// That is invisible on the native/opencode adapters, whose recorded calls are mostly one-line reads, and
// it is the whole transcript for an antigravity turn: agy reports its work AS tool calls (run_command,
// view_file, read_url_content) — the operator's only record of what it did. Reloading an agy
// conversation showed a reply with no evidence of the work behind it, and the refused-write frame in
// AC3's mode-refused capture could not be produced at all.
//
// WHY THE RESULT IS PAIRED HERE. A call and its result are SEPARATE ledger rows keyed by tool_call_id.
// The row wants both — the subject comes from the call's arguments, the output from the result — so they
// are joined by id and drawn as ONE row, which is also what the GUI's ToolCard does with the same pair.

import (
	"strings"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// toolCallRendered reports whether a recorded tool call becomes a row of its own.
//
// TWO KINDS OF CALL ARE NOT ROWS:
//   - an ask_user call: it renders as its CARD (see parseAskUserCall), and drawing a row beside the card
//     would show the operator a duplicate of the question they are being asked;
//   - a permission ledger row (`permission.*`): those are the RESOLUTIONS of a consent card, and
//     settledAsksFromPage already reads them to settle the card. As a row they would say "permission"
//     and nothing else.
func toolCallRendered(functionName string) bool {
	n := strings.ToLower(strings.TrimSpace(functionName))
	if n == "" || isAskUserCall(n) {
		return false
	}
	return !strings.HasPrefix(n, "permission.")
}

// toolRowsForMessage turns one message's recorded calls (and their results) into tool items, in ledger
// order, each carrying the call's own subject material and the result that answered it.
func toolRowsForMessage(m *apiv1.ChatMessage) []ChatItem {
	calls := m.GetToolCalls()
	if len(calls) == 0 {
		return nil
	}
	// INDEX THE RESULTS ONCE. A page can hold hundreds of calls, and a linear scan per call is the kind of
	// quadratic that only shows up on the conversations an operator most needs to open.
	results := make(map[string]*apiv1.ToolResult, len(m.GetToolResults()))
	for _, r := range m.GetToolResults() {
		if r != nil && r.GetToolCallId() != "" {
			results[r.GetToolCallId()] = r
		}
	}
	at := m.GetCreatedAt().AsTime().UnixMilli()
	out := make([]ChatItem, 0, len(calls))
	for i, c := range calls {
		if c == nil || !toolCallRendered(c.GetFunctionName()) {
			continue
		}
		id := c.GetId()
		// A RESULT MAY ARRIVE UNDER THE SAME ID OR (for a call recorded without one) NOT AT ALL. Falling back
		// to the call's index keeps an id-less ledger readable instead of silently dropping every output.
		tool := &ParsedTool{
			ID:       id,
			ToolName: c.GetFunctionName(),
			Input:    c.GetArguments(),
			At:       at,
		}
		if r := results[id]; r != nil {
			tool.Output = r.GetOutput()
			tool.Failed = r.GetIsError()
		} else if id == "" {
			for _, r := range m.GetToolResults() {
				if r != nil && r.GetToolCallId() == "" {
					tool.Output = r.GetOutput()
					tool.Failed = r.GetIsError()
					break
				}
			}
		}
		key := "tc-" + id
		if id == "" {
			key = "tc-" + m.GetId() + "-" + itoa(int64(i))
		}
		out = append(out, ChatItem{Kind: KindTool, Tool: tool, At: at, Key: key})
	}
	return out
}
