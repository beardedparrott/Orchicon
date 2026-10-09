package chat

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// settled_asks_test.go — THE SETTLE HALF, READ FROM THE SERVER'S OWN RECORD.
//
// An OPEN card has no durable row: the transcript records the OUTCOME of a decision
// (permission.allow_once / .deny / .expired), never the open ask. That asymmetry is why a client
// cannot settle a card by looking for it — it has to look for what HAPPENED to it, which is what
// these rows are. The GUI reads the same rows (lib/ask-consent.ts settleFromLedger); the TUI reads
// them here, so neither client has to guess from its own socket state (the guess that swallowed a
// live card — see chatStore.settleStaleConsent's call site in onStreamDone).

func pageWith(calls ...*apiv1.ToolCall) []*apiv1.ChatMessage {
	return []*apiv1.ChatMessage{{ToolCalls: calls}}
}

func TestSettledAsksFromPageReadsOnlyOutcomeRows(t *testing.T) {
	page := pageWith(
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.allow_once"},
		&apiv1.ToolCall{Id: "ask-2", FunctionName: "permission.deny"},
		// Not an outcome for an ask: an open question, a tally row, a tool call.
		&apiv1.ToolCall{Id: "ask-3", FunctionName: "permission.ask"},
		&apiv1.ToolCall{Id: "ask-4", FunctionName: "ask_user"},
		&apiv1.ToolCall{Id: "tc-9", FunctionName: "read"},
	)
	got := settledAsksFromPage(page)
	if len(got) != 3 {
		t.Fatalf("want the three permission.<outcome> rows, got %+v", got)
	}
	// The ASK ID is the row's id — the same key the card holds, which is what makes the settle land.
	want := map[string]string{"ask-1": "allow_once", "ask-2": "deny", "ask-3": "ask"}
	for _, o := range got {
		if want[o.AskID] != o.Outcome {
			t.Errorf("ask %q: outcome %q, want %q", o.AskID, o.Outcome, want[o.AskID])
		}
	}
}

// A LATER RECORD MUST NOT OVERWRITE WHAT ACTUALLY HAPPENED — mirrors the GUI's "first resolution
// wins" (resolveAsk). A second row for the same ask is a duplicate, not a revision.
func TestSettledAsksFromPageFirstResolutionWins(t *testing.T) {
	page := pageWith(
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.allow_once"},
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.deny"},
	)
	got := settledAsksFromPage(page)
	if len(got) != 1 {
		t.Fatalf("want one outcome for ask-1, got %+v", got)
	}
	if got[0].Outcome != "allow_once" {
		t.Fatalf("the FIRST resolution must win, got %q", got[0].Outcome)
	}
}

// A malformed row must never retire a live card: no id, no outcome, no row.
func TestSettledAsksFromPageIgnoresMalformedRows(t *testing.T) {
	page := pageWith(
		&apiv1.ToolCall{Id: "", FunctionName: "permission.allow_once"},
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission."},
		nil,
	)
	page = append(page, (*apiv1.ChatMessage)(nil))
	if got := settledAsksFromPage(page); len(got) != 0 {
		t.Fatalf("want no outcomes from malformed rows, got %+v", got)
	}
	if got := settledAsksFromPage(nil); len(got) != 0 {
		t.Fatalf("an empty page settles nothing, got %+v", got)
	}
}
