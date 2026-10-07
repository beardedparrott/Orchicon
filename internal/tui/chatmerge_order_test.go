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

// The operator: "A lot of the permission/answer logs and even some actual commands ran stays at the bottom
// of the screen and the conversation from the model continues above it. This made me think the conversation
// wasn't going anywhere... We need to have those look and feel like actual conversation logs in the
// conversation and move them up as new text streams in just like any other activity."
//
// The durable assistant row is timestamped when its ROW was created — the START of the turn — while a card
// or command row raised DURING that turn is stamped when IT arrived. Sorted by those two clocks the reply
// beats every item raised inside it for the whole turn, so it stayed pinned above them and they sat at the
// bottom of the screen. A reply that is still growing must sort by its LAST ARRIVAL instead, so the items
// raised while it was being written float above it.
func TestGrowingReplySinksBelowTheCardsRaisedDuringIt(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	// A permission card raised mid-turn (arrived at 2000ms) and the acked reply row as the server stores it
	// (created at 1500ms — the turn's start, i.e. BEFORE the card).
	s.items["c1"] = []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a1"}, 2000)}
	header := []chat.ChatItem{{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"}}
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working", At: 1500, Key: "m-2"}))

	got := s.snapshot("c1")
	if len(got) != 3 || got[0].Kind != chat.KindUser {
		t.Fatalf("first merge: %v", mergeTexts(got))
	}
	if got[2].Kind != chat.KindConsent {
		t.Fatalf("the card starts below the reply it arrived after: %v", mergeTexts(got))
	}

	// The reply GROWS — the server's 250ms mirror — which is the moment the two orderings diverge. What the
	// operator reads from here on is "the card moved up as the text streamed in".
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working steadily", At: 1500, Key: "m-2"}))

	got = s.snapshot("c1")
	if len(got) != 3 {
		t.Fatalf("the card must survive the merge: %v", mergeTexts(got))
	}
	if got[0].Kind != chat.KindUser {
		t.Fatalf("the operator's own message must stay first: %v", mergeTexts(got))
	}
	if got[1].Kind != chat.KindConsent {
		t.Fatalf("a card raised during the turn must move ABOVE the reply that is still growing, not sit at the bottom of the screen: %v", mergeTexts(got))
	}
	if got[2].Key != "m-2" {
		t.Fatalf("the growing reply must be the newest row: %v", mergeTexts(got))
	}
}

// A message's rows are one UNIT: its text, each reasoning part and its recorded ask card were written
// together, so they move together and keep the order they were emitted in (which is what keeps a recorded
// question card at the bottom of its own turn — the layout the operator asked for when the card was landing
// above the prose that followed it).
func TestAnchoredMessageKeepsItsInternalOrder(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.items["c1"] = []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a1"}, 9000)}
	msg := func(text string) []chat.ChatItem {
		return []chat.ChatItem{
			{Kind: chat.KindReasoning, Text: "thinking", At: 1500, Key: "m-2-r0"},
			{Kind: chat.KindText, Text: text, At: 1500, Key: "m-2"},
			{Kind: chat.KindAsk, Key: "m-2-ask", At: 1500},
		}
	}
	s.mergeHistory("c1", msg("short"))
	s.mergeHistory("c1", msg("much longer now"))

	got := s.snapshot("c1")
	var order []string
	for _, it := range got {
		order = append(order, it.Key)
	}
	want := []string{"consent-a1", "m-2-r0", "m-2", "m-2-ask"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (the card floats above the whole message; the message keeps its own order)", order, want)
		}
	}
}

// The anchor must NOT fire for a transcript the store is seeing for the first time: opening an old
// conversation would otherwise sort every past reply to the bottom of its own history.
func TestOpeningAnOldConversationIsNotAnchored(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	history := []chat.ChatItem{
		{Kind: chat.KindUser, Text: "q1", At: 1000, Key: "m-1"},
		{Kind: chat.KindText, Text: "a1", At: 1100, Key: "m-2"},
		{Kind: chat.KindUser, Text: "q2", At: 2000, Key: "m-3"},
		{Kind: chat.KindText, Text: "a2", At: 2100, Key: "m-4"},
	}
	s.replace("c1", history)

	got := s.snapshot("c1")
	for i := range history {
		if got[i].Key != history[i].Key {
			t.Fatalf("a loaded transcript must render in its own order: %v", mergeTexts(got))
		}
	}
}

// The anchor survives the completion replace: the in-flight reply jump back up to its row-creation time at
// the exact moment the operator starts reading the finished reply.
func TestCompletionReplaceKeepsTheAnchor(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.items["c1"] = []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a1"}, 2000)}
	header := []chat.ChatItem{{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"}}
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working", At: 1500, Key: "m-2"}))
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working steadily", At: 1500, Key: "m-2"}))

	// Turn over: the poll REPLACES the buffer with the durable transcript (the completion authority). The
	// settled reply must not jump back above the card it arrived after.
	s.replace("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"},
		{Kind: chat.KindText, Text: "working steadily", At: 1500, Key: "m-2"},
	})
	got := s.snapshot("c1")
	if len(got) != 3 || got[1].Kind != chat.KindConsent {
		t.Fatalf("the card must not jump back below the reply when the turn ends: %v", mergeTexts(got))
	}
}
