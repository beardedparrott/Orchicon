package askorchicon

// ask_card_antigravity_test.go — THE CARD FOR AN agy ASK, from agy's OWN wire shape.
//
// MEASURED against agy 1.5.13 (internal/antigravity/testdata/acp/mcp_tool_call.jsonl:19-24): an MCP tool
// call reaches the ACP wire as `mcp_<server>_<tool>` with SINGLE underscores, and its COMPLETION carries
// NO OUTPUT AT ALL —
//
//	{"sessionUpdate":"tool_call_update","toolCallId":"call_…","name":"mcp_echo_echo",
//	 "status":"completed","kind":"other","title":"Run mcp_echo_echo","rawInput":{…}}
//
// There is no `content[]` and no `rawOutput` (the file contains zero `rawOutput` keys). The MCP result
// travels agy → MCP server → model, never onto the ACP wire, so `toolOutput()` correctly returns "".
//
// THAT EMPTY STRING IS WHAT KILLED THE CARD. The ledger's rule "an ask that returned nothing is not an
// answered ask" DROPS such a call entirely — the call, not just its result — so an antigravity
// conversation's ask_user never reached the transcript at all and no client could draw a card from it.
// Measured on this deployment: conversation 01M8EAS38RC58SDK7RFYBFGMT8 shows the model ECHOING
// `{"recorded":true,"question":"Which colour do you prefer?",…}` (so the tool ran and returned) while the
// row's `toolCalls` is null.
//
// The rule's INTENT is right and is preserved: an empty output must never read as "answered with
// nothing". It is achieved by recording the awaiting marker, not by discarding the call.

import (
	"strings"
	"testing"
)

// agyAskTool is the name agy puts on the wire for the platform's ask tool.
const agyAskTool = "mcp_orchicon_ask_user"

// agyAskArgs is the argument JSON an agy ask carries (agy passes the model's arguments through verbatim,
// plus its own `toolSummary`).
const agyAskArgs = `{"question":"Which colour do you prefer?","options":["red","blue"],` +
	`"allow_other":true,"toolSummary":"Ask which colour"}`

// askResults is every (callID → result output) pair the ledger holds.
func askResultsByID(l *toolLedger) map[string]string {
	out := map[string]string{}
	for _, r := range l.results {
		if _, ok := out[r.ToolCallID]; !ok {
			out[r.ToolCallID] = r.Output
		}
	}
	return out
}

// TestAgyAskSurvivesItsEmptyCompletion — the antigravity half of "no card appeared".
//
// The event order is the one the antigravity Ask mapper actually emits (internal/antigravity/ask_mapper.go):
// a `part{Type:"tool_use"}` when the call is first seen (state.input only — no output exists yet), then a
// typed `tool_result` on completion carrying the tool's output, which for agy's MCP tools is EMPTY.
func TestAgyAskSurvivesItsEmptyCompletion(t *testing.T) {
	l := newToolLedger()

	// 1. The call is issued: the mapper streams part{tool_use} with the input and NO output.
	l.recordResolve(map[string]any{
		"tool":   agyAskTool,
		"callID": "call_agy_1",
		"state":  map[string]any{"input": map[string]any{"question": "Which colour do you prefer?"}},
	})
	// 2. It completes with no output — agy's MCP shape.
	l.recordToolResolution(agyAskTool, agyAskArgs, "", false)

	if len(l.calls) == 0 {
		t.Fatal("the ledger recorded NO call for an agy ask. The ask tool ran (the model echoed its " +
			"{\"recorded\":true,…} result) but the call was dropped, so the transcript has nothing to " +
			"draw a card from — this is the reported `no card appears` for antigravity.")
	}

	// The call must survive with its arguments readable, or the card asks nothing.
	awaiting := 0
	var awaitingID string
	for id, output := range askResultsByID(l) {
		if IsAwaitingAnswer(output) {
			awaiting++
			awaitingID = id
		}
		if strings.TrimSpace(output) == "" {
			t.Errorf("ask call %s carries an EMPTY result — an empty result reads as `answered with "+
				"nothing`, which the clients render as `was dismissed — no answer sent` over a question "+
				"nobody answered", id)
		}
	}
	if awaiting != 1 {
		t.Fatalf("the ledger holds %d awaiting ask(s), want exactly 1 — the card is drawn on the awaiting "+
			"call, so any other count either hides it or draws two cards for one question", awaiting)
	}
	for _, c := range l.calls {
		if c.ID != awaitingID {
			continue
		}
		if !strings.Contains(c.Arguments, "Which colour do you prefer?") {
			t.Fatalf("the awaiting ask's arguments are not the question (%q) — the card would render blank",
				c.Arguments)
		}
	}
}

// TestAnAnsweredAgyAskIsNotAwaiting — the settle half: once the operator answers, the promoted-ask
// reply writes their words over the marker and the card must read as ANSWERED.
func TestAnAnsweredAgyAskIsNotAwaiting(t *testing.T) {
	l := newToolLedger()
	// The SAME ask, announced twice: the mapper streams it as a tool_use part and then reports its
	// completion. Both carry the same question, so they must collapse to one card.
	l.recordResolve(map[string]any{
		"tool":   agyAskTool,
		"callID": "call_agy_2",
		"state":  map[string]any{"input": map[string]any{"question": "Which colour do you prefer?"}},
	})
	l.recordToolResolution(agyAskTool, agyAskArgs, "", false)

	ids := askResultsByID(l)
	if len(ids) != 1 {
		t.Fatalf("want exactly one ask call, got %d", len(ids))
	}
	for id, out := range ids {
		if !IsAwaitingAnswer(out) {
			t.Fatalf("a freshly asked agy ask is not awaiting (output %q) — no card would be drawn", out)
		}
		// The operator answers: this is what recordPromotedAnswer writes.
		l.setResultOutput(id, "blue")
		if IsAwaitingAnswer(l.results[0].Output) {
			t.Error("the answered ask still reads as awaiting — the card would stay open for ever")
		}
	}
}

// TestAClaudePhantomStillCannotSettleTheCard — the invariant the drop rule existed to protect, kept:
// claude announces an ask TWICE (a tool_part, then its completion). Whatever the ledger does with the
// first, the card must land on an AWAITING call and never on an empty result.
func TestAClaudePhantomStillCannotSettleTheCard(t *testing.T) {
	l := newToolLedger()
	l.recordStart("mcp__orchicon__ask_user")
	l.recordToolResolution("mcp__orchicon__ask_user", agyAskArgs, "", false) // the empty announcement
	l.recordStart("mcp__orchicon__ask_user")                                 // re-announced
	l.recordToolResolution("mcp__orchicon__ask_user", agyAskArgs,
		`{"recorded":true,"question":"Which colour do you prefer?","options":["red","blue"]}`, false)

	awaiting := 0
	for id, output := range askResultsByID(l) {
		if strings.TrimSpace(output) == "" {
			t.Errorf("ask call %s carries an EMPTY result — the phantom that hid the claude card", id)
		}
		if IsAwaitingAnswer(output) {
			awaiting++
		}
	}
	if awaiting == 0 {
		t.Fatal("no claude ask is awaiting — the card would never be drawn")
	}
	// ONE QUESTION IS ONE CARD. Two awaiting calls for the same question would draw it twice.
	if awaiting > 1 {
		t.Errorf("%d claude ask calls are awaiting one question — that draws the card more than once", awaiting)
	}
}
