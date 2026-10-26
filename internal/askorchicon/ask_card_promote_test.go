package askorchicon

// ask_card_promote_test.go — an ask must be recognised from its RESULT, whatever the caller's name.
//
// REPORTED: "I tried an ask conversation with both opencode and claude adapters. Both say they sent a
// card, but no actual card appeared. I know ask cards used to work."
//
// Both halves of that are true and they are consistent: nothing in this pipeline regressed — the
// adapters' CALLING STYLE did. Both clients render the card from the TRANSCRIPT, matching a recorded
// tool call named ask_user/orchicon_ask_user. The MCP-served adapters never record that name:
//
//	opencode  function_name "execute", the call inside a JavaScript body
//	claude    function_name "mcp__orchicon__ask_user", which the predicate did not accept
//
// The payloads below are COPIED FROM THIS DEPLOYMENT'S OWN TRANSCRIPT TABLE, which is what makes these
// a regression test rather than a restatement of the parser.

import (
	"encoding/json"
	"strings"
	"testing"
)

// The exact result opencode's sandbox returned for `tools.orchicon.ask_user(...)`.
const opencodeExecuteResult = `[{"text":"{\n  \"recorded\": true,\n  \"question\": \"Ask-card test: which answer should I treat as the passing result for this smoke test?\",\n  \"options\": [\n    {\n      \"label\": \"Green — it works\",\n      \"description\": \"The ask card rendered and recorded the answer correctly.\"\n    },\n    {\n      \"label\": \"Amber — partial\",\n      \"description\": \"The card appeared but something in the flow felt off.\"\n    }\n  ],\n  \"allow_other\": true\n}","type":"text"}]`

func TestARecordedAskIsPromotedFromItsResult(t *testing.T) {
	args := promotedAskArgs(opencodeExecuteResult)
	if args == "" {
		t.Fatal("promotedAskArgs found no ask in the opencode execute result.\n" +
			"  That result is the tool's own acknowledgement — recorded:true with the question and its\n" +
			"  options — and it is the ONLY structure that survives every adapter's wrapping. Missing it\n" +
			"  is the reported bug: the model is told a card was shown and no card exists.")
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
	if len(got.Options) != 2 {
		t.Errorf("options = %d, want 2 — the card renders exactly what the tool returned", len(got.Options))
	}
	if !got.AllowOther {
		t.Error("allow_other was dropped; the free-text row would not render")
	}
	// THE WRAPPER'S KEYS MUST NOT LEAK: only the card's own fields travel, so nothing the model wrote
	// outside the tool's contract reaches the rendered card.
	//
	// Checked as PARSED KEYS rather than substrings — the question text legitimately contains the
	// word "recorded", which a substring test flags (measured: it did).
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

// TestOnlyARecordedAskIsPromoted — the detection must not fire on a mere MENTION of the tool, or a
// model quoting the description would draw a phantom card.
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

// TestPromotionKeepsOneCardForADirectCall — a native (direct) ask_user call already renders; promoting
// it too would draw the same question twice.
func TestPromotionKeepsOneCardForADirectCall(t *testing.T) {
	l := &toolLedger{}
	l.recordStart("ask_user")
	l.recordResolve(map[string]any{
		"tool":  "ask_user",
		"state": map[string]any{"status": "completed", "input": map[string]any{"question": "Pick one"}, "output": `{"recorded":true,"question":"Pick one","options":[]}`},
	})
	asks := 0
	for _, c := range l.calls {
		if isAskToolName(c.FunctionName) {
			asks++
		}
	}
	if asks != 1 {
		t.Errorf("recorded %d ask calls for one direct ask_user invocation, want 1 — a promoted "+
			"duplicate draws a second card for the same question", asks)
	}
}

// TestAWrappedAskBecomesItsOwnRecordedCall — the end-to-end shape: an `execute` call that carries an
// ask yields TWO transcript entries — the wrapper (what the adapter really did) and the ask (what the
// clients render), with no result on the ask so the card is LIVE.
func TestAWrappedAskBecomesItsOwnRecordedCall(t *testing.T) {
	l := &toolLedger{}
	l.recordStart("execute")
	l.recordResolve(map[string]any{
		"tool":  "execute",
		"state": map[string]any{"status": "completed", "input": map[string]any{"code": "await tools.orchicon.ask_user({...})"}, "output": opencodeExecuteResult},
	})

	var wrapper, ask *toolCallEntry
	for i := range l.calls {
		switch {
		case l.calls[i].FunctionName == "execute":
			wrapper = &l.calls[i]
		case isAskToolName(l.calls[i].FunctionName):
			ask = &l.calls[i]
		}
	}
	if wrapper == nil {
		t.Fatal("the wrapper call was dropped — the transcript must still show what the adapter did")
	}
	if ask == nil {
		t.Fatal("no ask call was recorded for a wrapped ask — no card can render")
	}
	// THE CARD MUST BE OPEN. A result on the ask id would render it already answered, with the tool's
	// own JSON as the operator's words.
	for _, r := range l.results {
		if r.ToolCallID == ask.ID {
			t.Error("the promoted ask carries a result, so the card renders AS ANSWERED — it must stay " +
				"open until the operator replies")
		}
	}
	if wrapper.ID == ask.ID {
		t.Error("the promoted ask reused the wrapper's id")
	}
}

// TestIsAskToolNameCoversTheMCPSpelling — claude records the qualified name, and that was half of the
// report.
func TestIsAskToolNameCoversTheMCPSpelling(t *testing.T) {
	for _, name := range []string{"ask_user", "orchicon_ask_user", "mcp__orchicon__ask_user", "mcp__other__ask_user"} {
		if !isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = false — an MCP-served ask is recorded under its qualified "+
				"name, so this is invisible without it", name)
		}
	}
	for _, name := range []string{"execute", "ToolSearch", "read", "", "ask_user_extra"} {
		if isAskToolName(name) {
			t.Errorf("isAskToolName(%q) = true — an ordinary tool must not be treated as an ask", name)
		}
	}
}
