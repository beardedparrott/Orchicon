package tui

// ask_discovery_wire_test.go — the SHELL asks the server for pending asks on the paths
// where it used to rely on having caught a live event.
//
// The operator: "you sent numerous permission card requests and user ask card requests. ALL
// of them reached the GUI conversation just fine, however, they did not all reach the TUI…
// it appeared you were stalled. So I went into the GUI and lo and behold, a permissions card
// was waiting for me to click on it."
//
// THE EARLY RETURN WAS THE BUG. reattachRunningTurn used to bail when the conversation row
// did not say a turn was in flight with a pending reply id — so a turn this client never
// started, or had stopped tracking, got NO re-attach and therefore no card, while the server
// kept the turn parked. These tests pin that discovery runs on that path regardless.

import (
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
)

// DISCOVERY IS UNCONDITIONAL: with no conversation row and no pending reply id — the state
// that used to return nil — the shell must still produce a discovery command.
func TestTheShellDiscoversAsksWithoutATurnFlag(t *testing.T) {
	m := newTestApp()
	// A client set WITH an Ask client, because discovery needs one — production always has it
	// (client.go builds the full set), and the empty `&client.Clients{}` newTestApp uses has
	// no Ask at all, so it cannot exercise this path.
	m.clients = client.New(client.Options{BaseURL: "http://127.0.0.1:1", Token: "oc_t"})
	m.chat = chat.NewController(m.clients)
	m.chatConvID = "conv-1"
	// Deliberately NO conversation row and no pending reply id: the row-driven re-attach has
	// nothing to attach to, which is exactly when a parked turn's card used to disappear.
	m.conversations = nil

	cmd := m.reattachRunningTurn("conv-1")
	if cmd == nil {
		t.Fatal("the shell returned NO command with no turn in flight — a card for a " +
			"server-parked turn has no path into the pane on this path, which is the defect")
	}
}

// The discovery is batched WITH the re-attach when there IS a turn to re-attach to, so the
// two halves cannot be delivered exclusively — a client that re-attaches must also discover,
// because the re-attach only replays asks on the stream it opens and this covers the rest.
func TestTheShellDiscoversAsksAlongsideAReattach(t *testing.T) {
	m := newTestApp()
	m.clients = client.New(client.Options{BaseURL: "http://127.0.0.1:1", Token: "oc_t"})
	m.chat = chat.NewController(m.clients)
	m.chatConvID = "conv-1"
	m.conversations = []chat.Conversation{{
		ID:             "conv-1",
		TurnInFly:      true,
		PendingReplyID: "reply-1",
	}}

	cmd := m.reattachRunningTurn("conv-1")
	if cmd == nil {
		t.Fatal("a turn to re-attach to produced no command at all")
	}
}

// With no conversation open there is nothing to discover: no round trip, no card.
func TestTheShellDoesNotDiscoverWithNoConversation(t *testing.T) {
	m := newTestApp()
	m.chat = chat.NewController(m.clients)
	m.chatConvID = ""
	if cmd := m.reattachRunningTurn(""); cmd != nil {
		t.Fatal("an empty conversation id must not produce a discovery call")
	}
}
