package tui

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// Regression: the operator's message rendered TWICE and at the BOTTOM.
// The composer appends an optimistic user row ("draft-*") on send; the durable
// transcript then arrives containing that same user message, and mergeHistory
// concatenated history ++ live — so the text appeared twice and the optimistic
// copy (appended last) landed below the reply.
func TestMergeHistoryDedupesOptimisticUserRow(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	// Optimistic send, then the model's streamed reply.
	s.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "draft-1", Live: true})
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "hi back", Key: "st-1", Live: true})

	// The durable transcript lands (oldest-first): user, then assistant.
	s.mergeHistory("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "hello", Key: "m-1"},
		{Kind: chat.KindText, Text: "hi back", Key: "m-2"},
	})

	got := s.snapshot("c1")
	users := 0
	for _, it := range got {
		if it.Kind == chat.KindUser && strings.Contains(it.Text, "hello") {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user message rendered %d times, want exactly 1: %+v", users, got)
	}
	// Ordering: the user row must come BEFORE the assistant's reply.
	if got[0].Kind != chat.KindUser {
		t.Fatalf("first item must be the user's message, got kind %v: %+v", got[0].Kind, got)
	}
	// A live-only row (not yet persisted) must survive the merge.
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "still streaming", Key: "st-9", Live: true})
	s.mergeHistory("c1", []chat.ChatItem{{Kind: chat.KindUser, Text: "hello", Key: "m-1"}})
	found := false
	for _, it := range s.snapshot("c1") {
		if strings.Contains(it.Text, "still streaming") {
			found = true
		}
	}
	if !found {
		t.Fatal("a live row that history does not cover must survive the merge")
	}
}

// Regression for the operator's "it sent my message twice and put it below the
// model's response".
//
// The message that reaches the plane carries the context preamble prepended
// (Controller.Send: full = preamble + "\n" + text), while the optimistic row
// carries the RAW text. The dedupe originally compared them for EQUALITY, so it
// never matched when a context was attached — and since live rows are appended
// after history, the surviving optimistic copy landed below the reply.
func TestMergeHistoryDedupesWithContextPreamble(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	const raw = "What am I looking at?"
	const persisted = "[context: Themes: ember]\n" + raw

	// The optimistic echo the composer appended (raw text), then a live chunk.
	s.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: raw, Key: "draft-1", Live: true})
	s.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "partial…", Key: "st-1", Live: true})

	// The durable transcript arrives with the PREAMBLE-prefixed user message.
	s.mergeHistory("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: persisted, Key: "m-1"},
		{Kind: chat.KindText, Text: "the answer", Key: "m-2"},
	})

	got := s.snapshot("c1")
	users := 0
	firstUser := -1
	for i, it := range got {
		if it.Kind == chat.KindUser {
			users++
			if firstUser < 0 {
				firstUser = i
			}
		}
	}
	if users != 1 {
		t.Fatalf("user message rendered %d times, want exactly 1: %+v", users, got)
	}
	if firstUser != 0 {
		t.Fatalf("the user row must come first, got index %d: %+v", firstUser, got)
	}
	// The surviving copy is the DURABLE one (so the transcript matches the
	// conversation's stored history, preamble and all).
	if got[0].Text != persisted {
		t.Fatalf("kept user row = %q, want the durable text", got[0].Text)
	}
}

// Regression: a durable poll that lands BEFORE the operator's message is visible
// to it must not erase that message.
//
// replace() is the completion authority and swaps the store for the durable
// transcript, which is right once the turn resolves. But the transcript load at
// conversation CREATION races the turn that writes the user row, so an early —
// legitimately EMPTY — fetch used to wipe the optimistic echo. The conversation
// then rendered only the reply, and because the first-send path skipped its
// durable reload the operator's opening message never came back: the reported
// "it is still not showing the initial user message on a new conversation".
//
// The rule: an unmatched optimistic echo SURVIVES a replace, and the durable copy
// supersedes it the moment the durable list actually contains it (exactly one copy
// either way).
func TestReplaceKeepsAnUnmatchedOptimisticEcho(t *testing.T) {
	m := newTestApp()
	const conv = "c1"
	user := chat.ChatItem{Kind: chat.KindUser, Text: "test", Key: "draft-1", Live: true}
	reply := chat.ChatItem{Kind: chat.KindText, Text: "reply", Key: "m-2", Live: true}

	m.chatStore.append(conv, user)

	// An early durable fetch that has not seen the user row yet.
	m.chatStore.replace(conv, nil)
	got := m.chatStore.snapshot(conv)
	if len(got) != 1 || got[0].Text != "test" {
		t.Fatalf("an early EMPTY poll wiped the just-sent message: %+v", got)
	}

	// The reply streams in.
	m.chatStore.append(conv, reply)
	if got := m.chatStore.snapshot(conv); len(got) != 2 {
		t.Fatalf("store = %+v, want the user echo plus the reply", got)
	}

	// A poll that DOES contain the message supersedes the echo — one copy, not two.
	m.chatStore.replace(conv, []chat.ChatItem{
		{Kind: chat.KindUser, Text: "test", Key: "m-1"},
		{Kind: chat.KindText, Text: "reply", Key: "m-2"},
	})
	got = m.chatStore.snapshot(conv)
	if len(got) != 2 {
		t.Fatalf("store = %+v, want exactly two rows (no duplicate echo)", got)
	}
	users := 0
	for _, it := range got {
		if it.Kind == chat.KindUser {
			users++
			if !strings.HasPrefix(it.Key, "m-") {
				t.Errorf("the surviving user row must be the DURABLE one, got key %q", it.Key)
			}
		}
	}
	if users != 1 {
		t.Fatalf("found %d user rows, want exactly 1", users)
	}
}
