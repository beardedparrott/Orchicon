package tui

// consent_focus_test.go — the two halves of "ctrl+g must let me type, and must not decide".
//
// The operator: "I hit ctrl+g to gain focus to the composer in the TUI so I could do a fullsend
// test and it registered it as a deny." Two things were wrong and they compound: the chord
// DECIDED (a false refusal, fixed in the ask screen's DropKeyClaim), and the claim then
// swallowed the typing the chord was asking for — because the shell's key gate handed every
// key to a claiming screen regardless of where focus was.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// TestCtrlGWhileACardIsPendingDoesNotDeny — the shell's own path for the chord, end to end.
func TestCtrlGWhileACardIsPendingDoesNotDeny(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/p/x.go", Directory: "/p",
	})
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("precondition: a card must be pending")
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(*App)

	// NO DECISION. The card is still pending, so the case it gated is still waiting on the
	// operator rather than refused on their behalf.
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("ctrl+g settled the card — the operator never decided anything")
	}
	st, _ := m.chatStore.consentState("c1", "ask-1"), 0
	_ = st
	item := m.chatStore.snapshot("c1")
	if len(item) == 0 || item[0].Consent == nil || !item[0].Consent.Pending() {
		t.Fatalf("the card must still be pending, got %+v", item)
	}
	// And focus did move — the chord still does its documented job.
	if m.chatFocus != focusComposer {
		t.Fatalf("ctrl+g must focus the composer, got %v", m.chatFocus)
	}
}

// TestAFocusedComposerReceivesKeysWhileACardClaimsThem is the other half, and the one that makes
// the chord USEFUL: after ctrl+g the operator can actually type — to answer, or to reach
// /fullsend while the card is up.
//
// The claim is deliberately absolute for the CONTENT pane (a typed character must not hit a
// shell route), so this pins the exception rather than weakening the rule.
func TestAFocusedComposerReceivesKeysWhileACardClaimsThem(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/p/x.go", Directory: "/p",
	})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(*App)

	// Type a character. It must land in the COMPOSER, not be eaten by the card and not become
	// a screen action.
	for _, r := range "/fullsend" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(*App)
	}
	if got := m.dock.Value(); got != "/fullsend" {
		t.Fatalf("the composer received %q, want /fullsend — the claim swallowed the typing", got)
	}
	// AND THE CARD IS STILL PENDING THROUGHOUT: typing is not answering.
	if !m.chatStore.hasPendingConsent("c1") {
		t.Fatal("typing settled the card")
	}
}

// TestWithFocusOnContentTheCardStillOwnsTheKeys guards the rule this exception exists inside:
// the claim is absolute where it matters, so a bare letter with focus on the CONTENT pane is
// the card's business and never a shell route.
func TestWithFocusOnContentTheCardStillOwnsTheKeys(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-1", Kind: chat.AskTool, Tool: "write",
		Target: "/p/x.go", Directory: "/p",
	})
	// Move focus OFF the composer the documented way, so the claim governs again.
	m.setFocus(focusContent)
	m.dock.Blur()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = next.(*App)
	if m.quitting {
		t.Fatal("'q' reached the shell's quit route while a card claimed the keys")
	}
	if strings.TrimSpace(m.dock.Value()) != "" {
		t.Fatalf("the composer collected %q with focus on content — the claim must own the keys there", m.dock.Value())
	}
}
