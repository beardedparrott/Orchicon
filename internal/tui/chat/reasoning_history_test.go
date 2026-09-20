package chat

// reasoning_history_test.go — THE DURABLE REASONING REACHES THE TRANSCRIPT.
//
// The operator: "All I am seeing is 'Orchicon is thinking...' in the TUI. I am not seeing your
// thinking or reasoning. I can see this in the GUI. I already reported this and you said I was
// wrong. Here is the screenshot for proof."
//
// THEY WERE RIGHT AND I WAS WRONG. ChatMessage.reasoning is a `repeated string` the proto
// documents as "rendered by the frontend as thinking bubbles", and the GUI reads it — but
// conversationItems read only GetContent(), so every durable reasoning part was DROPPED the
// moment the transcript was loaded from history. The live stream chunks were the only path that
// ever produced a reasoning item, and the completion poll REPLACES the live buffer with the
// durable list (chatStore.replace), so the reasoning the operator had just watched arrive
// vanished the instant the turn finished. The thinking NOTICE, which is live, survived — which is
// exactly what they described seeing.
//
// These tests live beside conversationItems because that function is where the data was lost;
// the rendering of a reasoning ITEM was already correct and already covered.

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// EVERY REASONING PART BECOMES AN ITEM, and it comes BEFORE the words it produced.
//
// Order is asserted separately from presence because it is the half that is easy to get wrong
// while looking right: reasoning rendered AFTER the answer reads as an afterthought rather than
// as the thinking that preceded it.
func TestDurableReasoningBecomesItemsBeforeTheText(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	page := []*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant", Content: "the answer", CreatedAt: at,
			Reasoning: []string{"first thought", "second thought"}},
		{Id: "m1", Role: "user", Content: "the question", CreatedAt: at},
	}

	items := conversationItems(page)

	var kinds []ItemKind
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	want := []ItemKind{KindUser, KindReasoning, KindReasoning, KindText}
	if len(items) != len(want) {
		t.Fatalf("items = %d (%v), want %d: %v", len(items), kinds, len(want), want)
	}
	for i, w := range want {
		if items[i].Kind != w {
			t.Fatalf("item %d kind = %q, want %q — the reasoning must come BEFORE the answer it "+
				"produced, and both must survive the load", i, items[i].Kind, w)
		}
	}
	if items[1].Text != "first thought" || items[2].Text != "second thought" {
		t.Errorf("reasoning parts are %q and %q, want them in the order the turn produced them",
			items[1].Text, items[2].Text)
	}
	// The last item is the answer, unchanged — the reasoning must not have taken its place.
	if items[3].Text != "the answer" {
		t.Errorf("the assistant text is %q, want %q", items[3].Text, "the answer")
	}
	if items[3].Live {
		t.Error("a durable reasoning/text row is marked Live, which is for streamed chunks only")
	}
}

// KEYS ARE STABLE ACROSS LOADS, derived from the message id and the part index.
//
// This is not cosmetic: a reasoning block's collapsed/expanded state is held per ITEM KEY
// (chatStore.foldedReasoning), so a key that changed between loads would spring every collapsed
// block open again on every refresh — the operator folds a long block away and it comes back.
func TestReasoningKeysAreStableAndUnique(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	page := []*apiv1.ChatMessage{
		{Id: "m1", Role: "assistant", Content: "a", CreatedAt: at, Reasoning: []string{"r1", "r2"}},
		{Id: "m2", Role: "assistant", Content: "b", CreatedAt: at, Reasoning: []string{"r3"}},
	}

	first := conversationItems(page)
	second := conversationItems(page)

	seen := map[string]bool{}
	for i, it := range first {
		if it.Key == "" {
			t.Fatalf("item %d (%q) has no key", i, it.Kind)
		}
		if seen[it.Key] {
			t.Fatalf("duplicate key %q — two items would share fold state", it.Key)
		}
		seen[it.Key] = true
		if second[i].Key != it.Key {
			t.Fatalf("the key for item %d changed between loads: %q then %q — a collapsed reasoning "+
				"block would reopen on every refresh", i, it.Key, second[i].Key)
		}
	}
	// The text row keeps its own key, so a reasoning key can never collide with it. The
	// convention is "m-" + the message id for the text row, and "m-" + id + "-r" + index for
	// each part — asserted as the real strings rather than a prefix check, because the two must
	// stay distinguishable for the fold state (keyed per item) to land on the right row.
	if !seen["m-m1"] {
		t.Errorf("the assistant text row's key %q is missing; keys seen: %v", "m-m1", seen)
	}
	if !seen["m-m1-r0"] || !seen["m-m1-r1"] {
		t.Errorf("the reasoning part keys are missing; keys seen: %v", seen)
	}
}

// A BLANK OR ABSENT REASONING PART IS NOT A BLOCK.
//
// An empty reasoning entry would render an empty bubble — a fold affordance over nothing — and
// every turn with no reasoning at all would grow one.
func TestBlankReasoningPartsAreSkipped(t *testing.T) {
	at := timestamppb.New(time.Unix(1_700_000_000, 0))
	page := []*apiv1.ChatMessage{
		{Id: "m1", Role: "assistant", Content: "answer", CreatedAt: at,
			Reasoning: []string{"", "   ", "\n", "real thought"}},
	}
	items := conversationItems(page)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (the reasoning part and the text): %v", len(items), items)
	}
	if items[0].Kind != KindReasoning || items[0].Text != "real thought" {
		t.Errorf("first item = %+v, want the one non-blank reasoning part", items[0])
	}

	// No reasoning at all: just the text, and nothing invented.
	plain := conversationItems([]*apiv1.ChatMessage{
		{Id: "m2", Role: "assistant", Content: "answer", CreatedAt: at},
	})
	if len(plain) != 1 || plain[0].Kind != KindText {
		t.Errorf("a message with no reasoning produced %d item(s): %v", len(plain), plain)
	}
}
