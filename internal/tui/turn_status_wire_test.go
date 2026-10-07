package tui

// turn_status_wire_test.go — the client reflects the PLANE's turn state, read from the conversation row.
//
// Verified against the live plane while a turn was running (a standalone curl, not a test):
//
//	GetConversation   → turnInFlight: true, pendingAssistantMessageId: …, turnProgressing: true
//	ListConversations → the same row, same fields
//
// while the TUI showed no activity line and the rail showed no "running" marker. The plane was right and
// the client's COPY of that state was wrong — which is why turnInFlight reads
// Conversation.TurnInFly, populated by the list load the rolling refresh already performs, rather than
// trusting nothing at all.
//
// A per-conversation status FETCH was tried here and REMOVED: it put a GetConversation on every refresh
// tick to answer a question the list load already answers. The state below is what the row carries.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE RAIL MARKS A RUNNING CONVERSATION — from the row the server computed.
func TestRailShowsRunningFromTheServerRow(t *testing.T) {
	m, _ := newAskApp(t)
	m.conversations = []chat.Conversation{
		{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1", MessageN: 22},
	}
	m.convRailOpen = true
	m.projectScope = projectScopeAll
	// Asserted through the ROW RENDERER the rail itself uses, so this covers the meta the operator reads.
	row := m.conversationRow(m.conversations[0], 0, 40)
	if !strings.Contains(row, "running") {
		t.Errorf("the rail does not mark a running conversation: %q", row)
	}
}

// turnInFlight REPORTS THE ROW'S STATE, and an idle row stays idle — a fallback that never cleared would
// claim a turn was running for the rest of the session.
func TestTurnInFlightFollowsTheRow(t *testing.T) {
	m, _ := newAskApp(t)
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true}}
	if !m.turnInFlight("c1") {
		t.Error("a row the server reports as running did not read as in flight")
	}
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	if m.turnInFlight("c1") {
		t.Error("an idle row still reads as in flight — the activity line would never clear")
	}
	// A conversation the shell does not hold is NOT in flight: the rail may filter it out, and inventing
	// a turn for a row we cannot see would put a line under a conversation with nothing running.
	if m.turnInFlight("not-in-the-list") {
		t.Error("turnInFlight invented a turn for a conversation the shell does not hold")
	}
}
