package askorchicon

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// Live tool ledger for Ask Orchicon turns: every tool call issued and every result observed during
// a turn is recorded in memory as it happens.
type toolLedger struct {
	mu      sync.Mutex
	nextID  int
	calls   []toolCallEntry
	results []toolResultEntry
}

// toolCallIDPrefix makes a SYNTHESIZED tool-call id unique for the life of the process.
//
// THE ID IS AN IDENTITY, NOT A LABEL, AND IT WAS BEING MINTED LIKE A LABEL. A ledger is created per TURN
// (chat.go: turnLedger := newToolLedger()), so its counter restarted at 1 every turn and every turn's ask
// became `tc-3`. Three separate things treat that string as an identity:
//
//	the CLIENT's settled-ask map (ask-orchicon.tsx: const settledAsks = new Map<string,string>())
//	findPromotedAsk's query (tool_calls @> '{"id": <askID>}', across the whole conversation)
//	the in-process pending-ask registry
//
// REPORTED: "clicking an answer sometimes doesn't work unless I first refresh the page." Answering an ask in an
// earlier turn put `tc-3` in the client's settled map; a LATER turn's ask was also `tc-3`; the card was drawn
// already-answered, so every option button was disabled and clicks did nothing. A refresh re-initialises the
// module, clearing the map — which is exactly why refreshing "fixed" it, and why it looked flaky rather than
// broken: it only bites when the new ask's ordinal matches one already answered.
//
// The process-scoped prefix makes collisions impossible ACROSS turns, conversations and restarts within a run,
// while keeping the `tc-<n>` shape every reader already expects. (Nothing parses the number; the shape is only
// ever compared for equality.)
var toolCallIDPrefix = "tc-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"

// toolCallSeq is the PROCESS-WIDE sequence behind every synthesized tool-call id.
var toolCallSeq atomic.Int64

// newToolCallID mints a unique id for a synthesized call. THE CALLER MUST HOLD l.mu.
func (l *toolLedger) newToolCallID() string {
	l.nextID++ // the ledger's own count, kept for anything that reads it
	return toolCallIDPrefix + strconv.FormatInt(toolCallSeq.Add(1), 10)
}

type toolCallEntry struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments    string `json:"arguments"`
	// IssuedAtUnixMs is the wall-clock time at which the call was ISSUED, in epoch milliseconds — the
	// same time encoding the chat path already uses (chat.ParsedTool.At).
	IssuedAtUnixMs int64 `json:"issued_at_unix_ms"`
	resolved       bool
}

// ledgerNowMillis is the ledger's clock seam: the epoch-millisecond time of a tool event.
var ledgerNowMillis = func() int64 { return time.Now().UnixMilli() }

type toolResultEntry struct {
	ToolCallID string `json:"tool_call_id"`
	Output     string `json:"output"`
	IsError    bool   `json:"is_error"`
}

func newToolLedger() *toolLedger { return &toolLedger{} }

// recordPermission appends one consent decision as a synthetic tool call + result pair, so the
// decision lands in the persisted transcript alongside the real tool calls (AC.
func (l *toolLedger) recordPermission(tool, target, verdict, detail, askID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id := strings.TrimSpace(askID)
	if id == "" {
		// A FALLBACK ID MUST NOT BE A PER-LEDGER COUNTER, and this one was. The ledger is created per
		// TURN, so every turn re-minted perm-1, perm-2 … for different asks — and the transcript ledger
		// is the SETTLE AUTHORITY, keyed by this id. A question in one turn could therefore inherit an
		// answer recorded in another: measured on one conversation, within a single 200-message window,
		//
		//	perm-16 questions=3 answers=1
		//	perm-15 questions=1 answers=1
		//	perm-14 questions=1 answers=1
		//
		// One id, several incompatible histories — the same collision that made permission cards flicker
		// (see orchicon.NativeBridge.nextPermID). The prefix is kept so the log still groups by it.
		//
		// Only the FALLBACK is affected: a record with a real ask id uses that id, unchanged.
		id = "perm-" + db.NewID()
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

// promoteRecordedAsk appends a FIRST-CLASS ask_user call when a tool result reports one. THE CALLER
// MUST HOLD l.mu.
func (l *toolLedger) promoteRecordedAsk(callID, output string) {
	args := promotedAskArgs(output)
	if args == "" {
		return
	}
	// THE ACKNOWLEDGEMENT IS NEVER THE ANSWER — including when the call is ALREADY the ask.
	if idx := l.callIndex(callID); idx >= 0 && isAskToolName(l.calls[idx].FunctionName) {
		l.setResultOutput(callID, AskAwaitingAnswerMarker)
		return
	}
	// A WRAPPED ask (opencode's `execute`) needs a first-class call of its own, because a client finds
	// an ask by NAME.
	for i := len(l.calls) - 1; i >= 0; i-- {
		if !isAskToolName(l.calls[i].FunctionName) {
			continue
		}
		if l.calls[i].Arguments == args {
			return
		}
	}
	// NOT `callID`: that is the parameter naming the WRAPPED call this promotion came from, and
	// reusing it here would silently attribute the await to the wrong call.
	promotedID := l.newToolCallID()
	l.calls = append(l.calls, toolCallEntry{
		ID:           promotedID,
		Type:         "function",
		FunctionName: "orchicon_ask_user",
		Arguments:    args,
		// The issue time is the RESULT's arrival: the wrapped call's start was the wrapper, not the
		// ask, and the card is anchored to when the question became askable.
		IssuedAtUnixMs: ledgerNowMillis(),
		resolved:       true,
	})
	// THE RESULT IS THE AWAITING MARKER: not the wrapper's output, and NOT NOTHING.
	l.results = append(l.results, toolResultEntry{
		ToolCallID: promotedID,
		Output:     AskAwaitingAnswerMarker,
	})
}

// callIndex returns the position of the call with this id, or -1. THE CALLER MUST HOLD l.mu.
func (l *toolLedger) callIndex(callID string) int {
	if callID == "" {
		return -1
	}
	for i := range l.calls {
		if l.calls[i].ID == callID {
			return i
		}
	}
	return -1
}

// setResultOutput rewrites the output of an existing result, or appends one when the call has none.
// THE CALLER MUST HOLD l.mu.
func (l *toolLedger) setResultOutput(callID, output string) {
	for i := range l.results {
		if l.results[i].ToolCallID == callID {
			l.results[i].Output = output
			return
		}
	}
	l.results = append(l.results, toolResultEntry{ToolCallID: callID, Output: output})
}

// isAskToolName matches every spelling the ask tool is recorded under, including the MCP-qualified
// ones.
//
// AGY'S SPELLING IS A THIRD SHAPE AND IT IS NOT A DECORATION: `mcp_<server>_<tool>` with SINGLE underscores —
// the server is `orchicon` and the tool is `ask_user`, so the card arrives as `mcp_orchicon_ask_user`
// (measured against agy 1.5.13; see internal/antigravity/agy_tool_spelling_test.go). That matches neither
// claude's `mcp__orchicon__ask_user` nor the bare `orchicon_ask_user`, and without this arm an antigravity
// conversation's ask card would be recorded as an ordinary tool call and never rendered.
func isAskToolName(name string) bool {
	switch name {
	case "ask_user", "orchicon_ask_user", "mcp__orchicon__ask_user", "mcp_orchicon_ask_user",
		// The doubled-prefix spelling is kept as a DEFENSIVE arm: it is not what agy 1.5.13 sends, but an
		// adapter or a future agy build that qualifies the tool with its own server-side `orchicon_`
		// prefix must still render a card rather than swallow it as an ordinary tool call.
		"mcp_orchicon_orchicon_ask_user":
		return true
	}
	// A server may also qualify it with a different separator; the suffix is the stable part.
	if strings.HasSuffix(name, "__ask_user") || strings.HasSuffix(name, "_ask_user") {
		return true
	}
	return name == "ask_user"
}

// recordStart logs a tool call ISSUED but not yet resolved (the tool_part signal: Text carries only
// the tool name).
func (l *toolLedger) recordStart(tool string) {
	if l == nil || tool == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, toolCallEntry{
		ID:             l.newToolCallID(),
		Type:           "function",
		FunctionName:   tool,
		Arguments:      "{}",
		IssuedAtUnixMs: ledgerNowMillis(), // THE issue-time stamp: this call just went out.
	})
}

// recordResolve logs a COMPLETED tool_use part: it backfills the arguments onto the matching
// in-flight call and appends the result.
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

	// AN ASK THAT RETURNED NOTHING IS NOT AN ANSWERED ASK — BUT IT IS STILL AN ASK, AND IT MUST KEEP
	// ITS CARD. The rule used to be `return`, which threw away the CALL along with the empty output.
	//
	// agy makes that the COMMON case, not an edge case: an MCP tool's result travels agy → MCP server →
	// model and never onto the ACP wire, so EVERY antigravity ask completes with an empty output
	// (docs/adapters/antigravity-acp.md §3.6; testdata/acp/mcp_tool_call.jsonl has no `rawOutput` at
	// all). Dropping the call meant an antigravity conversation's question never reached the transcript
	// at all — measured on conversation 01M8EAS38RC58SDK7RFYBFGMT8, where the model echoed
	// `{"recorded":true,"question":"Which colour do you prefer?"}` (the tool ran and returned) while
	// the row's tool_calls was null, so no client had anything to draw a card from.
	//
	// The INTENT — an empty output must never read as `answered with nothing` — is preserved by
	// recording the AWAITING MARKER instead of an empty string, which is exactly what
	// promoteRecordedAsk does for a recorded ask and what the clients already read as "still open".
	if isAskToolName(tool) && strings.TrimSpace(output) == "" {
		output = AskAwaitingAnswerMarker
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	callID := ""
	for i := len(l.calls) - 1; i >= 0; i-- {
		if l.calls[i].FunctionName == tool && !l.calls[i].resolved {
			// The issue time is deliberately NOT re-stamped here: this branch backfills arguments onto a call
			// that recordStart already stamped, and the timestamp taken at ISSUE is the one the window needs.
			l.calls[i].Arguments = argsJSON
			l.calls[i].resolved = true
			callID = l.calls[i].ID
			break
		}
	}
	if callID == "" {
		// Resolution without an observed start (re-attached mid-tool, or a start event missed): synthesize
		// the call so the result is kept.
		callID = l.newToolCallID()
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
	l.promoteRecordedAsk(callID, output)
	l.collapseAwaitingAsks()
}

// recordToolResolution backfills a resolved call's arguments and appends its result, from the TYPED
// adapter-neutral fields of a SessionEvent (Kind "tool_result").
func (l *toolLedger) recordToolResolution(tool, argsJSON, output string, isErr bool) {
	if l == nil || tool == "" {
		return
	}
	// AN ASK THAT RETURNED NOTHING IS NOT AN ANSWERED ASK — the typed-transport twin of the rule in
	// recordResolve, and the one claude actually takes. It keeps the CALL and records the awaiting
	// marker, because an empty output is agy's only shape for an MCP tool result and discarding the
	// call is what left an antigravity question with no card at all.
	if isAskToolName(tool) && strings.TrimSpace(output) == "" {
		output = AskAwaitingAnswerMarker
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	callID := ""
	// Match the OLDEST unresolved call for the same tool.
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
		// Resolution without an observed start (re-attached mid-tool, or a start event missed): synthesize
		// the call so the result is kept — the same rule recordResolve applies.
		callID = l.newToolCallID()
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
	l.promoteRecordedAsk(callID, output)
	l.collapseAwaitingAsks()
}

// snapshot returns the ledger as (tool_calls, tool_results) JSON documents ready for the message
// columns.
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

// repairedSnapshot returns the ledger's TERMINAL (tool_calls, tool_results) documents, with an
// explicit aborted result attached for every call that never resolved.
func (l *toolLedger) repairedSnapshot() (calls, results []byte) {
	calls, results = l.snapshot()
	return sanitizeAssistantToolLedger(calls, results)
}

// hasCalls reports whether the ledger recorded any tool call.
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

// collapseAwaitingAsks enforces ONE QUESTION = ONE CARD, however many times the transport announced it.
//
// THE PROBLEM IT SOLVES IS CREATED BY ALLOWING AN EMPTY ASK TO SURVIVE. An ask that completes with no
// output is now RECORDED (it used to be dropped, which is what left agy conversations with no card at
// all — see recordToolResolution). But a transport that announces the same ask more than once — claude
// sends a tool_part and then its completion; agy sends a tool_call and then a tool_call_update — would
// otherwise leave TWO awaiting calls holding the SAME question, and the clients draw one card per
// awaiting call. The operator would see the question twice and answering one would leave the other open
// for ever.
//
// So when several ask calls are awaiting and their arguments are the same question, only ONE is kept:
// the one that carries real arguments (not the `{}` placeholder recordStart writes), and among those the
// OLDEST, because its call id is the one a client has already been shown. The dropped duplicate's
// result is removed with it — a result without a call is repaired at turn end as "tool call aborted",
// which is precisely the message that killed cards before.
//
// THE CALLER MUST HOLD l.mu.
func (l *toolLedger) collapseAwaitingAsks() {
	awaiting := map[int]string{}
	question := map[string]bool{}
	for i := range l.calls {
		if !isAskToolName(l.calls[i].FunctionName) {
			continue
		}
		out, ok := l.outputFor(l.calls[i].ID)
		if !ok || !IsAwaitingAnswer(out) {
			continue
		}
		q := questionFingerprint(l.calls[i].Arguments)
		awaiting[i] = q
		question[q] = true
	}
	for q := range question {
		keep := -1
		for i, qq := range awaiting {
			if qq != q {
				continue
			}
			if keep == -1 {
				keep = i
				continue
			}
			// Prefer real arguments over the recordStart `{}` placeholder, then the OLDEST.
			cur, prev := l.calls[i].Arguments, l.calls[keep].Arguments
			if placeholderArgs(prev) && !placeholderArgs(cur) {
				keep = i
			}
		}
		for i, qq := range awaiting {
			if qq != q || i == keep {
				continue
			}
			l.dropCall(i)
		}
	}
}

// outputFor returns the recorded output of a call's result. THE CALLER MUST HOLD l.mu.
func (l *toolLedger) outputFor(callID string) (string, bool) {
	for i := range l.results {
		if l.results[i].ToolCallID == callID {
			return l.results[i].Output, true
		}
	}
	return "", false
}

// dropCall removes a call AND every result pointing at it, so no dangling tool_call_id is left for the
// turn-end repair to write off as aborted. THE CALLER MUST HOLD l.mu.
func (l *toolLedger) dropCall(idx int) {
	if idx < 0 || idx >= len(l.calls) {
		return
	}
	id := l.calls[idx].ID
	l.calls = append(l.calls[:idx], l.calls[idx+1:]...)
	kept := l.results[:0]
	for _, r := range l.results {
		if r.ToolCallID == id {
			continue
		}
		kept = append(kept, r)
	}
	l.results = kept
}

// questionFingerprint is the identity of the QUESTION an ask call carries, so two announcements of the
// same ask collapse while two genuinely different questions do not. It reads `question` out of the
// arguments; an unreadable or absent payload falls back to its own trimmed text, which keeps distinct
// calls distinct (never merging two questions the ledger could not read).
func questionFingerprint(argsJSON string) string {
	var in struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(argsJSON)), &in); err == nil {
		if q := strings.TrimSpace(in.Question); q != "" {
			return q
		}
	}
	return "raw:" + strings.TrimSpace(argsJSON)
}

// placeholderArgs reports whether a call's arguments are still the `{}` recordStart writes, i.e. the
// call was announced without its payload.
func placeholderArgs(argsJSON string) bool {
	s := strings.TrimSpace(argsJSON)
	return s == "" || s == "{}" || s == "null"
}
