package askorchicon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// ask_user durability: the recorded call is a DURABLE TRANSCRIPT ROW, so it
// survives compaction and resume and the clients can render the card from
// history rather than from a live-only signal (AC: "The call is recorded in the
// durable transcript with its question and options intact, and survives
// compaction and resume").
//
// Compaction rewrites ADAPTER state, not DB rows (conversation_compact.go:
// "it rewrites adapter state, not DB rows"), and db.ListMessages has no
// compaction filter — so the row itself is never rewritten by a compaction. The
// one transform that DOES touch the row's tool ledger on its way into the next
// turn is sanitizeHistoryRows (chat.go), which repairs call/result pairing on
// the history window every turn replays. That is therefore exactly where a
// recorded clarifying question could be silently lost, and this test pins it.

type persistedToolCall struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	FunctionName string `json:"function_name"`
	Arguments    string `json:"arguments"`
}

// TestAskUserRecordedCallSurvivesHistoryRepair drives the REAL history repair
// over a persisted assistant row carrying a recorded ask_user call (plus a
// second, dangling call), and asserts the clarifying question comes out the
// other side intact and re-parseable — the state the card is rebuilt from.
func TestAskUserRecordedCallSurvivesHistoryRepair(t *testing.T) {
	// The arguments exactly as the tool handler echoes them back and the live
	// ledger persists them.
	args := `{"question":"Which branch should the run clone off?",` +
		`"options":[{"label":"develop","description":"the integration branch"},{"label":"main"}],` +
		`"allow_other":true}`
	callsJSON, err := json.Marshal([]persistedToolCall{
		{ID: "tc-1", Type: "function", FunctionName: "orchicon_ask_user", Arguments: args},
		// A second call whose result never arrived — the interrupted-turn shape
		// the repair exists for, so the repair is genuinely exercised here.
		{ID: "tc-2", Type: "function", FunctionName: "bash", Arguments: `{"cmd":"git status"}`},
	})
	if err != nil {
		t.Fatalf("marshal fixture calls: %v", err)
	}
	resultsJSON := []byte(`[{"tool_call_id":"tc-1","output":"{\"recorded\":true}","is_error":false}]`)

	history := []db.MessageRow{
		{ID: "u1", Role: "user", Content: "cut me a branch", ToolCalls: []byte("[]"), ToolResults: []byte("[]")},
		{ID: "a1", Role: "assistant", Content: "Which branch?", ToolCalls: callsJSON, ToolResults: resultsJSON},
	}

	repaired := sanitizeHistoryRows(history)
	if len(repaired) != 2 {
		t.Fatalf("repair changed the row count: %d", len(repaired))
	}
	row := repaired[1]

	// The repaired ledger is well-formed, so the NEXT turn can replay it (the
	// resume half of the AC): every call has a matching result.
	assertLedgerWellFormed(t, row.ToolCalls, row.ToolResults)

	var calls []persistedToolCall
	if err := json.Unmarshal(row.ToolCalls, &calls); err != nil {
		t.Fatalf("decoded tool_calls: %v (%s)", err, row.ToolCalls)
	}
	var ask *persistedToolCall
	for i := range calls {
		if calls[i].FunctionName == "orchicon_ask_user" {
			ask = &calls[i]
		}
	}
	if ask == nil {
		t.Fatalf("the recorded ask_user call did not survive history repair: %s", row.ToolCalls)
	}
	if strings.TrimSpace(ask.Arguments) == "" || ask.Arguments == "{}" {
		t.Fatalf("the recorded ask_user arguments were emptied by the repair: %q", ask.Arguments)
	}

	// The question and every option are still there, and still parse with the
	// SAME validation the handler applies — i.e. the persisted row alone is
	// enough to rebuild the card the operator answers.
	question, options, allowOther, perr := parseAskUserArgs([]byte(ask.Arguments))
	if perr != nil {
		t.Fatalf("the persisted arguments no longer parse (%v): %q", perr, ask.Arguments)
	}
	if !strings.Contains(question, "clone off") {
		t.Errorf("question = %q, want it intact in the durable row", question)
	}
	if len(options) != 2 || options[0].Label != "develop" || options[1].Label != "main" {
		t.Errorf("options = %+v, want develop + main intact", options)
	}
	if options[0].Description != "the integration branch" {
		t.Errorf("option description = %q, want it intact", options[0].Description)
	}
	if !allowOther {
		t.Error("allow_other was lost — a free-text answer would no longer be offered")
	}
}
