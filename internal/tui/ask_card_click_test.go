package tui

// ask_card_click_test.go — CLICKING AN OPTION ON A RECORDED ask_user CARD ANSWERS THE QUESTION.
//
// The `ask_user` tool RECORDS the question and the turn ENDS; the operator's answer is the next user message.
// On this client that answer is a CLICK on an option, sent through the ordinary send path
// (Controller.AnswerQuestion → Send) — so there is no rendezvous to wait on and nothing here may block.
//
// The same THREE COORDINATE SPACES the copy gestures use have to agree (frame row → body row → body line →
// item), and the geometry comes from the render that drew the card. These tests find their rows by SCANNING the
// real frame for the option text, so they cannot pass by agreeing with the formula they are checking.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// askCardPlane builds a conversation whose last assistant message recorded a clarifying question — the
// transcript the terminal draws as a card — and returns the app.
func askCardPlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindAsk,
		Key:  "m-m1-ask",
		At:   1,
		Ask: &chat.ParsedAsk{
			Question: "Which branch should the run clone off?",
			Options: []chat.AskOption{
				{Label: "develop", Description: "the integration branch"},
				{Label: "main"},
			},
		},
	})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "MARKER-REPLY", Key: "t1", At: 2})
	m.onChatWake()
	return m
}

// askRowAt returns the frame row carrying the marker, scanning the rendered frame so the assertions do not
// depend on the card's own arithmetic.
func askRowAt(t *testing.T, m *App, marker string) int {
	t.Helper()
	for i, row := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(row, marker) {
			return i
		}
	}
	t.Fatalf("fixture: %q is not on screen, so nothing could be clicked", marker)
	return -1
}

// THE CARD IS ON SCREEN AT ALL — the precondition that would otherwise let every assertion below pass
// vacuously.
func TestTheAskCardRendersInTheTranscript(t *testing.T) {
	m := askCardPlane(t)
	frame := m.viewFrame()
	for _, want := range []string{"Orchicon asks", "Which branch should the run clone off?", "develop", "main"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the clarifying-question card is not on screen (%q missing)", want)
		}
	}
}

// CLICKING AN OPTION RESOLVES TO THAT OPTION'S LABEL — both of them, so the rows are genuinely distinct
// rather than one row answering for the whole card.
func TestAClickOnAnOptionResolvesToItsLabel(t *testing.T) {
	m := askCardPlane(t)
	for _, want := range []string{"develop", "main"} {
		row := askRowAt(t, m, want)
		got, ok := m.transcriptAskOptionAtFrameRow(row)
		if !ok {
			t.Fatalf("a click on the %q option resolved to nothing — the card's options are not a choice", want)
		}
		if got != want {
			t.Errorf("clicking the %q row sent %q — the answer would be the wrong option", want, got)
		}
	}
}

// AN OPTION'S DESCRIPTION BELONGS TO THE OPTION: a click on the description row is a click on that choice, and
// not a dead row inside a live one.
func TestAClickOnAnOptionsDescriptionPicksThatOption(t *testing.T) {
	m := askCardPlane(t)
	row := askRowAt(t, m, "the integration branch")
	got, ok := m.transcriptAskOptionAtFrameRow(row)
	if !ok || got != "develop" {
		t.Errorf("clicking the option's description gave (%q, %v), want (develop, true)", got, ok)
	}
}

// THE CARD'S OWN PROSE IS NOT A CHOICE: the question row and the header must resolve to nothing, or a click
// meant to select text would send a message.
func TestAClickOnTheQuestionIsNotAChoice(t *testing.T) {
	m := askCardPlane(t)
	for _, marker := range []string{"Orchicon asks", "Which branch should the run clone off?"} {
		row := askRowAt(t, m, marker)
		if got, ok := m.transcriptAskOptionAtFrameRow(row); ok {
			t.Errorf("a click on %q resolved to the option %q — the card would answer a question nobody asked", marker, got)
		}
	}
}

// AND THE CLICK IS WIRED TO THE SEND — resolvable is not enough; a press has to start the turn that carries
// the choice.
func TestAClickOnAnOptionIsWiredToTheSend(t *testing.T) {
	m := askCardPlane(t)
	row := askRowAt(t, m, "main")
	_, cmd := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 10, Y: row})
	if cmd == nil {
		t.Fatal("a click on a clarifying-question option produced no command, so the choice was never sent")
	}
}

// AN ANSWERED CARD IS SETTLED. Once a later user message exists the question has been answered, and its
// options are shown but no longer clickable — otherwise a click on a stale card re-sends a choice the operator
// already made. (The same rule the web card applies with its `answered` prop.)
func TestAnAnsweredCardIsNoLongerClickable(t *testing.T) {
	m := askCardPlane(t)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "develop", Key: "u2", At: 3})
	m.onChatWake()

	row := askRowAt(t, m, "develop")
	if got, ok := m.transcriptAskOptionAtFrameRow(row); ok {
		t.Errorf("a click on an ANSWERED card resolved to %q — the question was already answered", got)
	}
}
