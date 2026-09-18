package tui

// turn_poll_test.go — LIVE STREAMING THAT DOES NOT DEPEND ON THE LIVE STREAM.
//
// The operator, on a pane that showed the watchdog's age resetting (so events WERE arriving) but never
// the reply: "We NEED live streaming working... I don't even care if you have to find a brand new
// method." The method was already in the server, built for exactly this: the running turn's collected
// text, reasoning and tool ledger are mirrored into the ACKED assistant message every 250ms
// (askorchicon.upsertPartialMessage), so a client that lost the socket — a refresh, another device —
// polls ListMessages and WATCHES THE REPLY GROW.
//
// So the TUI polls during the turn. It does not replace the socket (which is faster when it works); it
// removes the socket's ability to be the single point of failure for liveness. This file covers the
// MERGE, which is where a poll can go wrong; the scheduling is covered beside the controller in
// internal/tui/chat/turn_poll_test.go.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// A GROWING PARTIAL RENDERS AS A GROWING REPLY — the whole point, and the property that makes the poll
// safe to run once a second.
//
// Each poll returns the same assistant message id carrying MORE text. The merge must show the newest
// content, once, in place: the previous generation of that key is already in the live buffer, so a
// merge that could not tell "already seen" from "new" would stack generations instead of advancing
// one. (That is the duplicate-message bug, which is why the idempotence fix had to land first — and it
// is why this test failing would look exactly like that bug returning.)
func TestAPolledPartialReplacesItsOwnEarlierGeneration(t *testing.T) {
	s := &chatStore{items: map[string][]chat.ChatItem{}}
	const conv = "c1"

	s.append(conv, chat.ChatItem{Kind: chat.KindUser, Text: "explain the design", Key: "draft-1", Live: true, At: 10})

	// Three polls of the SAME turn, the assistant row growing each time — exactly what the 250ms
	// mirror produces.
	polls := [][]chat.ChatItem{
		{
			{Kind: chat.KindUser, Text: "explain the design", Key: "m-1", At: 10},
			{Kind: chat.KindReasoning, Text: "weighing options", Key: "m-2-r0", At: 11},
			{Kind: chat.KindText, Text: "The design has", Key: "m-2", At: 11},
		},
		{
			{Kind: chat.KindUser, Text: "explain the design", Key: "m-1", At: 10},
			{Kind: chat.KindReasoning, Text: "weighing options", Key: "m-2-r0", At: 11},
			{Kind: chat.KindText, Text: "The design has three parts", Key: "m-2", At: 11},
		},
		{
			{Kind: chat.KindUser, Text: "explain the design", Key: "m-1", At: 10},
			{Kind: chat.KindReasoning, Text: "weighing options", Key: "m-2-r0", At: 11},
			{Kind: chat.KindText, Text: "The design has three parts, and the first is the writer.", Key: "m-2", At: 11},
		},
	}
	for i, p := range polls {
		s.mergeHistory(conv, p)
		got := s.snapshot(conv)
		if len(got) != len(p) {
			t.Fatalf("after poll %d the buffer holds %d items, want %d — polls must ADVANCE the reply, "+
				"not stack generations of it:\n%v", i+1, len(got), len(p), keysOf(got))
		}
	}
	got := s.snapshot(conv)
	if got[2].Text != "The design has three parts, and the first is the writer." {
		t.Errorf("the rendered reply is %q, want the newest poll's text", got[2].Text)
	}
	// The reasoning part came through too — it is equally mirrored, and the transcript must show it,
	// which is the other half of the operator's report ("no reasoning... showing up").
	if got[1].Kind != chat.KindReasoning || got[1].Text != "weighing options" {
		t.Errorf("the polled reasoning did not render: %+v", got[1])
	}
	// The optimistic echo is gone: the durable copy superseded it.
	for _, it := range got {
		if it.Key == "draft-1" {
			t.Error("the optimistic echo survived a poll that carried its own message")
		}
	}
}

// A POLL DURING THE TURN IS THE SAME SHAPE AS THE COMPLETION POLL, so the transcript that grows while
// streaming and the transcript that lands at the end cannot disagree.
//
// Both go through ListMessages → conversationItems → GroupByPhase, so the ordering guarantee they share
// (reversal out of the server's newest-first page, millisecond timestamps, stable same-instant order)
// is asserted once, where that mapping lives: internal/tui/chat (conversationItems, order_test.go and
// reasoning_history_test.go). It is named here so a reader of this file knows the interleaving —
// growing partial, then finalized reply — is covered rather than overlooked.
