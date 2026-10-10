package tui

import (
	"strings"
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
// THE RULE THIS PINS WAS REVERSED, deliberately, on the operator's report.
//
// It used to assert that a growing reply SINKS BELOW the card raised during it — so the card moved up as
// text streamed in, which read as "the card moves out of the way of the reply". The operator's
// screenshot killed that: with a 29,528-character reasoning block the card travelled off the TOP of the
// pane, and the only part left on screen was the card's own hint row ("↑/↓ select · enter confirm · esc
// denies") while the question it belonged to was nowhere visible. Their instruction: "The cards need to
// be placed at the bottom of the screen so it is apparent what is asked of the user."
//
// So the assertion is inverted, and the inversion is the point: the card is the PROMPT, the reply is
// HISTORY, and while the turn is blocked on an answer the prompt outranks it for the bottom of the pane.
func TestAPendingCardStaysAtTheBottomWhileTheReplyGrows(t *testing.T) {
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
	if got[1].Key != "m-2" {
		t.Fatalf("the growing reply must sit above the card once the card is pinned to the bottom: %v", mergeTexts(got))
	}
	if got[2].Kind != chat.KindConsent {
		t.Fatalf("a PENDING card must stay at the BOTTOM while the reply grows — every row that moves above "+
			"it pushes it further up, and a card off the top of the pane cannot be answered: %v", mergeTexts(got))
	}
}

// THE REPORTED CASE, at its actual scale: a 29,528-character reasoning block and a pending card. The card
// must be the last row — that is what keeps it inside the pane, because the transcript follows the bottom
// (kit2.Stream.AtBottom) and no amount of growth above it can then push it off.
func TestAPendingCardSurvivesAHugeGrowingReply(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.items["c1"] = []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a1"}, 2000)}
	header := []chat.ChatItem{{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"}}
	reasoning := chat.ChatItem{Kind: chat.KindReasoning, Text: strings.Repeat("thinking. ", 1000), At: 1500, Key: "m-2-r0"}
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...), reasoning))

	// And it keeps growing, poll after poll, which is what used to walk the card off the screen.
	for i := 0; i < 3; i++ {
		reasoning.Text += strings.Repeat("more. ", 1000)
		s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...), reasoning))
	}

	got := s.snapshot("c1")
	if len(got) != 3 {
		t.Fatalf("want user, reasoning, card — got %v", mergeTexts(got))
	}
	if got[2].Kind != chat.KindConsent {
		t.Fatalf("the pending card must be the LAST row however long the reply grows: %v", mergeTexts(got))
	}
}

// A message's rows are one UNIT: its text, each reasoning part and its recorded ask card were written
// together, so they move together and keep the order they were emitted in (which is what keeps a recorded
// question card at the bottom of its own turn).
//
// THE CARD'S OWN POSITION IS NOT PART OF THAT UNIT, and the distinction is what this asserts after the
// operator's reversal. A message's rows describe something that ALREADY HAPPENED, so their internal order
// is fixed. A pending card is a PROMPT — it has not happened yet — so it is moved out of the message and
// pinned to the bottom of the pane (see chat.PendingCardsLast), which is the reported fix. The message
// keeps its order; the card leaves its slot for the bottom.
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
	// The message keeps its own order (reasoning, text, its recorded ask) — the guarantee this test has
	// always protected — and the PENDING card is moved to the end of the list.
	want := []string{"m-2-r0", "m-2", "m-2-ask", "consent-a1"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (the message keeps its own order; the pending card is pinned "+
				"to the bottom of the pane)", order, want)
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

// The anchor survives the completion replace: the in-flight reply must not jump back up to its
// row-creation time at the exact moment the operator starts reading the finished reply.
//
// THE GUARANTEE IS STABILITY, NOT A PARTICULAR POSITION, and it is asserted that way here: the order is
// captured BEFORE the completion replace and required to be identical after it. Previously that order had
// the pending card above the reply; now it has the card at the bottom — and because the rule is the same on
// both sides of the boundary, the transition is still a non-event for the operator. What this test will not
// tolerate is the card appearing to move as the turn ends.
func TestCompletionReplaceKeepsTheAnchor(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	s.items["c1"] = []chat.ChatItem{chat.ConsentItem(chat.PermissionAsk{ID: "a1"}, 2000)}
	header := []chat.ChatItem{{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"}}
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working", At: 1500, Key: "m-2"}))
	s.mergeHistory("c1", append(append([]chat.ChatItem{}, header...),
		chat.ChatItem{Kind: chat.KindText, Text: "working steadily", At: 1500, Key: "m-2"}))

	before := keysOf(s.snapshot("c1"))

	// Turn over: the poll REPLACES the buffer with the durable transcript (the completion authority).
	s.replace("c1", []chat.ChatItem{
		{Kind: chat.KindUser, Text: "go", At: 1000, Key: "m-1"},
		{Kind: chat.KindText, Text: "working steadily", At: 1500, Key: "m-2"},
	})
	got := s.snapshot("c1")
	if len(got) != 3 {
		t.Fatalf("want user, reply, card — got %v", mergeTexts(got))
	}
	// The pending card is at the bottom on BOTH sides of the boundary...
	if got[2].Kind != chat.KindConsent {
		t.Fatalf("the pending card must be the last row, got %v", mergeTexts(got))
	}
	// ...so nothing moved as the turn ended.
	after := keysOf(got)
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("the order changed at the turn boundary: %v -> %v (the operator watches the card "+
				"move as the reply completes)", before, after)
		}
	}
}
