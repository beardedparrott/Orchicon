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
// These pin the two halves of the fix: the sweep is gated on the SERVER's turn state, and a card that
// was settled anyway is re-armed rather than refused when the server still lists its ask as open.

// THE REGRESSION. While the plane still reports the turn in flight, the turn is NOT over and no
// sweep may touch its card — whatever this client's own sockets just did.
func TestAParkedTurnsCardIsNotSweptAway(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be pending after it is raised")
	}

	m.chatStore.settleStaleConsent("c1", false)

	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("a turn the SERVER still reports in flight had its card settled — this is the " +
			"reported swallow: the operator is left with a parked turn and nothing to answer")
	}
	if st := m.chatStore.consentState("c1", "ask-1"); st == nil || !st.Pending() {
		t.Fatalf("the card must still be a choice, got %+v", st)
	}
}

// The sweep still does its job when the turn really is over — it is a gate, not a removal.
func TestAFinishedTurnsCardIsSwept(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})

	m.chatStore.settleStaleConsent("c1", true)

	if m.chatStore.hasPendingConsent("c1") {
		t.Fatal("a finished turn's card must stop being a choice")
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
	// AND THE CARD IS GONE, not merely inert. The same load passes through onTranscript's durable
	// re-read, which is a `replace` here (the turn is over), and replace keeps a consent card only
	// while PENDING — so a settled card leaves the TUI's transcript. That is this client's existing
	// choice, and it is why the TUI draws no settled record where the GUI draws one (the TUI renders
	// no durable tool rows at all — see TranscriptMsg.ToolCalls). Asserted so the asymmetry is
	// deliberate rather than discovered later.
	if st := m.chatStore.consentState("c1", "ask-1"); st != nil && st.Pending() {
		t.Fatalf("the card must not survive as a live choice, got %+v", st)
	}
	if items := m.chatStore.snapshot("c1"); len(items) != 0 {
		t.Fatalf("a settled card is dropped by the durable re-read, got %d item(s)", len(items))
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
