package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// consent_waiting_footer_test.go — THE FOOTER MUST NAME THE CARD THE TURN IS PARKED ON.
//
// The reported symptom, in the operator's words: "I will notice no progress being made for a long
// time and if I exit the TUI and relaunch orch, a permissions card is sitting waiting for me when I
// relaunch it."
//
// The pane's fixed footer is the one row that is always visible, and it was reading the STREAM's
// silence — so a turn parked on a permission ask was drawn as "Orchicon is <verb>… · no output for
// 40s — the stream will re-attach if it stays silent". That sentence blames the stream and promises
// a re-attach while the stream is healthy and the turn is waiting on the OPERATOR: the operator is
// told nothing is happening, and the only thing that would re-present the card was a fresh attach.
//
// So the footer now says what it is waiting for. It is asserted through transcriptStatusLine — the
// function that BUILDS the row (and the one onChatWake hands to SetDetailFooter) — because the full
// pty path needs the e2e harness.

func parkedWithCard(t *testing.T) *App {
	t.Helper()
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "main.go", Directory: "/home/ops/project",
	})
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: the card must be pending")
	}
	return m
}

func TestTheFooterNamesTheCardTheTurnIsWaitingOn(t *testing.T) {
	m := parkedWithCard(t)

	got := m.transcriptStatusLine(nil)

	if !strings.Contains(got, "waiting for your approval") {
		t.Errorf("the footer must say it is waiting on the operator, got %q", got)
	}
	if !strings.Contains(got, "write main.go") {
		t.Errorf("the footer must name WHAT is being asked about, got %q", got)
	}
	// THE REGRESSION: the silence band must not be reached at all while a card is pending. It is the
	// sentence that made a parked turn read as a stalled one.
	if strings.Contains(got, "no output for") {
		t.Errorf("the footer blamed the stream while a card was waiting, got %q", got)
	}
	if strings.Contains(got, "Orchicon is ") {
		t.Errorf("the footer drew the rotating activity verb while a card was waiting, got %q", got)
	}
}

// A QUESTION CARD IS NOT AN APPROVAL, and the footer must not call it one — the operator's own words
// differ ("the question it asked me"), and the card's kinds do too (chat.AskQuestion).
func TestTheFooterSaysAnswerForAQuestionCard(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-q", Kind: chat.AskQuestion,
		Question: "which project?",
	})

	got := m.transcriptStatusLine(nil)

	if !strings.Contains(got, "waiting for your answer") {
		t.Errorf("a question card is not an approval, got %q", got)
	}
	if strings.Contains(got, "approval") {
		t.Errorf("the footer called a question an approval, got %q", got)
	}
	if !strings.Contains(got, "which project?") {
		t.Errorf("the footer must name the question, got %q", got)
	}
}

// ONE ROW, ALWAYS — the footer's own contract (the pane draws a single row and clips the tail, which is why
// the activity line is fitted too). A long subject must therefore degrade WITHIN the row rather than push the
// hint off the edge, and the subject is dropped to a truncated form before it is dropped entirely.
func TestTheWaitingLineAlwaysFitsOneRow(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-long", Kind: chat.AskTool, Tool: "bash",
		Target: "cd /home/ops/project && npm run build --workspace=frontend --if-present",
	})

	got := m.transcriptStatusLine(nil)
	width := m.askWidth()
	if width <= 0 {
		// Unmeasured pane: the line is returned whole, and the invariant below has nothing to check against.
		if !strings.Contains(got, "waiting for your approval") {
			t.Fatalf("unmeasured pane must still name the wait, got %q", got)
		}
		return
	}
	if w := ansi.StringWidth(got); w > width {
		t.Errorf("the waiting line is %d cells in a %d-cell pane — it would wrap or clip: %q", w, width, got)
	}
	if !strings.Contains(got, "waiting for your approval") {
		t.Errorf("the label must survive the fit at any width that fits it, got %q", got)
	}
}

// PRECEDENCE IS PRESERVED: a card cannot be answered across a dead plane, so the connection arms
// still outrank it — the order the pane already documents.
func TestReconnectingStillOutranksTheCard(t *testing.T) {
	m := parkedWithCard(t)
	m.chatStore.setReconnecting("c1", true)

	if got := m.transcriptStatusLine(nil); got != "reconnecting…" {
		t.Errorf("reconnecting must outrank a waiting card, got %q", got)
	}
}

// AND NOTHING LEAKS WHEN THERE IS NO CARD: the footer falls through to its ordinary arms, so this
// addition cannot quietly take over a pane with no ask on it.
func TestNoCardMeansNoWaitingLine(t *testing.T) {
	m, _ := consentApp(t)

	if got := m.transcriptStatusLine(nil); strings.Contains(got, "waiting for your") {
		t.Errorf("no card is pending, so nothing may claim one is: %q", got)
	}
}
