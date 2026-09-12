package chat

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The operator, twice: "User messages are printing AFTER the model's messages"
// and "When sending a message it is popping up the model ON TOP of the user."
//
// Root cause: db.ListMessages orders created_at DESC — NEWEST first — and
// askorchicon.Service returns those rows unchanged. The GUI reverses the page
// (frontend/src/api/askOrchicon.ts); the TUI did not, so the transcript
// rendered inverted. This pins the reversal against a DESC page.
func TestConversationItemsAreChronological(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(sec int64) *timestamppb.Timestamp {
		return timestamppb.New(base.Add(time.Duration(sec) * time.Second))
	}

	// The server's page: NEWEST first.
	page := []*apiv1.ChatMessage{
		{Id: "m4", Role: "assistant", Content: "reply 2", CreatedAt: at(40)},
		{Id: "m3", Role: "user", Content: "question 2", CreatedAt: at(30)},
		{Id: "m2", Role: "assistant", Content: "reply 1", CreatedAt: at(20)},
		{Id: "m1", Role: "user", Content: "question 1", CreatedAt: at(10)},
	}

	items := conversationItems(page)
	want := []string{"question 1", "reply 1", "question 2", "reply 2"}
	if len(items) != len(want) {
		t.Fatalf("items = %d, want %d", len(items), len(want))
	}
	for i, w := range want {
		if items[i].Text != w {
			t.Fatalf("item %d = %q, want %q (order %v)", i, items[i].Text, w, labelled(items))
		}
	}
	// The transcript must OPEN on the operator's earliest message and END on
	// the latest reply — the two ends are what a reader perceives first.
	if items[0].Kind != KindUser || items[0].Text != "question 1" {
		t.Fatalf("the transcript must open on the operator's first message: %v", labelled(items))
	}
	if last := items[len(items)-1]; last.Kind != KindText || last.Text != "reply 2" {
		t.Fatalf("the transcript must end on the latest reply: %v", labelled(items))
	}
	// Timestamps are strictly ascending and at millisecond precision.
	for i := 1; i < len(items); i++ {
		if items[i].At <= items[i-1].At {
			t.Fatalf("timestamps not ascending at %d: %d then %d", i, items[i-1].At, items[i].At)
		}
	}
	// Sub-second precision is preserved (the old GetSeconds()*1000 truncation
	// tied same-second messages together).
	sub := []*apiv1.ChatMessage{
		{Id: "b", Role: "assistant", Content: "later", CreatedAt: timestamppb.New(base.Add(400 * time.Millisecond))},
		{Id: "a", Role: "user", Content: "earlier", CreatedAt: timestamppb.New(base.Add(100 * time.Millisecond))},
	}
	got := conversationItems(sub)
	if len(got) != 2 || got[0].Text != "earlier" {
		t.Fatalf("sub-second order lost: %v", labelled(got))
	}
	if got[1].At-got[0].At != 300 {
		t.Fatalf("sub-second delta = %d ms, want 300", got[1].At-got[0].At)
	}
}

func labelled(items []ChatItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it.Kind)+":"+it.Text)
	}
	return out
}
