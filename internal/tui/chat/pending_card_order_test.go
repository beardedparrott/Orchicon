package chat

import "testing"

// pending_card_order_test.go — A PROMPT IS NOT A HISTORY ROW.
//
// The ordering rule is asserted here rather than only through the store because it is the whole fix for
// the operator's report: "The cards need to be placed at the bottom of the screen so it is apparent what
// is asked of the user." A pending card pushed off the top of the pane (behind a 29,528-character
// reasoning block) cannot be answered, and the transcript follows the bottom — so being LAST is what keeps
// it on screen.

func settledCard(id string) ChatItem {
	it := ConsentItem(PermissionAsk{ID: id}, 3000)
	it.Consent.Decision = DecisionDeny
	return it
}

func TestPendingCardsLastMovesThePromptToTheEnd(t *testing.T) {
	items := []ChatItem{
		{Kind: KindUser, Text: "go", At: 1000},
		ConsentItem(PermissionAsk{ID: "a1"}, 2000),
		{Kind: KindText, Text: "working", At: 4000},
	}
	got := PendingCardsLast(items)
	if len(got) != 3 {
		t.Fatalf("no item may be lost: %v", got)
	}
	if got[2].Kind != KindConsent || got[2].AskID != "a1" {
		t.Fatalf("the pending card must be LAST, got %+v", got)
	}
	// The rest keeps its own order: this rule removes nothing and reverses nothing.
	if got[0].Kind != KindUser || got[1].Kind != KindText {
		t.Fatalf("everything else must keep its chronological order, got %+v", got)
	}
}

// A SETTLED CARD IS A RECORD, and a record belongs where it happened. Moving it would rewrite history to
// say the permission was asked for at the end of the turn.
func TestPendingCardsLastLeavesSettledCardsInPlace(t *testing.T) {
	items := []ChatItem{
		{Kind: KindUser, Text: "go", At: 1000},
		settledCard("a1"),
		{Kind: KindText, Text: "working", At: 4000},
	}
	got := PendingCardsLast(items)
	if got[1].Kind != KindConsent || got[1].AskID != "a1" {
		t.Fatalf("a settled card must stay in its own place, got %+v", got)
	}
}

// Several pending cards keep the order they arrived in — the operator answers them front to back.
func TestPendingCardsLastPreservesTheirOrderAmongThemselves(t *testing.T) {
	items := []ChatItem{
		ConsentItem(PermissionAsk{ID: "a1"}, 1000),
		{Kind: KindText, Text: "x", At: 2000},
		ConsentItem(PermissionAsk{ID: "a2"}, 3000),
	}
	got := PendingCardsLast(items)
	if got[1].AskID != "a1" || got[2].AskID != "a2" {
		t.Fatalf("pending cards keep their arrival order, got %+v", got)
	}
}

// A no-op returns the SAME slice, so the common path (nothing pending) allocates nothing.
func TestPendingCardsLastIsANoOpWhenNothingIsPending(t *testing.T) {
	items := []ChatItem{{Kind: KindUser, Text: "go", At: 1000}}
	if got := PendingCardsLast(items); &got[0] != &items[0] {
		t.Fatal("with nothing pending the same slice must be returned, not a copy")
	}
	if got := PendingCardsLast(nil); got != nil {
		t.Fatalf("nil in, nil out, got %v", got)
	}
}
