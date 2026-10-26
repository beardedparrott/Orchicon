package ask

// askcard_test.go — the RECORDED ask card's CURSOR: the keys, the claim, and where an answer goes.
//
// The screen-level half of askcard.go. The operator's report — "It works but in the TUI the highlight bar
// is gone. Looks fine the gui." then "This already works with the orchicon adapter. It should mimic that."
// — is about the gap between the TWO cards the transcript can draw for one question:
//
//	KindConsent (native bridge)  has Sel, ↑/↓, Enter  — worked
//	KindAsk     (MCP-served)     drew ▸            — no cursor, no keys  — this file
//
// The renderer's half (the bar appearing, the footer naming the keys) is pinned in internal/tui/chat; this
// file pins the half a render test cannot see: which key does what, and that the claim cannot swallow the
// operator's typing.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// pendingCard is an unanswered recorded question with NO free-text row, so the cursor's range is exactly
// its options. recordedCard() (askdraft_test.go) is the same card WITH the Other row.
func pendingCard() chat.ChatItem {
	return chat.ChatItem{
		Kind: chat.KindAsk, Key: "ask-1", At: 1000,
		Ask: &chat.ParsedAsk{
			Question: "Which branch should the run clone off?",
			Options:  []chat.AskOption{{Label: "develop"}, {Label: "main"}},
		},
	}
}

// TestAPendingRecordedCardClaimsItsCursorKeys — the card the MCP adapters produce now owns ↑/↓ and Enter,
// as the permission card does. Without the claim the keys go to the rail, and the bar is unreachable.
func TestAPendingRecordedCardClaimsItsCursorKeys(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)

	if !m.ClaimsKeys() {
		t.Fatal("a pending recorded card must claim the keys — otherwise ↑/↓ move the conversation rail " +
			"while the operator is trying to move the card's cursor")
	}
	if m.FormOpen() {
		t.Fatal("a card is not a form — FormOpen must stay false so Tab is not captured as a field move")
	}
}

// TestTheCursorMovesAndEnterAnswers — the whole point: the highlight moves, and Enter sends the
// HIGHLIGHTED option as the operator's next message (the turn is over; this is not a reply to a live turn).
func TestTheCursorMovesAndEnterAnswers(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)

	if got := items[0].Ask.Sel; got != 0 {
		t.Fatalf("the cursor starts at row %d, want 0", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := items[0].Ask.Sel; got != 1 {
		t.Fatalf("after ↓ the cursor is at row %d, want 1 — the highlight the renderer draws comes from "+
			"this field, so a cursor that does not move here never moves on screen", got)
	}
	if len(h.sent) != 0 {
		t.Fatalf("moving the cursor sent %v — a move must never answer", h.sent)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(h.sent) != 1 || h.sent[0] != "main" {
		t.Fatalf("sent = %v, want [main] — Enter answers the HIGHLIGHTED row", h.sent)
	}
}

// TestTheCursorWrapsInBothDirections — it is a ring, as the permission card's is. A cursor that sticks at
// the ends reads as a broken arrow key.
func TestTheCursorWrapsInBothDirections(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyUp}) // from row 0, up wraps to the last row
	if got := items[0].Ask.Sel; got != 1 {
		t.Fatalf("↑ from row 0 left the cursor at %d, want 1 (wrapping)", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := items[0].Ask.Sel; got != 0 {
		t.Fatalf("↓ from the last row left the cursor at %d, want 0 (wrapping)", got)
	}
}

// TestTheOtherRowIsReachable — the free-text row is the LAST row of the card, so the cursor must reach it,
// and Enter on it OPENS THE INPUT rather than sending the word "Other".
func TestTheOtherRowIsReachable(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{recordedCard()} // options develop/main + Other
	m.RenderTranscript(items, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // 0 -> 1 (main)
	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // 1 -> 2 (Other)
	if got := items[0].Ask.Sel; got != 2 {
		t.Fatalf("the cursor stopped at row %d, want 2 — the Other row is a row and must be reachable", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if items[0].Ask.Drafting != true {
		t.Fatal("Enter on the Other row must OPEN the free-text row")
	}
	if len(h.sent) != 0 {
		t.Fatalf("sent = %v — pressing Enter on Other must not send the word %q", h.sent, chat.ConsentOther)
	}
}

// TestTypingHandsTheKeysBackAndIsNotSwallowed — THE DEPARTURE FROM THE PERMISSION CARD, and the reason it
// exists.
//
// The permission card claims the keyboard absolutely and swallows every key it does not use, because a live
// turn is blocked on it. A recorded card sits in an IDLE conversation, where the operator may well want to
// keep typing: an absolute claim would read as a composer that stopped working. So a printable character
// releases the claim AND is delivered, rather than eaten — losing the first letter of the next message was
// the obvious wrong answer.
func TestTypingHandsTheKeysBackAndIsNotSwallowed(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("precondition: the card claims the keys")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})

	if m.ClaimsKeys() {
		t.Fatal("typing must hand the keyboard back — otherwise the composer is dead for as long as the " +
			"question goes unanswered")
	}
	// AND THE KEYSTROKE IS DELIVERED, not dropped. The command the screen returns re-dispatches it.
	cmd := redispatchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if cmd == nil {
		t.Fatal("typing must produce a re-dispatch command")
	}
	if msg, ok := cmd().(tea.KeyMsg); !ok || string(msg.Runes) != "h" {
		t.Fatalf("the re-dispatch delivered %#v, want the keystroke the operator typed", msg)
	}
	// The card is UNANSWERED and nothing was sent: releasing the claim is not answering.
	if items[0].Ask.Answered {
		t.Fatal("typing must not answer the question")
	}
}

// TestEscapeReleasesWithoutDismissing — Escape on a question must never record an answer.
//
// On a PERMISSION card Escape denies, and on a consent QUESTION it records "dismissed — no answer sent".
// Neither belongs here: this card is inside an idle conversation, nothing is blocked on it, and a question
// must not be closed by the keystroke the operator used to get their keyboard back.
func TestEscapeReleasesWithoutDismissing(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ClaimsKeys() {
		t.Fatal("Escape must hand the keyboard back")
	}
	if len(h.sent) != 0 {
		t.Fatalf("Escape sent %v — it must not answer a question", h.sent)
	}
	if items[0].Ask.Answered || items[0].Ask.Refused {
		t.Fatal("Escape must leave the card PENDING and answerable")
	}
	// Re-arming is the same claim path a new ask takes, so the card is not lost by releasing it.
	m.ReArmAskCardClaim()
	if !m.ClaimsKeys() {
		t.Fatal("the claim must be re-armable — a released card is not a lost one")
	}
}

// TestAnAnsweredCardGivesTheKeysBack — a settled ask is a RECORD, not a card. It must not keep the
// keyboard, or the transcript would be permanently captured by a question that is already history.
func TestAnAnsweredCardGivesTheKeysBack(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{pendingCard()}
	m.RenderTranscript(items, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("precondition: the pending card claims the keys")
	}

	items[0].Ask.Answered = true
	items[0].Ask.AnswerText = "develop"
	m.RenderTranscript(items, chat.Conversation{}, false)
	if m.ClaimsKeys() {
		t.Fatal("an ANSWERED card must release the keys — it draws as one line of history, with nothing to move")
	}
}

// TestTheNewestPendingCardOwnsTheCursor — one transcript can hold several unanswered questions. The one the
// operator is looking at is the last, and the claim must follow it rather than an older card.
func TestTheNewestPendingCardOwnsTheCursor(t *testing.T) {
	m, _ := newTestModel(t)
	first := pendingCard()
	first.Key = "ask-old"
	second := pendingCard()
	second.Key = "ask-new"
	second.Ask.Question = "And which tag?"

	m.RenderTranscript([]chat.ChatItem{first, second}, chat.Conversation{}, false)
	if got := m.askCardKey; got != "ask-new" {
		t.Fatalf("the adopted card is %q, want ask-new — the newest pending question is the one being asked", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if first.Ask.Sel != 0 {
		t.Error("moving the cursor touched the OLDER card")
	}
	if second.Ask.Sel != 1 {
		t.Error("moving the cursor did not move the card that holds the keys")
	}
}

// TestTheCursorCannotRunOffTheCard — an item reused for a new question must not start with a cursor past its
// rows, or Enter would read a row that is not there and do nothing.
func TestTheCursorCannotRunOffTheCard(t *testing.T) {
	m, h := newTestModel(t)
	card := pendingCard()
	card.Ask.Sel = 9 // stale, from a longer question
	m.RenderTranscript([]chat.ChatItem{card}, chat.Conversation{}, false)

	if card.Ask.Sel >= len(card.Ask.Options) {
		t.Fatalf("the cursor is at row %d of %d — it must be clamped on adoption",
			card.Ask.Sel, len(card.Ask.Options))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(h.sent) != 1 {
		t.Fatalf("Enter sent %v, want one answer from the clamped row", h.sent)
	}
}

// TestTheFreeTextRowOutranksTheCursor — while the operator types into Other, the CURSOR must not also claim:
// two claims over one keyboard would move the highlight under their typing.
func TestTheFreeTextRowOutranksTheCursor(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.Drafting = true // the click (or Enter on Other) opens it
	m.RenderTranscript(items, chat.Conversation{}, false)

	if !m.ClaimsKeys() {
		t.Fatal("the open row claims the keys")
	}
	if m.askCardClaims() {
		t.Fatal("the CURSOR must not claim while the free-text row owns the keys")
	}
	before := items[0].Ask.Sel
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if items[0].Ask.Sel != before {
		t.Fatal("an arrow while typing into Other must not move the card's cursor")
	}
}

// TestEnterAnswersTheAskRatherThanSendingAMessage pins that Enter ANSWERS the ask rather than sending a
// message.
//
// THE DEFECT THIS PINS, and it is the whole of "I answer it and it asks again": answering a RECORDED
// card used to go out as ConsentSend — an ordinary next message. The transcript's awaiting marker was
// therefore never cleared, so the card was still open to every other client and came back on the next
// reload, while the operator's answer sat beside it as a message. The marker IS the card's state.
//
// The answer must be a REPLY addressed to the ask's id, and it must NOT also send a message (that would
// deliver the answer twice: once as the reply's turn, once as a message).
func TestEnterAnswersTheAskRatherThanSendingAMessage(t *testing.T) {
	m, h := newTestModel(t)
	card := pendingCard()
	card.Ask.ID = "tc-2" // the recorded tool call's id — what ReplyPermissionAsk addresses
	m.RenderTranscript([]chat.ChatItem{card}, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyDown})  // highlight "main"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // answer it

	if len(h.asked) != 1 {
		t.Fatalf("AskReply called %d times, want exactly 1 — the card must answer its ASK", len(h.asked))
	}
	if h.asked[0].id != "tc-2" {
		t.Fatalf("replied to ask %q, want tc-2 — a reply addressed to the wrong id settles the wrong "+
			"card, or nothing at all", h.asked[0].id)
	}
	if h.asked[0].answer != "main" {
		t.Fatalf("answer = %q, want the highlighted row", h.asked[0].answer)
	}
	if len(h.sent) != 0 {
		t.Fatalf("sent %v as a message — the answer must travel as the REPLY, or the awaiting marker "+
			"survives and the card comes back open", h.sent)
	}
}

// TestACardWithNoIDStillAnswers — the degraded path, kept deliberately.
//
// A row recorded before ParsedAsk carried its id has nothing to address a reply to, so the old
// send-the-label path is the only thing that can work. A degraded answer beats a card that cannot be
// answered at all — and the failure mode of dropping this fallback is a silently dead card.
func TestACardWithNoIDStillAnswers(t *testing.T) {
	m, h := newTestModel(t)
	card := pendingCard() // no ID set
	m.RenderTranscript([]chat.ChatItem{card}, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(h.asked) != 0 {
		t.Fatalf("AskReply called with no id (%v) — a reply needs an ask to address", h.asked)
	}
	if len(h.sent) != 1 || h.sent[0] != "develop" {
		t.Fatalf("sent = %v, want the label as a message: a card with no id can only be answered this way",
			h.sent)
	}
}

// TestTheFreeTextRowAlsoReplies — Other must settle the card the same way the options do, or the
// operator's own words would leave the marker standing while their answer went out as a message.
func TestTheFreeTextRowAlsoReplies(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.ID = "tc-7"
	m.RenderTranscript(items, chat.Conversation{}, false)

	m.Update(tea.KeyMsg{Type: tea.KeyDown})  // -> main
	m.Update(tea.KeyMsg{Type: tea.KeyDown})  // -> Other
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // opens the input
	if !items[0].Ask.Drafting {
		t.Fatal("precondition: Enter on Other opens the free-text row")
	}
	// THE WAKE THAT FOLLOWS THE REPAINT, reproduced: opening the row returns a repaint, the shell then
	// re-renders the transcript, and SyncAskDraft is what ADOPTS the item's ParsedAsk so the keys have
	// somewhere to land. Without it the screen holds no draft and every keystroke falls through.
	m.RenderTranscript(items, chat.Conversation{}, false)
	typeInto(m, "release-candidate")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // submits it

	if len(h.asked) != 1 || h.asked[0].answer != "release-candidate" {
		t.Fatalf("AskReply = %v, want one reply carrying the typed words", h.asked)
	}
	if h.asked[0].id != "tc-7" {
		t.Fatalf("replied to %q, want tc-7", h.asked[0].id)
	}
	if len(h.sent) != 0 {
		t.Fatalf("sent %v as a message — the typed answer must be the REPLY", h.sent)
	}
}
