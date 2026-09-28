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
	}, 1000)
}

// TestConsentCardSortsToTheBottomAndStaysThere is the operator's symptom as a
// test: "the card comes up for a fraction of a second and then disappears and
// then the composer is locked and you can't type anything."
//
// One cause. The card was built with NO timestamp, and mergeHistory/replace both
// re-sort the transcript oldest-first — so the next poll threw it to the very TOP
// of the conversation, out of view at the bottom, while the screen went on
// claiming the keyboard because the item was still there.
func TestConsentCardSortsToTheBottomAndStaysThere(t *testing.T) {
	s := newTestChatStore()

	// A conversation already under way, then the card arrives.
	s.replace("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "earlier", At: 100, Key: "m-1"},
		{Kind: chat.KindText, Text: "older reply", At: 200, Key: "m-2"},
	})
	s.append("c1", pendingConsentItem("ask-late"))

	// A poll lands (the completion poll, or a liveness refresh).
	s.replace("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "earlier", At: 100, Key: "m-1"},
		{Kind: chat.KindText, Text: "older reply", At: 200, Key: "m-2"},
		{Kind: chat.KindText, Text: "newer reply", At: 300, Key: "m-3"},
	})

	snap := s.snapshot("c1")
	last := snap[len(snap)-1]
	if last.Kind != chat.KindConsent {
		t.Fatalf("the card is not the LAST item after a poll — it is at %d of %d: %+v",
			len(snap)-1, len(snap), dumpKinds(snap))
	}
}

// TestConsentCardSortsBelowTheReplyItInterrupted: the same rule against the
// CHRONOLOGY it actually shares with the turn — the card must sit after the text
// that preceded it, not above it.
func TestConsentCardSortsBelowTheReplyItInterrupted(t *testing.T) {
	items := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "do the thing", At: 100, Key: "m-1"},
		pendingConsentItem("ask-mid"),
		{Kind: chat.KindText, Text: "working on it", At: 200, Key: "m-2"},
	}
	chat.SortChronologically(items)
	// The test item is stamped 1000, so it is the newest thing here.
	if items[len(items)-1].Kind != chat.KindConsent {
		t.Fatalf("the card did not sort last: %+v", dumpKinds(items))
	}
}

// dumpKinds renders a compact view of an item list for a failure message.
func dumpKinds(items []chat.ChatItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.Kind))
	}
	return out
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
