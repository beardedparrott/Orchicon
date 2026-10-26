package askorchicon

// ask_card_promote_test.go — an ask is recognised from its RESULT, and it survives the turn-end repair.
//
// REPORTED across three rounds: "Both say they sent a card, but no actual card appeared" then "the card
// goes away before I can click on it … tool call aborted" then "that card in the GUI never went away
// either". Each round is pinned here, because each was a real failure of this pipeline.
//
// The payloads are COPIED FROM THIS DEPLOYMENT'S OWN TRANSCRIPT TABLE, which is what makes these a
// regression test rather than a restatement of the parser.

import (
	"encoding/json"
	"strings"
	"testing"
)

// The exact result opencode's sandbox returned for `tools.orchicon.ask_user(...)`.
const opencodeExecuteResult = `[{"text":"{\n  \"recorded\": true,\n  \"question\": \"Ask-card test: which answer should I treat as the passing result for this smoke test?\",\n  \"options\": [\n    {\n      \"label\": \"Green — it works\",\n      \"description\": \"The ask card rendered and recorded the answer correctly.\"\n    },\n    {\n      \"label\": \"Amber — partial\",\n      \"description\": \"The card appeared but something in the flow felt off.\"\n    }\n  ],\n  \"allow_other\": true\n}","type":"text"}]`

// TestARecordedAskIsPromotedFromItsResult — round one: the card must EXIST.
func TestARecordedAskIsPromotedFromItsResult(t *testing.T) {
	args := promotedAskArgs(opencodeExecuteResult)
	if args == "" {
		t.Fatal("promotedAskArgs found no ask in the opencode execute result.\n" +
			"  That result is the tool's own acknowledgement — recorded:true with the question and its\n" +
			"  options — and it is the only structure that survives every adapter's wrapping.")
	}
	var got struct {
		Question   string            `json:"question"`
		Options    []json.RawMessage `json:"options"`
		AllowOther bool              `json:"allow_other"`
	}
	if err := json.Unmarshal([]byte(args), &got); err != nil {
		t.Fatalf("the promoted arguments are not JSON (%v): %s", err, args)
	}
	if !strings.Contains(got.Question, "which answer should I treat") {
		t.Errorf("question = %q, want the one the tool recorded", got.Question)
	}
	if len(got.Options) != 2 || !got.AllowOther {
		t.Errorf("options=%d allow_other=%v, want 2 and true — the card renders what the tool returned",
			len(got.Options), got.AllowOther)
	}
	// THE WRAPPER'S KEYS MUST NOT LEAK — asserted on PARSED KEYS, not substrings: the question text
	// legitimately contains the word "recorded", which a substring check flagged when this was written.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &keys); err != nil {
		t.Fatalf("promoted arguments are not a JSON object: %v", err)
	}
	for k := range keys {
		switch k {
		case "question", "options", "allow_other":
		default:
			t.Errorf("the promoted arguments carry %q, which is not a card field", k)
		}
	}
}

// TestOnlyARecordedAskIsPromoted — a mere MENTION of the tool must not draw a phantom card.
func TestOnlyARecordedAskIsPromoted(t *testing.T) {
	for _, output := range []string{
		`[{"text":"Unknown tool 'orchicon_ask_user'. Did you mean tools.opencode.list_mcp_resources?","type":"text"}]`,
		`{"items":[{"path":"tools.orchicon.ask_user","description":"Ask the user a clarifying question"}]}`,
		`{"recorded": false, "question": "x"}`,
		`{"recorded": true}`,
		`{"recorded": true, "question": "   "}`,
		`not json at all`,
		``,
	} {
		if args := promotedAskArgs(output); args != "" {
			t.Errorf("promotedAskArgs(%q) = %q, want empty — only a genuine recorded ask may draw a card",
				output, args)
		}
	}
}

// TestAPromotedAskSurvivesTheLedgerRepair — round two: the card must not VANISH at turn end.
//
// The card existed; sanitizeAssistantToolLedger appended "tool call aborted — the turn ended before this
// tool returned a result" to it, and the clients settle a card as soon as its call HAS a result. The
// marker is why the repair has nothing to add.
func TestAPromotedAskSurvivesTheLedgerRepair(t *testing.T) {
	l := &toolLedger{}
	l.recordStart("execute")
	l.recordResolve(map[string]any{
		"tool":  "execute",
		"state": map[string]any{"status": "completed", "input": map[string]any{}, "output": opencodeExecuteResult},
	})

	var ask *toolCallEntry
	for i := range l.calls {
		if isAskToolName(l.calls[i].FunctionName) {
			ask = &l.calls[i]
		}
	}
	if ask == nil {
		t.Fatal("no promoted ask call")
	}
	awaiting := false
	for _, r := range l.results {
		if r.ToolCallID == ask.ID && IsAwaitingAnswer(r.Output) {
			awaiting = true
		}
	}
	if !awaiting {
		t.Fatalf("the promoted ask carries no AWAITING result: %+v\n"+
			"  Without it the turn-end repair writes \"tool call aborted\" and the card vanishes.", l.results)
	}

	callsJSON, _ := json.Marshal(l.calls)
	resultsJSON, _ := json.Marshal(l.results)
	_, repaired := sanitizeAssistantToolLedger(callsJSON, resultsJSON)
	var got []askToolResultJSON
	if err := json.Unmarshal(repaired, &got); err != nil {
		t.Fatalf("results are not JSON: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after repair there are %d results, want 2 (wrapper + awaiting marker): %s", len(got), repaired)
	}
	for _, r := range got {
		if r.Output == abortedToolResultOutput {
			t.Errorf("the repair wrote %q over the ledger — the card is killed at turn end", abortedToolResultOutput)
		}
	}
}

// TestTheAwaitingMarkerIsNotAnAnswer — the clients' rule, pinned on the server side so the two ends
// cannot drift.
func TestTheAwaitingMarkerIsNotAnAnswer(t *testing.T) {
	if !IsAwaitingAnswer(AskAwaitingAnswerMarker) {
		t.Fatal("the marker does not identify itself")
	}
	for _, out := range []string{"Green", "", "tool call aborted — the turn ended"} {
		if IsAwaitingAnswer(out) {
			t.Errorf("IsAwaitingAnswer(%q) = true, want false", out)
		}
	}
}

// TestIsAskToolNameCoversTheMCPSpelling — half of the claude report: a direct MCP call records the
// QUALIFIED name, which neither client's predicate accepted.
func TestIsAskToolNameCoversTheMCPSpelling(t *testing.T) {
	for _, name := range []string{"ask_user", "orchicon_ask_user", "mcp__orchicon__ask_user", "mcp__other__ask_user"} {
		if !isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = false — an MCP-served ask is recorded under its qualified name", name)
		}
	}
	for _, name := range []string{"execute", "ToolSearch", "read", "", "ask_user_extra"} {
		if isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = true — an ordinary tool must not be treated as an ask", name)
		}
	}
}

// TestADirectAskIsNotPromotedTwice — the native bridge calls the tool bare, so its call already renders;
// promoting it too would draw the same question twice.
func TestADirectAskIsNotPromotedTwice(t *testing.T) {
	l := &toolLedger{}
	l.recordStart("ask_user")
	l.recordResolve(map[string]any{
		"tool":  "ask_user",
		"state": map[string]any{"status": "completed", "input": map[string]any{"question": "Pick one"}, "output": `{"recorded":true,"question":"Pick one","options":[]}`},
	})
	n := 0
	for _, c := range l.calls {
		if isAskToolName(c.FunctionName) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("recorded %d ask calls for one direct invocation, want 1", n)
	}
}
