package askorchicon

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
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
	results []toolResultEntry
}

type toolCallEntry struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments    string `json:"arguments"`
	// IssuedAtUnixMs is the wall-clock time at which the call was ISSUED, in
	// epoch milliseconds — the same time encoding the chat path already uses
	// (chat.ParsedTool.At). It is display-only durable data for the
	// rolling-window summarizer, never a wire contract: the ToolCall proto has no
	// field for it. 0 means "not stamped" (a row persisted before this field
	// existed); a reader must treat 0 as unknown, never as the epoch.
	IssuedAtUnixMs int64 `json:"issued_at_unix_ms"`
	resolved       bool
}

// ledgerNowMillis is the ledger's clock seam: the epoch-millisecond time of a
// tool event. It is indirected through a package-level var because askorchicon
// has no injectable clock today and the ledger tests need to assert an EXACT
// issuance time rather than a range. Production never replaces it.
var ledgerNowMillis = func() int64 { return time.Now().UnixMilli() }

type toolResultEntry struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

func newToolLedger() *toolLedger { return &toolLedger{} }

// recordPermission appends one consent decision as a synthetic tool call +
// result pair, so the decision lands in the persisted transcript alongside the
// real tool calls (AC: the transcript records the decision). Nil-safe.
//
// askID is the ADAPTER's id for the ask that was decided, and it becomes the
// record's ID when present. THAT IS THE POINT OF THE PARAMETER: this record is
// durable — it is persisted with the assistant message — so it is the server's own
// answer to "what happened to ask X", available to a client that never saw the live
// stream event (a second tab, another device, or the same page after a reload). It
// used to be a sequential `perm-N`, which no client could attach to an ask, so every
// client had to infer the outcome from the live stream alone and a reload lost it.
//
// The sequential fallback is kept for a decision with no ask id (a worker-path
// record, or a defensive caller): those are transcript notes, not reconcilable
// resolutions, and they must still be recorded.
//
// The record is stamped with the decision's own wall-clock issue time: it IS a
// real event, and the window classifier excludes `permission.*` by name, so the
// stamp costs the summary nothing.
func (l *toolLedger) recordPermission(tool, target, verdict, detail, askID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id := strings.TrimSpace(askID)
	if id == "" {
		l.nextID++
		id = fmt.Sprintf("perm-%d", l.nextID)
	}
	out := "permission " + verdict
	if detail != "" {
		out += ": " + detail
	}
	l.calls = append(l.calls, toolCallEntry{
		ID:             id,
		Type:           "function",
		FunctionName:   "permission." + verdict,
		Arguments:      truncateLedgerString(target, 4000),
		IssuedAtUnixMs: ledgerNowMillis(),
	})
	l.results = append(l.results, toolResultEntry{
		ToolCallID: id,
		Output:     truncateLedgerString(out, 8000),
		IsError:    verdict == "deny" || verdict == "never_allow" || verdict == "policy_error",
	})
}

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
		ID:             fmt.Sprintf("tc-%d", l.nextID),
		Type:           "function",
		FunctionName:   tool,
		Arguments:      "{}",
		IssuedAtUnixMs: ledgerNowMillis(), // THE issue-time stamp: this call just went out.
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
			// The issue time is deliberately NOT re-stamped here: this branch
			// backfills arguments onto a call that recordStart already stamped,
			// and the timestamp taken at ISSUE is the one the window needs.
			l.calls[i].Arguments = argsJSON
			l.calls[i].resolved = true
			callID = l.calls[i].ID
			break
		}
	}
	if callID == "" {
		// Resolution without an observed start (re-attached mid-tool, or a
		// start event missed): synthesize the call so the result is kept. "Now"
		// is the honest issue time for it — the call is known to have happened
		// by the time its result arrived, and the start was never seen.
		l.nextID++
		callID = fmt.Sprintf("tc-%d", l.nextID)
		l.calls = append(l.calls, toolCallEntry{
			ID:             callID,
			Type:           "function",
			FunctionName:   tool,
			Arguments:      argsJSON,
			IssuedAtUnixMs: ledgerNowMillis(),
			resolved:       true,
		})
	}
	l.results = append(l.results, toolResultEntry{
		ToolCallID: callID,
		Output:     output,
		IsError:    status == "error",
	})
}

// recordToolResolution backfills a resolved call's arguments and appends its
// result, from the TYPED adapter-neutral fields of a SessionEvent (Kind
// "tool_result"). It is the typed counterpart of recordResolve, which reads an
// opencode-shaped Part map.
//
// An adapter that does not speak opencode's dialect must not have to imitate it
// to get its tool calls recorded. This is the entry point such an adapter uses;
// both land in the same ledger, so a client renders one transcript regardless of
// which adapter produced it.
func (l *toolLedger) recordToolResolution(tool, argsJSON, output string, isErr bool) {
	if l == nil || tool == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	callID := ""
	// Match the OLDEST unresolved call for the same tool.
	//
	// Direction matters, and this is not cosmetic. The native Ask path executes a
	// round's tool calls SEQUENTIALLY in the order the model issued them, so the
	// Nth resolution belongs to the Nth unresolved call of that name — scanning
	// forward is what keeps each call paired with its own arguments and result.
	// An earlier version of this scanned BACKWARD (copying recordResolve, where
	// parallel execution makes the order inherently arbitrary) and swapped the
	// arguments of two same-name calls: the transcript then showed call #1 with
	// call #2's command. Caught by TestToolResolutionResolvesOnlyOneCallPerEvent.
	//
	// The ledger mints its own ids at recordStart, so the transport's call id
	// cannot correlate; the tool name plus issue order is what the two sides
	// share.
	for i := 0; i < len(l.calls); i++ {
		if l.calls[i].FunctionName == tool && !l.calls[i].resolved {
			// The issue time is deliberately NOT re-stamped here either: the
			// backfill must not disturb the stamp recordStart took at issue.
			l.calls[i].resolved = true
			// Only replace the "{}" placeholder when real arguments arrived.
			if args := strings.TrimSpace(argsJSON); args != "" && args != "{}" {
				l.calls[i].Arguments = truncateLedgerString(argsJSON, 4000)
			}
			callID = l.calls[i].ID
			break
		}
	}
	if callID == "" {
		// Resolution without an observed start (re-attached mid-tool, or a start
		// event missed): synthesize the call so the result is kept — the same
		// rule recordResolve applies, including the "stamp now" answer.
		l.nextID++
		callID = fmt.Sprintf("tc-%d", l.nextID)
		l.calls = append(l.calls, toolCallEntry{
			ID: callID, Type: "function", FunctionName: tool,
			Arguments: truncateLedgerString(argsJSON, 4000), resolved: true,
			IssuedAtUnixMs: ledgerNowMillis(),
		})
	}
	l.results = append(l.results, toolResultEntry{
		ToolCallID: callID,
		Output:     truncateLedgerString(output, 8000),
		IsError:    isErr,
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
		pub = append(pub, toolCallEntry{ID: c.ID, Type: c.Type, FunctionName: c.FunctionName, Arguments: c.Arguments, IssuedAtUnixMs: c.IssuedAtUnixMs})
	}
	calls, _ = json.Marshal(pub)
	res := l.results
	if res == nil {
		res = []toolResultEntry{}
	}
	results, _ = json.Marshal(res)
	return calls, results
}

// repairedSnapshot returns the ledger's TERMINAL (tool_calls, tool_results)
// documents, with an explicit aborted result attached for every call that
// never resolved. The live snapshot() is what the throttled partial mirror
// writes while the turn runs (a call is legitimately unresolved for as long as
// it runs); the TERMINAL write — the finalize of any turn, including one that
// ends abnormally (token exhaustion, Stop, provider error, supersede) — must
// never persist an assistant row whose tool_calls have no matching
// tool_results, because the next provider that replays that history rejects
// the entire request ("No tool output found for function call …").
func (l *toolLedger) repairedSnapshot() (calls, results []byte) {
	calls, results = l.snapshot()
	return sanitizeAssistantToolLedger(calls, results)
}

// hasCalls reports whether the ledger recorded any tool call. A turn that was
// interrupted between a tool call and its result leaves calls but no reply
// text: its row must still be finalized (repaired) rather than left dangling.
func (l *toolLedger) hasCalls() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls) > 0
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
