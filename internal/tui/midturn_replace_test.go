package tui

// midturn_replace_test.go — A DURABLE DELIVERY MID-TURN MUST NEVER WIPE LIVE-ONLY ROWS.
//
// The operator: "[my] user messages are being swallowed up when I send them and they don't print to the
// screen until the orchicon agent starts reasoning. Also, permission cards in the TUI no longer let me
// click on items."
//
// ONE CAUSE FOR BOTH. onTranscript chose merge-vs-replace on the CLIENT'S OWN STREAM SLOT alone:
//
//	if m.chat.IsStreaming(msg.ConvID) { mergeHistory } else { replace }
//
// and replace() keeps only two kinds of live-only row — a "draft-" user echo, and a PENDING consent
// card. Everything else that exists ONLY live is dropped:
//
//	a recorded clarifying-question card (KindAsk)   ← the operator's unclickable card
//	an in-flight text / reasoning chunk             ← the reply mid-sentence
//	an artifact
//
// So in the window where this client's slot is wrong while the turn is genuinely running (a slot lost,
// a turn started elsewhere, a superseding interject), a poll REPLACED the store with a durable
// transcript that cannot contain those rows yet — because the SERVER has not persisted them — and the
// operator's own message and the card vanished from screen. They came back when the server caught up,
// which is "not until the agent starts reasoning".
//
// A DURABLE TRANSCRIPT IS INCOMPLETE BY DEFINITION WHILE A TURN RUNS. The plane says so (turn_in_flight),
// and the shell already trusts that field elsewhere, so the merge decision uses BOTH halves — the same
// either-half rule the activity line uses.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE PLANE'S TURN STATE DECIDES THE MERGE, not only this client's slot.
func TestADurableDeliveryMidTurnMergesRatherThanReplaces(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")

	// THIS CLIENT'S SLOT IS WRONG (the gap state): the plane says a turn is running, the client holds no
	// stream for it. This is the state the status fetch exists to expose.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold no stream slot")
	}

	// Live-only rows that exist NOWHERE durably yet: the operator's own just-sent message, and a recorded
	// clarifying-question card the server has not written a durable row for.
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindUser, Text: "MY JUST-SENT MESSAGE", Key: "draft-123", Live: true, At: 1,
	})
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindAsk, Key: "m-ask-1", At: 2,
		Ask: &chat.ParsedAsk{Question: "Which one?", Options: []chat.AskOption{
			{Label: "a"}, {Label: "b"},
		}},
	})

	// A durable delivery that predates both — exactly what a poll returns before the server has persisted
	// them.
	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "an older message", Key: "m-old", At: 0},
	}})

	items := m.chatStore.snapshot("c1")
	var haveEcho, haveCard, haveOlder bool
	for _, it := range items {
		switch {
		case it.Key == "draft-123":
			haveEcho = true
		case it.Kind == chat.KindAsk && it.Key == "m-ask-1":
			haveCard = true
		case it.Key == "m-old":
			haveOlder = true
		}
	}
	if !haveOlder {
		t.Fatal("the durable row did not land — the delivery was not applied at all")
	}
	if !haveEcho {
		t.Errorf("THE OPERATOR'S OWN MESSAGE WAS WIPED by a durable delivery mid-turn — the operator's "+
			"\"user messages are being swallowed up when I send them\"")
	}
	if !haveCard {
		t.Errorf("AN ASK CARD WAS WIPED by a durable delivery mid-turn — the operator's card that no "+
			"longer lets them click on items (its spans are rebuilt from a store the card is no longer in)")
	}
}

// AND ONCE THE TURN IS OVER, THE DURABLE TRANSCRIPT IS THE AUTHORITY AGAIN: the replace still happens, so
// a finished turn cannot accumulate stale live rows (the completion path's whole purpose).
func TestADurableDeliveryAfterTheTurnReplaces(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}} // no turn in flight
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: no turn should be running")
	}
	// The message the operator sent, as the echo has it, and as the durable row now carries it (the wire
	// text is the context preamble prepended, which is why the dedupe matches on a SUFFIX — see
	// mergeHistory).
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindUser, Text: "MY JUST-SENT MESSAGE", Key: "draft-123", Live: true, At: 1,
	})
	m.onTranscript(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "[context]\nMY JUST-SENT MESSAGE", Key: "m-real", At: 5},
	}})
	for _, it := range m.chatStore.snapshot("c1") {
		if it.Key == "draft-123" {
			t.Errorf("the optimistic echo survived a post-turn replace — the durable view is now the " +
				"authority and it CARRIES this message, so a surviving echo would render it twice")
		}
	}
}
