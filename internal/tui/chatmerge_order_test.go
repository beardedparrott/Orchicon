package tui

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// The operator: "User messages are printing AFTER the model's messages."
//
// mergeHistory merged the durable transcript with the live buffer by
// CONCATENATION (history then live), so any live row the dedupe did not drop
// landed at the BOTTOM — below the model's reply. The combined list must be
// interleaved by timestamp instead.
func TestMergeHistoryKeepsChronologicalOrder(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	// A live user row the dedupe cannot match (its text differs from the
	// durable copy), timestamped BETWEEN the durable user row and the reply.
	s.items["c1"] = []chat.ChatItem{
		{Kind: chat.KindUser, Text: "live user", At: 1500, Key: "draft-1"},
	}
	history := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "durable user", At: 1000},
		{Kind: chat.KindText, Text: "model reply", At: 2000},
	}
	s.mergeHistory("c1", history)

	got := s.snapshot("c1")
	if len(got) != 3 {
		t.Fatalf("merged items = %d, want 3", len(got))
	}
	want := []string{"durable user", "live user", "model reply"}
	for i, w := range want {
		if got[i].Text != w {
			t.Fatalf("item %d = %q, want %q (order %v)", i, got[i].Text, w, mergeTexts(got))
		}
	}
	// The specific regression: nothing user-authored may end up after the reply.
	if last := got[len(got)-1]; last.Kind == chat.KindUser {
		t.Fatalf("a user row is the LAST item, after the model reply: %v", mergeTexts(got))
	}
}

// The dedupe still suppresses the optimistic echo AND the surviving order stays
// chronological.
func TestMergeHistoryDedupesOptimisticEchoInPlace(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.items["c1"] = []chat.ChatItem{
		{Kind: chat.KindUser, Text: "hello", At: 1200, Key: "draft-9"},
	}
	// The durable copy carries the injected context preamble, so the match is a
	// suffix match — and the reply is newer than both.
	history := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "[context: work items]\nhello", At: 1000},
		{Kind: chat.KindText, Text: "reply", At: 2000},
	}
	s.mergeHistory("c1", history)

	got := s.snapshot("c1")
	if len(got) != 2 {
		t.Fatalf("the optimistic echo must be dropped: %v", mergeTexts(got))
	}
	if got[0].Kind != chat.KindUser || got[1].Kind != chat.KindText {
		t.Fatalf("order must be user then reply: %v", mergeTexts(got))
	}
}

func mergeTexts(items []chat.ChatItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.Kind)+":"+it.Text)
	}
	return out
}
