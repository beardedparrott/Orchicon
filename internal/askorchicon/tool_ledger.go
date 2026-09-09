package askorchicon

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Live tool ledger for Ask Orchicon turns: every tool call issued and every
// result observed during a turn is recorded in memory as it happens, mirrored
// incrementally into the acked assistant message row (tool_calls /
// tool_results columns), and persisted terminally by the finalize. A session
// killed mid-turn therefore leaves the tool activity visible — not just the
// text. JSON shapes mirror the ChatMessage proto (ToolCall{ id, type,
// function_name, arguments } / ToolResult{ tool_call_id, output, is_error })
// so the read path maps them without translation.
type toolLedger struct {
	mu      sync.Mutex
	nextID  int
	calls   []toolCallEntry
	results  []toolResultEntry
}

type toolCallEntry struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments   string `json:"arguments"`
	resolved    bool
}

type toolResultEntry struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

func newToolLedger() *toolLedger { return &toolLedger{} }

// recordStart logs a tool call ISSUED but not yet resolved (the tool_part
// signal: Text carries only the tool name). The arguments are filled in when
// the resolution arrives (recordResolve matches the last unresolved call for
// the same tool).
func (l *toolLedger) recordStart(tool string) {
	if l == nil || tool == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	l.calls = append(l.calls, toolCallEntry{
		ID:           fmt.Sprintf("tc-%d", l.nextID),
		Type:         "function",
		FunctionName: tool,
		Arguments:    "{}",
	})
}

// recordResolve logs a COMPLETED tool_use part: it backfills the arguments
// onto the matching in-flight call and appends the result. Unknown shapes
// never fail the turn — the ledger is best-effort durability, not control.
func (l *toolLedger) recordResolve(part map[string]any) {
	if l == nil || part == nil {
		return
	}
	tool, _ := part["tool"].(string)
	if tool == "" {
		return
	}
	state, _ := part["state"].(map[string]any)
	status, _ := state["status"].(string)
	argsJSON := truncateLedgerString(marshalLedgerValue(state["input"]), 4000)
	output := truncateLedgerString(stringifyLedgerOutput(state["output"]), 8000)

	l.mu.Lock()
	defer l.mu.Unlock()
	callID := ""
	for i := len(l.calls) - 1; i >= 0; i-- {
		if l.calls[i].FunctionName == tool && !l.calls[i].resolved {
			l.calls[i].Arguments = argsJSON
			l.calls[i].resolved = true
			callID = l.calls[i].ID
			break
		}
	}
	if callID == "" {
		// Resolution without an observed start (re-attached mid-tool, or a
		// start event missed): synthesize the call so the result is kept.
		l.nextID++
		callID = fmt.Sprintf("tc-%d", l.nextID)
		l.calls = append(l.calls, toolCallEntry{
			ID:           callID,
			Type:         "function",
			FunctionName: tool,
			Arguments:    argsJSON,
			resolved:    true,
		})
	}
	l.results = append(l.results, toolResultEntry{
		ToolCallID: callID,
		Output:     output,
		IsError:    status == "error",
	})
}

// snapshot returns the ledger as (tool_calls, tool_results) JSON documents
// ready for the message columns. Empty ledgers encode as "[]" (never nil)
// so every write keeps the NOT NULL jsonb columns valid.
func (l *toolLedger) snapshot() (calls, results []byte) {
	if l == nil {
		return []byte("[]"), []byte("[]")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	pub := make([]toolCallEntry, 0, len(l.calls))
	for _, c := range l.calls {
		pub = append(pub, toolCallEntry{ID: c.ID, Type: c.Type, FunctionName: c.FunctionName, Arguments: c.Arguments})
	}
	calls, _ = json.Marshal(pub)
	res := l.results
	if res == nil {
		res = []toolResultEntry{}
	}
	results, _ = json.Marshal(res)
	return calls, results
}

func marshalLedgerValue(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func stringifyLedgerOutput(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncateLedgerString(s string, max int) string {
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}
