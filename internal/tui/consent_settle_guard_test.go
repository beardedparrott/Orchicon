package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// consent_settle_guard_test.go — A CARD MAY ONLY BE SETTLED BY SOMETHING THE SERVER SAID.
//
// The reported loss: "a permissions card in the TUI get swallowed up, but the GUI still had it" —
// and the follow-up that identified it, "the turn was sitting and waiting and no activity from the
// model was happening until I went into the GUI and accepted the card that wasn't present in the
// TUI". The card was therefore LIVE (the server still had the turn parked on the ask) when the TUI
// retired it, and once retired it could never come back.
//
// The mechanism was a LOCAL event read as proof about the server's turn: a WATCH re-dial is a passive
// follow that runs through the same consume(), so when the watch socket closed cleanly the shell saw
// a graceful stream end, tore the turn's slot down and swept every pending card to "settled". The
// durable re-read that follows is a `replace` (the slot is gone), and replace keeps a consent card
// only while PENDING — so the card was dropped, with nothing left to rediscover it (no live slot, no
// poll) while the plane still held the ask open.
//
// These pin the two halves of the fix: the sweep is gated on the SERVER's authoritative OPEN-ASK LIST
// (ListPendingAsks) rather than on any client-side guess about the turn, and a card that was settled
// anyway is re-armed rather than refused when the server still lists its ask as open.

// THE REGRESSION. While the SERVER still lists the ask as open, no sweep may touch its card — whatever
// this client's own sockets or conversation row just did.
func TestACardTheServerStillListsIsNotSweptAway(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be pending after it is raised")
	}

	// The server's open-ask list still contains the ask.
	m.chatStore.settleStaleConsent("c1", map[string]bool{"ask-1": true})

	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("the server still lists the ask as open, so its card must stay a choice — this is the " +
			"reported ghost: a settle here drops a card the operator had to answer")
	}
	if st := m.chatStore.consentState("c1", "ask-1"); st == nil || !st.Pending() {
		t.Fatalf("the card must still be a choice, got %+v", st)
	}
}

// AND NO EVIDENCE SETTLES NOTHING: a failed or never-run query hands the store nil, and a nil list must
// never be read as "the server lists nothing" — that is the bug this whole file guards, in one line.
func TestNoEvidenceSettlesNothing(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})

	m.chatStore.settleStaleConsent("c1", nil)

	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("a nil open-list is NO evidence, so the card must stay a choice — settling on absence " +
			"is exactly the inference bug this replaces")
	}
}

// The sweep still does its job when the server's list no longer carries the ask — it is a gate, not a
// removal (an EMPTY, non-nil list is the server saying "nothing is open").
func TestAnAskTheServerNoLongerListsIsSwept(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})

	m.chatStore.settleStaleConsent("c1", map[string]bool{})

	if m.chatStore.hasPendingConsent("c1") {
		t.Fatal("the server no longer lists the ask, so its card must stop being a choice")
	}
}

// THE TRUTH-BASED SETTLE: the recorded outcome settles the card on the next transcript load, which
// is what lets the gate above be strict without leaving a card live for an ask that is genuinely
// dead (an expiry written at finalize, or a decision made in the other client).
func TestAPageRecordedOutcomeSettlesTheCard(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})

	// The returned batch is NOT applied: it carries the grant refresh, which needs a live Ask client
	// this fixture deliberately has none of. The settle runs SYNCHRONOUSLY inside onTranscript, which
	// is the behaviour under test.
	_ = m.onTranscript(chat.TranscriptMsg{
		ConvID:      "c1",
		SettledAsks: []chat.AskOutcome{{AskID: "ask-1", Outcome: "expired"}},
	})

	if m.chatStore.hasPendingConsent("c1") {
		t.Fatal("the page records this ask as expired — the card must not still be a choice")
	}
	// AND THE CARD STAYS AS A RECORD, not as a live choice — "SETTLED, NOT REMOVED". The same load passes
	// through onTranscript's durable re-read, which is a `replace` here (the turn is over); replace now
	// KEEPS a settled card so the operator can see what was asked, and every choice path gates on
	// Pending() so the retained record is inert (see replace's retention rule, and
	// TestASettledCardStaysAsARecordAndNeverDoubles for the de-dupe against a durable twin).
	if st := m.chatStore.consentState("c1", "ask-1"); st == nil || st.Pending() {
		t.Fatalf("the card must survive as a settled RECORD, not a live choice, got %+v", st)
	}
	if items := m.chatStore.snapshot("c1"); len(items) != 1 {
		t.Fatalf("a settled card stays on screen as its record, got %d item(s)", len(items))
	}
}

// A DIFFERENT ASK'S OUTCOME MUST NOT TOUCH THIS CARD — the id is the binding.
func TestAOutcomeForAnotherAskLeavesTheCardAlone(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})

	_ = m.onTranscript(chat.TranscriptMsg{
		ConvID:      "c1",
		SettledAsks: []chat.AskOutcome{{AskID: "ask-OTHER", Outcome: "expired"}},
	})

	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("an outcome recorded for a different ask settled this card")
	}
}

// RECOVERY: a settled card is a RECORD of an ask, not a claim on its id. When the server still lists
// that ask as OPEN — the only reason discovery or a watch replay would raise it again — the operator
// must get the choice back, and must not end up with two cards for one ask.
func TestASettledCardIsReArmedRatherThanDuplicated(t *testing.T) {
	m, _ := consentApp(t)
	ask := chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	}
	m.ShowConsentAsk(ask)
	// Settled the wrong way round: the record says resolved, the server will say otherwise.
	m.chatStore.settleAsk("c1", "ask-1", "expired", "")
	if m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be settled")
	}

	// The server lists it as open, so it is raised again (DiscoverPendingAsks → ShowConsentAsk).
	m.ShowConsentAsk(ask)

	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("the ask is still open server-side — the card must be answerable again")
	}
	if items := m.chatStore.snapshot("c1"); len(items) != 1 {
		t.Fatalf("one ask must keep ONE card, got %d item(s)", len(items))
	}
}
