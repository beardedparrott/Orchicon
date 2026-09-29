package tui

// reattach_from_rail_test.go — the TUI attaches to a turn it did not start.
//
// The operator, twice: "the permission ask card pops up in the GUI but it doesn't pop up in
// the TUI. It just sits at 'orchicon is thinking'. ... It's not consistent in the TUI."
//
// A turn's asks ride that turn's stream and asks are STREAM-ONLY (the transcript records an
// ask's OUTCOME, never the open ask), so a client with no stream for the turn can neither
// receive a card nor poll one back. This client opened a stream only when the TUI ITSELF sent,
// or when a conversation was OPENED that the rail said was mid-turn — so a turn started in the
// other client, while the TUI sat on that very conversation, was invisible here.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// railFor builds the shell state a rail reload arrives in: the conversation is OPEN, the rail
// has loaded before, so the reload takes the normal path rather than the project self-heal.
func railFor(t *testing.T) *App {
	t.Helper()
	m, _ := consentApp(t)
	m.chatConvID = "c1"
	m.convLoaded = true
	m.railProjectsLoaded = true
	return m
}

// TestRailReloadAttachesToATurnStartedInTheOtherClient is the reported bug. The rail reports a
// turn running on the conversation already on screen, with a pending assistant id this client
// never acked — the shape of a message sent from the GUI. The TUI must ATTACH, because that is
// the only way a card raised on that turn can reach it.
func TestRailReloadAttachesToATurnStartedInTheOtherClient(t *testing.T) {
	m := railFor(t)
	if m.chat.IsStreaming("c1") {
		t.Fatal("precondition: this client has no stream for c1")
	}

	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{
		{ID: "c1", TurnInFly: true, PendingReplyID: "m-reply-from-the-gui"},
	}})

	// The slot is set by Reattach BEFORE it returns the Watch command, so this asserts the
	// attach happened without needing a server to dial.
	if !m.chat.IsStreaming("c1") {
		t.Fatal("the TUI did not attach to a turn it never started — a card raised on it (or a nudge to answer one) can never reach this client, because a permission ask has no durable row to poll")
	}
}

// The control: a rail row with NOTHING to re-attach to must not invent a slot. `turn_in_flight`
// alone is not enough — the Watch RPC needs the assistant message id, and attaching without one
// would address a turn that cannot be named.
func TestRailReloadWithoutAPendingReplyDoesNotAttach(t *testing.T) {
	m := railFor(t)

	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{
		{ID: "c1", TurnInFly: true, PendingReplyID: ""},
	}})

	if m.chat.IsStreaming("c1") {
		t.Fatal("a running turn with no pending assistant id must not be attached to: there would be no turn to address")
	}
}

// And an idle conversation attaches to nothing.
func TestRailReloadOnAnIdleConversationDoesNotAttach(t *testing.T) {
	m := railFor(t)

	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{
		{ID: "c1", TurnInFly: false, PendingReplyID: ""},
	}})

	if m.chat.IsStreaming("c1") {
		t.Fatal("an idle conversation must not open a stream")
	}
}

// The reload must not disturb a turn THIS client is running: Reattach refuses when a live local
// stream owns the slot, so a turn started here keeps its own stream and the reload only ever
// fills a gap.
func TestRailReloadLeavesALocallyStartedTurnAlone(t *testing.T) {
	m := railFor(t)
	// A stream already owns the slot (the shape a send from THIS client leaves behind). The
	// returned command is deliberately not run: the attach's SLOT state is what this pins.
	m.chat.Reattach("c1", "m-reply-this-client-acked")
	if !m.chat.IsStreaming("c1") {
		t.Fatal("precondition: a stream owns the slot")
	}
	gen := m.chat.CurrentGen("c1")

	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{
		{ID: "c1", TurnInFly: true, PendingReplyID: "m-someone-elses"},
	}})

	if m.chat.CurrentGen("c1") != gen {
		t.Fatalf("a rail reload repointed a live local turn's stream: generation moved %d -> %d", gen, m.chat.CurrentGen("c1"))
	}
	if !m.chat.IsStreaming("c1") {
		t.Fatal("a rail reload must not tear down a locally started turn")
	}
}
