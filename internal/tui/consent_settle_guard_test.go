package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// consent_settle_guard_test.go — A CARD MAY ONLY BE SETTLED BY POSITIVE EVIDENCE, NEVER BY ABSENCE.
//
// The reported loss: "a permissions card in the TUI get swallowed up, but the GUI still had it" — the turn
// was still parked on the server when the TUI retired the card, and once retired it could never come back.
// The first fix gated the sweep on the server's open-ask list, which was better than inferring from the
// turn but still settled on ABSENCE: a poll that answered empty or partial retired LIVE cards, the next
// poll drew them back, and the card flickered at poll cadence.
//
// The operator localised it precisely — "Looks like just in the TUI" — and the difference was that the GUI
// has never settled this way. Its discovery effect only ADDS:
//
//	if (!pendingAsks || pendingAsks.length === 0) return;
//	for (const ask of pendingAsks) applyAsk(activeConvId, ask);
//
// The TUI now matches it. Discovery can only add; a card is settled solely by
//   - a RECORDED OUTCOME in the durable page (which survives a reload, and covers an expiry at finalize
//     or a decision taken in the other client), or
//   - a DECISION THIS CLIENT MADE.
//
// The asymmetry is deliberate and it is the safe direction: a card the server has genuinely dropped but
// that no outcome record covers stays answerable — a stale card the operator can dismiss, resolving to
// "already answered" server-side — rather than a live turn parked behind a card nobody can see.

// DISCOVERY NEVER SETTLES, whatever the list does or does not contain. This is the flicker, pinned.
func TestDiscoveryNeverSettlesWhateverTheListSays(t *testing.T) {
	for _, tc := range []struct {
		name string
		asks []chat.PermissionAsk
	}{
		{"empty answer", nil},
		{"an answer listing nothing for this ask", []chat.PermissionAsk{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := consentApp(t)
			m.ShowConsentAsk(chat.PermissionAsk{
				ID: "ask-1", Kind: chat.AskTool, Tool: "write",
				Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
			})
			if !m.chatStore.hasPendingConsent("c1") {
				t.Fatal("precondition: the card must be pending after it is raised")
			}

			appEventStore{m: m}.ShowDiscoveredAsks("c1", tc.asks)

			if !m.chatStore.hasPendingConsent("c1") {
				t.Fatal("discovery settled a live card — absence is not evidence, and this is what made " +
					"the card flicker at poll cadence in the TUI while the GUI was stable")
			}
			if st := m.chatStore.consentState("c1", "ask-1"); st == nil || !st.Pending() {
				t.Fatalf("the card must still be a choice, got %+v", st)
			}
		})
	}
}

// AND REPEATEDLY, so a one-shot tolerance cannot pass: the poll runs on a cadence, and the card must
// survive every tick of it.
func TestRepeatedDiscoveryPollsLeaveTheCardAnswerable(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	store := appEventStore{m: m}
	for i := 0; i < 10; i++ {
		store.ShowDiscoveredAsks("c1", nil)
	}
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("ten empty discovery polls settled the card — at poll cadence that is the flicker")
	}
	if n := len(m.chatStore.snapshot("c1")); n == 0 {
		t.Fatal("the card vanished from the transcript entirely")
	}
}

// DISCOVERY STILL DRAWS. Adding is the half it must keep: a card raised by a turn this client never
// streamed is only ever learned about here.
func TestDiscoveryStillDrawsACardItIsToldAbout(t *testing.T) {
	m, _ := consentApp(t)
	ask := chat.PermissionAsk{
		ID: "ask-9", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/new.go", Directory: "/home/ops/project",
	}
	appEventStore{m: m}.ShowDiscoveredAsks("c1", []chat.PermissionAsk{ask})
	if st := m.chatStore.consentState("c1", "ask-9"); st == nil || !st.Pending() {
		t.Fatalf("a discovered ask must be drawn and answerable, got %+v", st)
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
