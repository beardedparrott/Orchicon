package tui

// consent_other_click_test.go — CLICKING "Other" ON A QUESTION CARD OPENS THE FREE-TEXT ROW.
//
// The operator: "In the GUI, it lets you type in your own response. In the TUI clicking on it does
// nothing. You should be able to click on other and type in a response there."
//
// THERE ARE TWO PATHS ONTO ONE CARD AND ONLY ONE OF THEM KNEW WHAT THE OTHER ROW IS.
//
//   - The KEYBOARD path (ask/consent.go confirmConsent) knows exactly: Enter on the Other row sets
//     OtherMode, the card grows an input row, and Enter submits what was TYPED. It is pinned by
//     TestQuestionCardOtherSendsTheTypedText.
//   - The CLICK path (App.consentDecideFromRow) treated the row as an ordinary choice and settled
//     the ask with the LITERAL label "Other" — so the operator's own words would have been replaced
//     by the name of the button they pressed, and the input row they expected never appeared.
//
// The GUI's card does the right thing on both (AskCard.tsx: "Other…" opens an inline input, and
// Enter sends what was typed through the same onSelect), which is precisely why the two clients
// disagreed about one gesture.
//
// These tests find their rows by SCANNING the real frame, so they cannot pass by agreeing with the
// card's own arithmetic — the same discipline the option-click tests use (ask_card_click_test.go).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// otherCardPlane builds a shell holding a pending QUESTION card whose ask allowed free text — the
// shape the operator's screenshot shows.
//
// It uses the SAME fixture the option-click tests use (askRelaunched + healthyPlane + onChatWake),
// so the card is reached through the real wake path onto a LIVE pane rather than planted in a store
// nothing repaints.
func otherCardPlane(t *testing.T) (*App, []chat.ChatItem) {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ConsentItem(chat.PermissionAsk{
		ID:         "ask-q1",
		Kind:       chat.AskQuestion,
		Question:   "Which branch should the run clone off?",
		Options:    []string{"develop", "main"},
		AllowOther: true,
	}, 1))
	m.onChatWake()
	items := m.chatStore.snapshot("c1")
	if len(items) == 0 || items[0].Consent == nil {
		t.Fatal("fixture: the card never reached the transcript")
	}
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be pending")
	}
	// AND IT IS ON SCREEN, or every click assertion below would pass vacuously.
	if !strings.Contains(m.viewFrame(), chat.ConsentOther) {
		t.Fatalf("fixture: the card is not on screen:\n%s", m.viewFrame())
	}
	return m, items
}

// rowCarryingFrame returns the frame row carrying marker, scanning the rendered frame.
func rowCarryingFrame(t *testing.T, m *App, marker string) int {
	t.Helper()
	for i, row := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(row, marker) {
			return i
		}
	}
	t.Fatalf("fixture: %q is not on screen, so nothing could be clicked:\n%s", marker, m.viewFrame())
	return -1
}

// clickRow presses the left mouse button on a frame row.
func clickRow(m *App, row int) *App {
	next, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: row,
	})
	return next.(*App)
}

// TestAClickOnOtherOpensTheFreeTextRow is the reported bug: the Other row exists on the card (the
// operator's screenshot), so a click on it must open the input the row promises — and must NOT
// answer the question in the operator's name.
func TestAClickOnOtherOpensTheFreeTextRow(t *testing.T) {
	m, items := otherCardPlane(t)
	row := rowCarryingFrame(t, m, chat.ConsentOther)
	m = clickRow(m, row)

	st := items[0].Consent
	if st == nil {
		t.Fatal("the card vanished from the transcript")
	}
	if !st.OtherMode {
		t.Fatal("a click on Other must open the free-text row — the GUI opens one and so does Enter on this card's own keyboard path")
	}
	if !st.Pending() {
		t.Fatalf("a click on Other SETTLED the ask as %q — the operator's words were replaced by the name of the button they pressed", st.Choice)
	}
	// AND THE CARD DRAWS IT. The operator's complaint was that nothing appeared; the row they were
	// promised is the input the card grows under its options (kit2.CardSpec.ShowInput).
	if card := chat.ConsentCardText(items[0], 80); !strings.Contains(card, "> ") {
		t.Fatalf("the card must draw the free-text row it just opened:\n%s", card)
	}
}

// TestTypingAfterAClickOnOtherBecomesTheAnswer is the other half of the report ("...and type in a
// response there"): once the row is open by click, typing goes into the CARD and Enter submits the
// typed words as the answer — not as a message, and not as the literal "Other".
func TestTypingAfterAClickOnOtherBecomesTheAnswer(t *testing.T) {
	m, items := otherCardPlane(t)
	m = clickRow(m, rowCarryingFrame(t, m, chat.ConsentOther))

	for _, r := range "release-candidate" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(*App)
	}
	if got := strings.TrimSpace(m.dock.Value()); got != "" {
		t.Fatalf("the answer leaked into the composer (%q) instead of the card's input row", got)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*App)

	st := items[0].Consent
	if st.Pending() {
		t.Fatal("Enter on a typed answer must submit it")
	}
	if st.Decision != chat.DecisionAnswer || st.Choice != "release-candidate" {
		t.Fatalf("the typed answer must be submitted, got dec=%q choice=%q", st.Decision, st.Choice)
	}
}

// TestAClickOnAnOptionStillAnswersTheQuestion is the CONTROL for the gate above: a click on a
// canned option is still a choice and still settles the ask with that option's label. Without it,
// "open the input row" could be satisfied by making every click on the card inert.
func TestAClickOnAnOptionStillAnswersTheQuestion(t *testing.T) {
	m, items := otherCardPlane(t)
	m = clickRow(m, rowCarryingFrame(t, m, "develop"))

	st := items[0].Consent
	if st.Pending() {
		t.Fatal("a click on a canned option must answer the question")
	}
	if st.Decision != chat.DecisionAnswer || st.Choice != "develop" {
		t.Fatalf("clicking the develop option answered (%q, %q), want (answer, develop)", st.Decision, st.Choice)
	}
}

// TestAClickOnOtherTakesTheKeyboardBackFromADeferredCard pins the RE-ARM.
//
// ctrl+g releases the claim without deciding anything, so the operator can reach the composer while
// a question is up (ask.Model.DropKeyClaim — TestCtrlGWhileACardIsPendingDoesNotDeny). A click on
// the card's own Other row is the operator acting ON the card, so the row it opens must be the thing
// that collects the typing — not the composer behind it.
func TestAClickOnOtherTakesTheKeyboardBackFromADeferredCard(t *testing.T) {
	m, items := otherCardPlane(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(*App)

	m = clickRow(m, rowCarryingFrame(t, m, chat.ConsentOther))
	for _, r := range "typed-here" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(*App)
	}
	if got := strings.TrimSpace(m.dock.Value()); got != "" {
		t.Fatalf("the typed answer landed in the composer (%q) while the card's input row was open", got)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*App)

	if st := items[0].Consent; st.Choice != "typed-here" {
		t.Fatalf("the click did not take the keyboard back for the card's input row, got answer %q", st.Choice)
	}
}

// AND THE ROW IS REPORTED AS A CHOICE AT ALL — the geometry half. If the Other row carried no span,
// no click could ever land on it and every assertion above would be testing nothing.
func TestTheOtherRowIsClickable(t *testing.T) {
	m, _ := otherCardPlane(t)
	row := rowCarryingFrame(t, m, chat.ConsentOther)
	kind, _, label, ok := m.transcriptCardOptionAtFrameRow(row)
	if !ok {
		t.Fatal("the Other row resolved to nothing — it is drawn but not reachable by a click")
	}
	if kind != chat.KindConsent || label != chat.ConsentOther {
		t.Fatalf("the Other row resolved to (%q, %q), want (%q, %q)", kind, label, chat.KindConsent, chat.ConsentOther)
	}
}
