package askorchicon

// ask_card_claude_test.go — claude's two failures, pinned on the REAL rows this deployment produced.
//
// REPORTED after the previous round shipped: "Ask/permissions cards are still not working for opencode
// and claude code adapters … it never actually pops up for Claude adapter."
//
// That was true, and the transcript showed why:
//
//	tc-1  ToolSearch  output ""
//	tc-2  ToolSearch  output ""
//	tc-3  ask_user    output ""                          ← the card read THIS one
//	tc-4  ask_user    output {"recorded":true,…}          ← the real result
//
// TWO defects, both of which have to be fixed for the card to appear at all:
//
//  1. claude announces the ask twice — a tool_part and then its completion — and the first announcement
//     was recorded with an EMPTY output and marked resolved. The real result then had no unresolved call
//     to land on, so it synthesized a DUPLICATE ask and left the first holding an empty answer. Every
//     client reads a result as an answer, so the phantom settled the card before it was ever drawn.
//
//  2. A direct MCP call needs no PROMOTION, and the promotion function returned early for exactly that
//     case — leaving the tool's own `{"recorded":true,…}` acknowledgement in the RESULT slot. The
//     acknowledgement is not the operator's answer; treating it as one draws the card settled from its
//     first frame, with the tool's JSON printed as the reply.
//
// The payloads below are copied verbatim from this deployment's own ask_orchicon_messages rows for the
// claude conversation, which is what makes this a regression test rather than a restatement of the code.

import (
	"encoding/json"
	"strings"
	"testing"
)

// claudeAskArgs is what claude recorded as the call's arguments — the question and its options.
const claudeAskArgs = `{"allow_other":true,"options":[{"description":"The question and options appeared as a card you could pick from.","label":"Card rendered correctly"},{"description":"Something looked wrong, such as missing options or layout problems.","label":"Rendered, but with issues"}],"question":"This is a test of the ask card. Which answer best describes how the card looks and behaves for you?"}`

// claudeRecordedAskResult is the tool's own acknowledgement, exactly as claude's adapter recorded it.
const claudeRecordedAskResult = `{"recorded":true,"question":"This is a test of the ask card. Which answer best describes how the card looks and behaves for you?","options":[{"label":"Card rendered correctly","description":"The question and options appeared as a card you could pick from."},{"label":"Rendered, but with issues","description":"Something looked wrong, such as missing options or layout problems."}],"allow_other":true,"note":"The question is recorded and shown to the user as a card. END YOUR TURN NOW — the user answers in their next message. Do not guess the answer and do not continue working."}`

// askResultsFor returns every recorded result belonging to an ask-named call.
func askResultsFor(l *toolLedger) map[string]string {
	out := map[string]string{}
	ask := map[string]bool{}
	for _, c := range l.calls {
		if isAskToolName(c.FunctionName) {
			ask[c.ID] = true
		}
	}
	for _, r := range l.results {
		if ask[r.ToolCallID] {
			out[r.ToolCallID] = r.Output
		}
	}
	return out
}

// TestADirectMCPServedAskIsAnsweredByNothing — defect 2: the acknowledgement must never be the answer.
//
// claude calls the tool by its MCP-qualified name, so no promotion is needed and none happens. What must
// happen instead is that the acknowledgement is replaced, in place, by the awaiting marker — on the SAME
// id the operator's answer will settle.
func TestADirectMCPServedAskIsAnsweredByNothing(t *testing.T) {
	l := newToolLedger()
	l.recordStart("mcp__orchicon__ask_user")
	l.recordToolResolution("mcp__orchicon__ask_user", claudeAskArgs, claudeRecordedAskResult, false)

	asks := 0
	for _, c := range l.calls {
		if isAskToolName(c.FunctionName) {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("%d ask calls recorded, want exactly 1 — a direct MCP ask must not be promoted as well", asks)
	}

	results := askResultsFor(l)
	if len(results) != 1 {
		t.Fatalf("%d results on ask calls, want 1: %+v", len(results), results)
	}
	for id, output := range results {
		if !IsAwaitingAnswer(output) {
			t.Fatalf("the ask's result is %q, want the awaiting marker.\n"+
				"  That result is the tool's own {\"recorded\":true,…} acknowledgement. Every client reads a\n"+
				"  result as the operator's ANSWER, so leaving it here draws the card settled from its first\n"+
				"  frame and it never appears — the reported \"it never actually pops up for Claude adapter\".", output)
		}
		if id == "" {
			t.Fatal("the awaiting marker is attached to an empty call id")
		}
	}
}

// TestAnEmptyAskResultIsNotAResolution — defect 1: the phantom call, and the duplicate it creates.
//
// claude emits the ask's completion part with an EMPTY output before the real one. Recording that as a
// resolution both settled the ask with no answer and pushed the real result onto a second, synthesized
// call. This replays the observed sequence.
func TestAnEmptyAskResultIsNotAResolution(t *testing.T) {
	l := newToolLedger()
	l.recordStart("mcp__orchicon__ask_user")
	l.recordToolResolution("mcp__orchicon__ask_user", claudeAskArgs, "", false) // the empty completion
	l.recordStart("mcp__orchicon__ask_user")                                   // re-announced
	l.recordToolResolution("mcp__orchicon__ask_user", claudeAskArgs, claudeRecordedAskResult, false)

	// NO ask call may hold an empty result: that is the phantom the card used to read, and an empty
	// result reads as "answered with nothing" — which the client prints as
	// `Question "…" was dismissed — no answer sent`, over a question nobody was ever shown.
	for id, output := range askResultsFor(l) {
		if output == "" {
			t.Errorf("ask call %s carries an EMPTY result — that is the phantom that hid the card", id)
		}
		_ = id
	}

	// And exactly one ask call must be AWAITING: the card the operator sees, and the id their answer
	// settles (findPromotedAsk matches on it, so a card drawn on any other id can never be answered).
	awaiting := ""
	for id, output := range askResultsFor(l) {
		if IsAwaitingAnswer(output) {
			if awaiting != "" {
				t.Fatalf("two ask calls are awaiting (%s and %s) — one question cannot be two cards", awaiting, id)
			}
			awaiting = id
		}
	}
	if awaiting == "" {
		t.Fatal("no ask call is awaiting an answer — the card would never be drawn")
	}

	// The awaiting call must carry the REAL question, or the card asks nothing.
	for _, c := range l.calls {
		if c.ID != awaiting {
			continue
		}
		if c.Arguments == "" || c.Arguments == "{}" {
			t.Fatalf("the awaiting ask carries no arguments (%q) — the card would render without a question",
				c.Arguments)
		}
		if !strings.Contains(c.Arguments, "This is a test of the ask card") {
			t.Fatalf("the awaiting ask's arguments are not the question: %s", c.Arguments)
		}
	}
}

// TestTheRepairDoesNotKillADirectMCPAsk — the turn-end repair must leave the awaiting marker alone.
//
// The repair appends "tool call aborted" to every unresolved call. A promoted ask survives it by
// carrying the marker; a DIRECT MCP ask had no such protection before this fix, because its result was
// the acknowledgement and the repair never ran. Pinned so the two shapes stay equally safe.
func TestTheRepairDoesNotKillADirectMCPAsk(t *testing.T) {
	l := newToolLedger()
	l.recordStart("mcp__orchicon__ask_user")
	l.recordToolResolution("mcp__orchicon__ask_user", claudeAskArgs, claudeRecordedAskResult, false)

	callsJSON, resultsJSON := l.snapshot()
	calls, repaired := sanitizeAssistantToolLedger(callsJSON, resultsJSON)
	var kept []askToolCallJSON
	if err := json.Unmarshal(calls, &kept); err != nil {
		t.Fatalf("calls are not JSON: %v — %s", err, calls)
	}
	if got := len(kept); got != 1 {
		t.Fatalf("the repair kept %d calls, want 1: %s", got, calls)
	}
	var out []askToolResultJSON
	if err := json.Unmarshal(repaired, &out); err != nil {
		t.Fatalf("results are not JSON: %v — %s", err, repaired)
	}
	if len(out) != 1 {
		t.Fatalf("the repair wrote %d results, want 1: %s", len(out), repaired)
	}
	if !IsAwaitingAnswer(out[0].Output) {
		t.Fatalf("after repair the result is %q, want the awaiting marker — the card is killed at turn end",
			out[0].Output)
	}
	if out[0].IsError {
		t.Error("the awaiting marker is recorded as an ERROR, which draws it as a failure")
	}
}
