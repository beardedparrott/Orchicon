package tui

// consent_unanswered_test.go — the CLIENT half of `unanswered`.
//
// The server now publishes a question that ran out of time as `unanswered` rather than `expired`
// (see internal/askorchicon/consent_unanswered_test.go). This is why that distinction had to be
// made: this client folded `expired` into DENY, so a question the operator was away from was
// recorded as a decision they made against it — under the tool-and-target of a permission they
// were never asked about — and the question itself, the context they needed to resume, was not in
// the record at all.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// questionCardApp builds a shell holding one pending QUESTION card.
func questionCardApp(t *testing.T) *App {
	t.Helper()
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID:         "ask-q1",
		ConvID:     "c1",
		Kind:       chat.AskQuestion,
		Question:   "Which branch should the run clone off?",
		Options:    []string{"develop", "main"},
		AllowOther: true,
	})
	return m
}

// cardRecord renders the ask's settled line, which is what the operator reads when they return.
func cardRecord(t *testing.T, m *App, id string) string {
	t.Helper()
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Kind == chat.KindConsent && it.AskID == id {
			return chat.ConsentCardText(it, 200)
		}
	}
	t.Fatalf("no card for ask %s", id)
	return ""
}

// TestAQuestionThatRanOutOfTimeIsUnansweredNotDenied is the reporting half: the record must say
// nobody answered it, must name the question, and must not claim a denial.
func TestAQuestionThatRanOutOfTimeIsUnansweredNotDenied(t *testing.T) {
	m := questionCardApp(t)
	m.chatStore.settleAsk("c1", "ask-q1", "unanswered", "")

	got := cardRecord(t, m, "ask-q1")
	if !strings.Contains(got, "unanswered") {
		t.Errorf("a question nobody answered must be recorded as unanswered, got %q", got)
	}
	if !strings.Contains(got, "Which branch should the run clone off?") {
		t.Errorf("the record must name the question — the question IS the context that has to "+
			"survive, got %q", got)
	}
	if strings.Contains(got, "Deny") || strings.Contains(got, "deny") {
		t.Errorf("a question has no allow/deny to decide, so it must not read as a refusal, got %q", got)
	}
	// The decision on the item is the same fact, for any other reader.
	for _, it := range m.chatStore.snapshot("c1") {
		if it.AskID == "ask-q1" && (it.Consent == nil || it.Consent.Decision != chat.DecisionUnanswered) {
			t.Errorf("the card's decision = %v, want %q", it.Consent, chat.DecisionUnanswered)
		}
	}
}

// AND `expired` STILL MEANS DENIED, the control: for a PERMISSION the serve really was answered
// `reject`, so the call was refused and the record saying so is true. The two outcomes are
// deliberately different because the two asks are.
func TestAnExpiredPermissionStillReadsAsDenied(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-p1", ConvID: "c1", Kind: chat.AskTool, Tool: "write", Target: "/p/x.go", Directory: "/p",
	})
	m.chatStore.settleAsk("c1", "ask-p1", "expired", "")

	got := cardRecord(t, m, "ask-p1")
	if !strings.Contains(got, "Deny") {
		t.Errorf("an expired permission reads %q, want the denial the serve was actually given", got)
	}
}
