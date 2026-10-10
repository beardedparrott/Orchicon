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

func TestSettledAsksFromPageReadsOnlyResolutionRows(t *testing.T) {
	page := pageWith(
		// The enum spelling the operator's own click records (consentTurn.record -> verdict
		// `user_` + the choice enum), which is what the GUI matches too.
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.user_PERMISSION_CHOICE_ALLOW_ONCE"},
		&apiv1.ToolCall{Id: "ask-2", FunctionName: "permission.user_PERMISSION_CHOICE_DENY"},
		&apiv1.ToolCall{Id: "ask-3", FunctionName: "permission.expired"},
		// Not resolutions: a tool call and an ask_user card.
		&apiv1.ToolCall{Id: "ask-4", FunctionName: "ask_user"},
		&apiv1.ToolCall{Id: "tc-9", FunctionName: "read"},
	)
	got := settledAsksFromPage(page)
	if len(got) != 3 {
		t.Fatalf("want the three resolution rows, got %+v", got)
	}
	// The ASK ID is the row's id — the same key the card holds, which is what makes the settle land.
	want := map[string]string{"ask-1": "allow_once", "ask-2": "deny", "ask-3": "expired"}
	for _, o := range got {
		if want[o.AskID] != o.Outcome {
			t.Errorf("ask %q: outcome %q, want %q", o.AskID, o.Outcome, want[o.AskID])
		}
	}
}

// THE REGRESSION, AND IT SHIPPED ONCE. An ask being RAISED is recorded on the ledger as
// `permission.ask` — and a question as `permission.question` — by the very code that raises the card,
// carrying the CARD'S OWN ASK ID. The first version of this matched any `permission.` prefix and took
// the suffix as an outcome, so it read a raise as a resolution: the next transcript load settled the
// card the instant it was drawn, `replace` dropped a settled card, and the periodic discovery re-armed
// it — a card that appeared and vanished on a ~2s cycle, unclickable. The operator: "permission cards
// are popping up and then going away almost immediately before I can click on them and it seems to
// rotate every few seconds."
//
// This test is the one that should have caught it: the version before this one asserted the OPPOSITE,
// listing `permission.ask` among the expected outcomes, which is how a defect becomes the expected
// behaviour. The comment there called it "an open question" while the assertion said it settles.
func TestRaiseRecordsAreNotResolutions(t *testing.T) {
	for _, raise := range []string{"permission.ask", "permission.question"} {
		page := pageWith(&apiv1.ToolCall{Id: "ask-1", FunctionName: raise})
		if got := settledAsksFromPage(page); len(got) != 0 {
			t.Errorf("%s is the RAISE record (it carries the open card's own id) — reading it as a "+
				"resolution settles every card the moment it is drawn, got %+v", raise, got)
		}
	}
}

// An UNRECOGNISED resolution settles nothing. Under-settling is safe (the card can still be answered,
// and turn end settles it); over-settling removes a click the operator needed to make.
func TestAnUnknownPermissionRowSettlesNothing(t *testing.T) {
	page := pageWith(&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.zzz_unknown_verdict"})
	if got := settledAsksFromPage(page); len(got) != 0 {
		t.Fatalf("an unknown verdict must not settle a card, got %+v", got)
	}
}

// A LATER RECORD MUST NOT OVERWRITE WHAT ACTUALLY HAPPENED — mirrors the GUI's "first resolution
// wins" (resolveAsk). A second row for the same ask is a duplicate, not a revision.
func TestSettledAsksFromPageFirstResolutionWins(t *testing.T) {
	page := pageWith(
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.user_PERMISSION_CHOICE_ALLOW_ONCE"},
		&apiv1.ToolCall{Id: "ask-1", FunctionName: "permission.user_PERMISSION_CHOICE_DENY"},
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
