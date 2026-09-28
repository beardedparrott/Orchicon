package tui

// ask_draft_click_test.go — a CLICK on the recorded card's Other row opens the input ON THE CARD.
//
// The shell-level half of the fix: ask/askdraft.go owns the keys once the row is open, and this is
// the gesture that opens it — through the same click resolver the option rows use, so the row is not
// reachable by one path and dead by another.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// recordedAskPlane builds a shell showing a RECORDED question card that allows free text.
func recordedAskPlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindAsk, Key: "ask-1", At: 1,
		Ask: &chat.ParsedAsk{
			Question:   "Which branch should the run clone off?",
			Options:    []chat.AskOption{{Label: "develop"}, {Label: "main"}},
			AllowOther: true,
		},
	})
	m.onChatWake()
	if !strings.Contains(m.viewFrame(), chat.ConsentOther) {
		t.Fatalf("fixture: the card is not on screen:\n%s", m.viewFrame())
	}
	return m
}

// TestAClickOnTheRecordedCardsOtherRowOpensTheInput is the reported gap through the shell's own
// click path: the row opens the card's input, the card takes the keys so the typing lands in it, and
// the row's own label is NOT sent as the answer.
func TestAClickOnTheRecordedCardsOtherRowOpensTheInput(t *testing.T) {
	m := recordedAskPlane(t)
	m = clickRow(m, rowCarryingFrame(t, m, chat.ConsentOther))

	var drafting bool
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindAsk && it.Ask != nil && it.Ask.Drafting {
			drafting = true
		}
	}
	if !drafting {
		t.Fatal("a click on Other must open the free-text row on the card")
	}
	// AND THE CARD TAKES THE KEYS: an input row the operator cannot type into is not an input.
	as, ok := m.screens[TabAsk].(*ask.Model)
	if !ok {
		t.Fatal("the Ask screen is not the one registered")
	}
	if !as.ClaimsKeys() {
		t.Fatal("an open free-text row must claim the keys")
	}
	// NOTHING WENT ON THE WIRE. The alternative — the row's own label as the answer — is the
	// operator's words replaced by the name of the button they pressed.
	if got := strings.TrimSpace(m.dock.Value()); got != "" {
		t.Fatalf("the click put %q in the composer", got)
	}
}

// AND A CLICK ON A REAL OPTION IS UNCHANGED — it still starts the send of that option's label, so
// opening the free-text row did not make the rest of the card inert. The command is the assertion:
// the send path (Controller.AnswerQuestion → Send) returns one, and the free-text path does not.
func TestAClickOnARecordedCardsOptionStillStartsTheSend(t *testing.T) {
	m := recordedAskPlane(t)
	next, cmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10,
		Y: rowCarryingFrame(t, m, "develop"),
	})
	m = next.(*App)

	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindAsk && it.Ask != nil && it.Ask.Drafting {
			t.Fatal("an option click must not open the free-text row")
		}
	}
	if cmd == nil {
		t.Fatal("a click on a canned option must still start the send — the free-text row must not " +
			"have made the rest of the card inert")
	}
}
