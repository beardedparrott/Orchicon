package ask

// askdraft_test.go — the RECORDED ask card's open free-text row: the keys, and where the answer goes.
//
// The screen-level half of askdraft.go. The shell's click opens the row (App.beginAskDraft sets
// chat.ParsedAsk.Drafting on the ITEM); from there this screen owns the keys, and what Enter does is
// the part worth pinning, because the two cards look identical and behave differently on purpose:
// a CONSENT card's answer replies to a paused turn, while a RECORDED card's answer is the operator's
// next MESSAGE.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// recordedCard is an unanswered recorded question that allows free text.
func recordedCard() chat.ChatItem {
	return chat.ChatItem{
		Kind: chat.KindAsk, Key: "ask-1", At: 1000,
		Ask: &chat.ParsedAsk{
			Question:   "Which branch should the run clone off?",
			Options:    []chat.AskOption{{Label: "develop"}, {Label: "main"}},
			AllowOther: true,
		},
	}
}

// typeInto feeds a string as keystrokes, the way the shell hands them to the screen.
func typeInto(m *Model, text string) {
	for _, r := range text {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestAnOpenFreeTextRowCollectsTheTypingAndSendsItAsTheNextMessage is the reported gap end to end:
// the row takes the keys, what is typed lands in the ROW, and Enter sends it as the operator's next
// message rather than settling the card.
func TestAnOpenFreeTextRowCollectsTheTypingAndSendsItAsTheNextMessage(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.Drafting = true // what the click sets
	m.RenderTranscript(items, chat.Conversation{}, false)

	if !m.ClaimsKeys() {
		t.Fatal("an open free-text row must claim the keys — it is an input the operator is typing into")
	}
	typeInto(m, "release-candidate")
	if got := items[0].Ask.Draft; got != "release-candidate" {
		t.Fatalf("the row collected %q, want what was typed", got)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(h.sent) != 1 || h.sent[0] != "release-candidate" {
		t.Fatalf("sent = %v, want the typed answer as the next message", h.sent)
	}
	if items[0].Ask.Drafting || items[0].Ask.Draft != "" {
		t.Fatalf("the row must close on submit, got drafting=%v draft=%q", items[0].Ask.Drafting, items[0].Ask.Draft)
	}
	// A RECORDED CARD'S TURN IS OVER, so its answer is a message and not a decision on an ask: it
	// must not be routed through the consent reply path.
	if h.resolved != 0 || h.choice != "" {
		t.Fatalf("a recorded card's answer settled an ask (resolved=%d choice=%q) — there is no turn left to reply to",
			h.resolved, h.choice)
	}
	// Nothing is sent for an EMPTY submit: the operator's silence is not an answer.
	h.sent = nil
	items[0].Ask.Draft = ""
	items[0].Ask.Drafting = true
	m.RenderTranscript(items, chat.Conversation{}, false)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(h.sent) != 0 {
		t.Fatalf("an empty submit sent %v", h.sent)
	}
}

// TestEscOnTheFreeTextRowKeepsTheQuestion: Esc puts the ROW away, not the question — the same rule
// the consent card's free text follows, because a typo must not discard what was asked.
func TestEscOnTheFreeTextRowKeepsTheQuestion(t *testing.T) {
	m, h := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.Drafting = true
	m.RenderTranscript(items, chat.Conversation{}, false)
	typeInto(m, "half typed")

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if items[0].Ask.Drafting || items[0].Ask.Draft != "" {
		t.Fatalf("esc must close the row, got drafting=%v draft=%q", items[0].Ask.Drafting, items[0].Ask.Draft)
	}
	if len(h.sent) != 0 {
		t.Fatalf("esc sent %v", h.sent)
	}
	if items[0].Ask.Answered {
		t.Fatal("esc must not answer the question — the row is an input, not a decision")
	}
	// AND THE CARD IS STILL ANSWERABLE: its options still resolve, so nothing was thrown away.
	if m.ClaimsKeys() {
		t.Fatal("a closed row must release the keys")
	}
}

// TestCtrlGDefersTheRowAndAClickTakesItBack pins the chord's contract on this card too: ctrl+g
// reaches the composer and does not discard what was typed, and clicking the row again is the
// operator acting on the card, so the row takes the keys back.
func TestCtrlGDefersTheRowAndAClickTakesItBack(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.Drafting = true
	m.RenderTranscript(items, chat.Conversation{}, false)
	typeInto(m, "in progress")

	m.DropKeyClaim() // the chord's release, as the shell runs it
	if m.ClaimsKeys() {
		t.Fatal("ctrl+g must hand the keyboard to the composer while the row stays open")
	}
	if items[0].Ask.Draft != "in progress" {
		t.Fatalf("the deferral discarded what was typed (%q)", items[0].Ask.Draft)
	}

	m.ReArmAskDraftClaim() // what a click on the row runs
	if !m.ClaimsKeys() {
		t.Fatal("a click on the row must take the keyboard back")
	}
}

// TestADraftIsReleasedWhenItsCardLeavesTheTranscript: the claim is a latch, and a card that is no
// longer in the transcript must not keep the keyboard — the same release rule the consent card has.
func TestADraftIsReleasedWhenItsCardLeavesTheTranscript(t *testing.T) {
	m, _ := newTestModel(t)
	items := []chat.ChatItem{recordedCard()}
	items[0].Ask.Drafting = true
	m.RenderTranscript(items, chat.Conversation{}, false)
	if !m.ClaimsKeys() {
		t.Fatal("precondition: the open row claims the keys")
	}
	m.RenderTranscript(nil, chat.Conversation{}, false)
	if m.ClaimsKeys() {
		t.Fatal("a draft whose card has left the transcript must release the keys")
	}
}
