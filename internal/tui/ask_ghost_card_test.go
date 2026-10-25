package tui

// ask_ghost_card_test.go — THE GHOST PHASE: a card the SERVER still lists as open must never be settled
// or dropped by a CLIENT-SIDE inference about the turn.
//
// The operator, verbatim: "Every once in awhile, a card will still be in this ghost phase where it shows
// up and then goes away before I can answer the card repeatedly. It forces me to go into the GUI to accept
// or deny the card or type my response."
//
// THE MECHANISM. settleStaleConsent used to fire on `!runningFor(convID)`, and runningFor is
// IsStreaming || the POLLED turn_in_flight row. Neither half is real evidence about the ask:
//
//   - a WATCH RE-DIAL's passive close clears the local slot, so IsStreaming goes false on a turn that is
//     still running;
//   - the polled turn_in_flight row is stale by construction and ABSENT for a conversation off the current
//     rail page (the rail is paginated).
//
// Together they made `runningFor` false for a LIVE turn, the sweep settled the still-open card,
// `replace` dropped it (its retention rule was pending-only), and DiscoverPendingAsks re-added it
// from the server on the next attach/poll: add → settle → drop → re-add. That is the flicker.
//
// THE FIX, pinned here: settle on EVIDENCE (the server's open-ask list, or a recorded outcome row), never
// on an inferred turn state. A dead ask still settles; a live one never does.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// ghostApp builds the shell with the REAL Ask screen, an open conversation, an ARMED controller (so the
// stream-close path is the real one), and NO conversation row — the absent-row case from the report.
func ghostApp(t *testing.T) (*App, chat.PermissionAsk) {
	t.Helper()
	m, _ := consentApp(t)
	m.clients = client.New(client.Options{BaseURL: "http://127.0.0.1:1", Token: "oc_t"})
	m.chat = chat.NewController(m.clients)
	m.conversations = nil // off the current rail page: the polled row is ABSENT
	// Arm the stream slot without opening a socket (production sends; this is the same state EndStream
	// clears). No goroutines are started, so the reproduction stays deterministic.
	m.chat.SetServerTimeForTest("c1", 1_700_000_000_000)
	ask := chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	}
	m.ShowConsentAsk(ask)
	m.onChatWake()
	return m, ask
}

// consentCopy returns the transcript's COPY of an ask's card state — (decision, present) — which is what
// the renderer draws. "" is a PENDING card.
func consentCopy(s *chatStore, convID, askID string) (chat.ConsentDecision, bool) {
	for _, it := range s.snapshot(convID) {
		if it.Kind == chat.KindConsent && it.AskID == askID && it.Consent != nil {
			return it.Consent.Decision, true
		}
	}
	return "", false
}

// countConsent counts the cards held for one ask.
func countConsent(s *chatStore, convID, askID string) int {
	n := 0
	for _, it := range s.snapshot(convID) {
		if it.Kind == chat.KindConsent && it.AskID == askID {
			n++
		}
	}
	return n
}

// THE REGRESSION (AC1/AC2). The whole reported sequence, driven through the real close path: a PENDING
// card is up while the server still lists the ask; a stream close lands with runningFor false; a durable
// re-read follows — and through NONE of it does the card stop being pending. On the old code the first
// step settled it (the failure is the card COPY changing state, not a return value).
func TestAGhostCardSurvivesTheReportedSequenceWhileTheServerStillListsIt(t *testing.T) {
	m, ask := ghostApp(t)
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-1"); !ok || dec != "" {
		t.Fatalf("precondition: the card must be up and PENDING, got (dec=%q present=%v)", dec, ok)
	}
	if !m.runningFor("c1") {
		t.Fatal("fixture: the slot must be armed before the close, or the reproduction measures nothing")
	}

	// 1. THE STREAM CLOSE. `ended` is true for the slot's own generation; EndStream clears the slot and
	//    the conversation row is absent, so `runningFor` becomes FALSE — the exact input that used to make
	//    settleStaleConsent settle a still-open card.
	gen := m.chat.CurrentGen("c1")
	m.onStreamDone(chat.StreamDoneMsg{ConvID: "c1", Gen: gen})
	if m.runningFor("c1") {
		t.Fatal("fixture: the close must have made runningFor false (slot cleared, row absent)")
	}
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-1"); !ok {
		t.Fatal("the stream close DROPPED the card — the operator's \"goes away before I can answer\"")
	} else if dec != "" {
		t.Fatalf("the stream close SETTLED the card while the server still lists the ask as open: "+
			"decision=%q", dec)
	}

	// 2. THE DURABLE RE-READ (the Poll the close drives). No recorded outcome => nothing settles.
	_ = m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "do the thing", Key: "m-1", At: 1},
	}})
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-1"); !ok || dec != "" {
		t.Fatalf("a transcript load dropped/settled the still-open card, got (dec=%q present=%v)", dec, ok)
	}

	// 3. THE SERVER'S OWN ANSWER: discovery lists the ask as still open and re-draws it (idempotent).
	//    It cannot settle the card — discovery only ADDS (see ShowDiscoveredAsks) — and there is still
	//    exactly ONE.
	m.ShowConsentAsk(ask)
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-1"); !ok || dec != "" {
		t.Fatalf("the server lists the ask as open, so the card must stay answerable, got (dec=%q present=%v)", dec, ok)
	}
	if n := countConsent(m.chatStore, "c1", "ask-1"); n != 1 {
		t.Fatalf("one ask must keep ONE card, got %d", n)
	}

	// 4. REPEATEDLY, so a one-shot fix cannot pass: the same events again leave it answerable.
	for i := 0; i < 3; i++ {
		m.onStreamDone(chat.StreamDoneMsg{ConvID: "c1", Gen: m.chat.CurrentGen("c1")})
		m.ShowConsentAsk(ask)
	}
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("reapplying the client-side events settled the card the server still lists as open")
	}
}

// AC6 (outermost proof). On the PAINTED frame, the card the events were applied to is STILL the pending
// card: its target and its CHOICE rows are present, not the one-line record a settled card draws.
func TestTheGhostCardIsStillTheLiveCardInThePaintedFrame(t *testing.T) {
	m, ask := ghostApp(t)
	m.onStreamDone(chat.StreamDoneMsg{ConvID: "c1", Gen: m.chat.CurrentGen("c1")})
	_ = m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "do the thing", Key: "m-1", At: 1},
	}})
	m.ShowConsentAsk(ask)
	m.onChatWake()

	frame := m.viewFrame()
	if !strings.Contains(frame, "/home/ops/project/main.go") {
		t.Fatalf("the pending card is not on the painted frame at all:\n%s", tailOf(frame, 1500))
	}
	if !strings.Contains(frame, chat.ConsentAllowOnce) {
		t.Fatalf("the painted frame does not show the card's CHOICE rows — it is a settled record, not "+
			"the live card:\n%s", tailOf(frame, 1500))
	}
}

// AC3. THE OPERATOR'S OWN ANSWER IS UNCHANGED — a regression here would be worse than the flicker.
func TestAnsweringFromThisClientStillSettlesAndReplies(t *testing.T) {
	m, _ := ghostApp(t)
	cmd := m.ConsentResolve("ask-1", chat.DecisionAllowOnce, "")
	if cmd == nil {
		t.Fatal("no command — the reply (ReplyPermissionAsk) never leaves the client")
	}
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-1"); !ok || dec != chat.DecisionAllowOnce {
		t.Fatalf("the operator's own decision must settle the card as allow-once, got (dec=%q present=%v)", dec, ok)
	}
	if m.chatStore.hasPendingConsent("c1") {
		t.Fatal("an answered card must stop being a choice")
	}
}

// AC4. A GENUINELY-FINISHED ASK STILL SETTLES — by the page RECORDING an outcome, which is the one
// authority left now that absence does not settle. Without this the rule would be "never settle" and a
// dead card would live forever.
//
// THE "SERVER NO LONGER LISTS IT" HALF IS GONE ON PURPOSE. It used to settle here, and it is exactly what
// made the TUI flicker (see TestDiscoveryOnlyAddsAndNeverSettles): the open-ask list is a point-in-time
// read over a poll, so a transient empty answer retired live cards. The GUI never settled that way and
// never flickered. A card the server has genuinely dropped but that no outcome record covers now stays
// answerable — clickable, and resolving to "already answered" server-side.
func TestAGenuinelyFinishedAskStillSettles(t *testing.T) {
	// (i) a recorded outcome on the durable page (answered elsewhere / expired at finalize).
	m2, _ := ghostApp(t)
	_ = m2.onTranscript(chat.TranscriptMsg{
		ConvID:      "c1",
		SettledAsks: []chat.AskOutcome{{AskID: "ask-1", Outcome: "expired"}},
	})
	if m2.chatStore.hasPendingConsent("c1") {
		t.Fatal("the page records this ask as expired — the card must not still be a choice")
	}
}

// AC5. A settle is VISIBLE AS SETTLED, not a disappearance — and never drawn TWICE once its durable twin
// lands. A permission ask has no durable rendering row, so its record persists; a question HAS a durable
// KindAsk twin, so its record is de-duped away.
func TestASettledCardStaysAsARecordAndNeverDoubles(t *testing.T) {
	// (a) no durable twin: the record persists through replace.
	s := newTestChatStore()
	item := pendingConsentItem("ask-1")
	item.Consent.Decision = chat.DecisionSettled
	s.append("c1", item)
	s.replace("c1", []chat.ChatItem{{Kind: chat.KindText, Text: "reply", Key: "m-1"}})
	if dec, ok := consentCopy(s, "c1", "ask-1"); !ok || dec != chat.DecisionSettled {
		t.Fatalf("a settled card must persist as a settled RECORD through replace, got (dec=%q present=%v)", dec, ok)
	}
	if s.hasPendingConsent("c1") {
		t.Fatal("a settled record must not be a live choice")
	}

	// (b) a durable twin exists: the live record is dropped, so the card never renders twice.
	s2 := newTestChatStore()
	q := chat.ConsentItem(chat.PermissionAsk{ID: "q-1", Kind: chat.AskQuestion, Question: "Which branch?"}, 1000)
	q.Consent.Decision = chat.DecisionSettled
	s2.append("c1", q)
	s2.replace("c1", []chat.ChatItem{
		{Kind: chat.KindText, Text: "reply", Key: "m-1", At: 1},
		{Kind: chat.KindAsk, Key: "m-1-ask", At: 1, Ask: &chat.ParsedAsk{Question: "Which branch?"}},
	})
	if _, ok := consentCopy(s2, "c1", "q-1"); ok {
		t.Fatal("a settled question must be de-duped against its durable KindAsk card — it would draw twice")
	}
}

// DISCOVERY ONLY ADDS — it can never retire a card, which is the rule the GUI has always followed.
//
// This is the TUI-only flicker: "Looks like just in the TUI." The TUI settled every pending card ABSENT
// from the server's open-ask list, and a poll that answered empty or partial (a read racing the raise, a
// conversation still being indexed) retired a LIVE card; the next poll drew it back. Settle → redraw, at
// poll cadence.
//
// The card is settled instead by POSITIVE evidence, and this pins both halves.
func TestDiscoveryOnlyAddsAndNeverSettles(t *testing.T) {
	m, keep := ghostApp(t)

	// A card the server no longer lists. Under the old rule it would be settled by the next poll; under
	// this one it stays ANSWERABLE, because "the list did not mention it" is not evidence. A stale card
	// the operator can still click is a far smaller harm than a live turn behind a card nobody can see.
	drop := chat.PermissionAsk{
		ID: "ask-dropped", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/gone.go", Directory: "/home/ops/project",
	}
	m.ShowConsentAsk(drop)

	// THE ONE CALL the discovery path makes, several times, with both an empty answer and one that lists
	// only `keep` — neither may settle anything.
	store := appEventStore{m: m}
	store.ShowDiscoveredAsks("c1", nil)
	store.ShowDiscoveredAsks("c1", []chat.PermissionAsk{keep})
	store.ShowDiscoveredAsks("c1", nil)

	for _, id := range []string{"ask-1", "ask-dropped"} {
		if dec, ok := consentCopy(m.chatStore, "c1", id); !ok || dec != "" {
			t.Errorf("%s must still be ANSWERABLE after discovery polled without listing it, got "+
				"(dec=%q present=%v) — settling on absence is the flicker", id, dec, ok)
		}
	}
	if n := countConsent(m.chatStore, "c1", "ask-1"); n != 1 {
		t.Errorf("repeated discovery must not duplicate a card: got %d for one ask", n)
	}

	// AND THE POSITIVE EVIDENCE STILL SETTLES. The durable page's recorded outcome is what retires a card
	// now — the path that survives a reload and covers an expiry at finalize or a decision taken in the
	// other client. Without this the rule would leak cards forever.
	m.chatStore.settleAsk("c1", "ask-dropped", "allow_once", "")
	if dec, ok := consentCopy(m.chatStore, "c1", "ask-dropped"); !ok || dec != chat.DecisionAllowOnce {
		t.Errorf("a RECORDED outcome must still settle the card, got (dec=%q present=%v)", dec, ok)
	}
	if dec, _ := consentCopy(m.chatStore, "c1", "ask-1"); dec != "" {
		t.Errorf("settling one ask must not touch another; ask-1 is now %q", dec)
	}
}
