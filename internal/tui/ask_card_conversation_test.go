package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// The ask-bleed regression. The operator: "I noticed a bleed through of an ask card
// from a separate conversation in the TUI." A turn's ask rides the TURN's own stream,
// so with conversation A running and B on screen the card was appended to B's slot.
// These pin that a card lands in the conversation it NAMES, and that a card parked in
// another conversation cannot take the keyboard away from the one on screen.

// TestAskCardLandsInItsOwnConversationNotTheOneOnScreen is the reported bug: an ask
// raised on c2 while c1 is displayed must not appear in c1.
func TestAskCardLandsInItsOwnConversationNotTheOneOnScreen(t *testing.T) {
	m, _ := consentApp(t) // the operator is looking at "c1"
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-c2", ConvID: "c2", Kind: chat.AskTool, Tool: "write",
		Target: "/other/main.go", Directory: "/other",
	})
	if items := m.chatStore.snapshot("c1"); len(items) != 0 {
		t.Fatalf("the ask bled into the conversation on screen: c1 carries %d item(s)", len(items))
	}
	items := m.chatStore.snapshot("c2")
	if len(items) != 1 || items[0].AskID != "ask-c2" {
		t.Fatalf("the ask must land in ITS OWN conversation c2, got %+v", items)
	}
}

// TestAskCardDoesNotClaimKeysFromAnotherConversation is the second half, and the one
// that makes the bleed more than cosmetic: the Ask screen adopts any PENDING card from
// the items the shell hands it, so a card in the wrong slot also took the keyboard.
// A card raised elsewhere must leave this conversation's composer alone.
func TestAskCardDoesNotClaimKeysFromAnotherConversation(t *testing.T) {
	m, as := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-c2", ConvID: "c2", Kind: chat.AskTool, Tool: "write",
		Target: "/other/main.go", Directory: "/other",
	})
	// The shell hands the screen the DISPLAYED conversation's items — the only hop a
	// card takes to be adopted (SyncTranscriptConsent).
	as.RenderTranscript(m.chatStore.snapshot("c1"), chat.Conversation{}, false)
	if as.ClaimsKeys() {
		t.Fatal("a card raised on another conversation must not claim this conversation's keyboard")
	}
	if got := as.ConsentPendingForTest(); got != "" {
		t.Fatalf("no card in this conversation is pending, got %q", got)
	}
}

// TestAskCardWithoutAConversationUsesTheOpenOne is the CONTROL: an ask built locally
// (no ConvID — the shape every pre-existing test and the fullsend path use) keeps the
// old behaviour, so the fix cannot silently drop a card that names no conversation.
func TestAskCardWithoutAConversationUsesTheOpenOne(t *testing.T) {
	m, _ := consentApp(t)
	m.ShowConsentAsk(chat.PermissionAsk{
		ID: "ask-local", Kind: chat.AskTool, Tool: "write",
		Target: "/home/ops/project/main.go", Directory: "/home/ops/project",
	})
	items := m.chatStore.snapshot("c1")
	if len(items) != 1 || items[0].AskID != "ask-local" {
		t.Fatalf("a local ask must land in the open conversation, got %+v", items)
	}
}
