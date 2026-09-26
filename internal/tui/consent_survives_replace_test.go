package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// newTestChatStore builds the same store the shell builds.
func newTestChatStore() *chatStore {
	return &chatStore{items: map[string][]chat.ChatItem{}}
}

// pendingConsentItem is a live-only permission card, exactly as the wire arm
// appends one.
func pendingConsentItem(id string) chat.ChatItem {
	return chat.ConsentItem(chat.PermissionAsk{
		ID:        id,
		Tool:      "write",
		Target:    "/tmp/x.txt",
		Directory: "/tmp",
		Kind:      chat.AskTool,
	})
}

// TestReplaceKeepsAPendingConsentCard is THE bug: the permission card was drawn
// and then wiped by the next transcript poll, which is why the operator reported
// that no card ever appeared.
//
// A pending ask has NO durable row by design — the transcript records the
// OUTCOME (permission.allow / .deny / .expired), never the open ask — so the
// durable view can never carry it, and replace is its only chance to survive.
func TestReplaceKeepsAPendingConsentCard(t *testing.T) {
	s := newTestChatStore()
	s.append("c1", pendingConsentItem("ask-1"))

	// A poll lands (a completion poll, or a liveness refresh).
	s.replace("c1", []chat.ChatItem{{Kind: chat.KindText, Text: "hello", Key: "m-1"}})

	snap := s.snapshot("c1")
	var found bool
	for _, it := range snap {
		if it.Kind == chat.KindConsent && it.AskID == "ask-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the pending consent card did not survive the durable replace: %+v", snap)
	}
}

// TestReplaceDropsASettledConsentCard is the CONTROL for the rule above: only a
// PENDING card is kept. A settled one is recorded durably (or about to be), so
// keeping it would render the decision twice.
func TestReplaceDropsASettledConsentCard(t *testing.T) {
	s := newTestChatStore()
	item := pendingConsentItem("ask-1")
	// Settle it: a decision means it is no longer awaiting anyone.
	item.Consent.Decision = chat.DecisionDeny
	s.append("c1", item)

	s.replace("c1", []chat.ChatItem{{Kind: chat.KindText, Text: "hello", Key: "m-1"}})

	for _, it := range s.snapshot("c1") {
		if it.Kind == chat.KindConsent {
			t.Fatalf("a SETTLED consent card was kept by the replace: %+v", it)
		}
	}
}

// TestReplaceStillKeepsTheDraftEcho: the pre-existing rule must not have been
// disturbed by the new one — a live optimistic user echo the durable view has not
// caught up with is still kept.
func TestReplaceStillKeepsTheDraftEcho(t *testing.T) {
	s := newTestChatStore()
	s.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "my new message", Key: "draft-1"})

	s.replace("c1", []chat.ChatItem{{Kind: chat.KindText, Text: "reply", Key: "m-1"}})

	var found bool
	for _, it := range s.snapshot("c1") {
		if it.Kind == chat.KindUser && it.Text == "my new message" {
			found = true
		}
	}
	if !found {
		t.Fatal("the draft echo was dropped — the new preserve rule broke the existing one")
	}
}
