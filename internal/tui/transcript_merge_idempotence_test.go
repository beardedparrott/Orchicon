package tui

// transcript_merge_idempotence_test.go — THE DUPLICATE USER MESSAGE.
//
// The operator: "a weird bug when I click out of an ongoing session and back into it, it
// duplicated the user message in the stream." Their screenshot showed the SAME message three
// times stacked at the top of the transcript.
//
// mergeHistory folds the durable transcript under the live chunks, and it wrote its result
// back into the live store — so after ONE pass the buffer already contained the durable rows.
// The next pass appended the transcript to a buffer that already had it:
//
//	merge 1 -> 1 item    (history only; the optimistic echo was correctly dropped)
//	merge 2 -> 2 items   (the same durable row twice)
//	merge 3 -> 3 items   (the screenshot)
//
// It compounds because a mid-turn transcript load happens on EVERY RE-ENTRY into the
// conversation, which is precisely the gesture the operator described — once per click, and
// every durable row was affected, not just the user's.
//
// The existing merge tests all passed throughout, and they were right to: each of them calls
// mergeHistory ONCE. Idempotence is a property only a repeated call can show, and nothing
// asked for the second call.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE MERGE IS IDEMPOTENT: merging the same transcript again changes nothing.
func TestMergeHistoryIsIdempotent(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	const conv = "c1"
	// The operator sends a turn: the optimistic echo lands first.
	s.append(conv, chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "draft-1", Live: true, At: 10})
	// The durable transcript, as a mid-turn load returns it.
	durable := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "hello", Key: "m-1", At: 10},
		{Kind: chat.KindReasoning, Text: "thinking about it", Key: "m-2-r0", At: 11},
		{Kind: chat.KindText, Text: "the reply so far", Key: "m-2", At: 11},
	}

	s.mergeHistory(conv, durable)
	s.mergeHistory(conv, durable)
	s.mergeHistory(conv, durable)

	got := s.snapshot(conv)
	if len(got) != len(durable) {
		t.Fatalf("after three merges the buffer holds %d items, want %d — each re-entry re-appended "+
			"the transcript the previous merge had already put there:\n%v", len(got), len(durable), keysOf(got))
	}
	// BY IDENTITY, not just by count: every durable key exactly once, and NO leftover echo.
	seen := map[string]int{}
	for _, it := range got {
		seen[it.Key]++
	}
	for _, it := range durable {
		if seen[it.Key] != 1 {
			t.Errorf("key %q appears %d time(s), want exactly 1", it.Key, seen[it.Key])
		}
	}
	if seen["draft-1"] != 0 {
		t.Errorf("the optimistic echo survived a merge whose transcript contains its own words")
	}
}

// THE ECHO IS STILL DROPPED ON THE FIRST MERGE, so the fix did not buy idempotence by keeping
// both copies of the operator's message — which would have traded one duplicate for another.
func TestMergeHistoryStillDropsTheEchoOnFirstMerge(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	const conv = "c1"
	s.append(conv, chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "draft-1", Live: true, At: 10})
	s.mergeHistory(conv, []chat.ChatItem{{Kind: chat.KindUser, Text: "hello", Key: "m-1", At: 10}})

	got := s.snapshot(conv)
	if len(got) != 1 {
		t.Fatalf("items = %d (%v), want 1 — the durable copy supersedes the echo", len(got), keysOf(got))
	}
	if got[0].Key != "m-1" {
		t.Errorf("the surviving row is %q, want the durable one (m-1)", got[0].Key)
	}
}

// AN ECHO THE TRANSCRIPT DOES NOT YET CONTAIN IS KEPT, which is the case the original dedupe
// existed for: at conversation creation the transcript load races the turn that writes the
// message, and a lost echo would leave the operator's own words missing from the top of a
// brand-new conversation. Idempotence must not have broken that.
func TestMergeHistoryKeepsAnEchoTheTranscriptLacks(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	const conv = "c1"
	s.append(conv, chat.ChatItem{Kind: chat.KindUser, Text: "brand new", Key: "draft-1", Live: true, At: 10})
	// The transcript has not caught up yet: it carries an older exchange only.
	older := []chat.ChatItem{{Kind: chat.KindUser, Text: "older question", Key: "m-0", At: 5}}
	s.mergeHistory(conv, older)

	got := s.snapshot(conv)
	if len(got) != 2 {
		t.Fatalf("items = %d (%v), want 2 — the unmatched echo must survive", len(got), keysOf(got))
	}
	// AND IT INTERLEAVES BY TIME, after the older row rather than before it.
	if got[0].Key != "m-0" || got[1].Key != "draft-1" {
		t.Errorf("order = %v, want the older durable row first", keysOf(got))
	}
}

func keysOf(items []chat.ChatItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}
