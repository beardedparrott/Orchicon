package tui

// turn_status_wire_test.go — the client must reflect the PLANE's turn state.
//
// VERIFIED AGAINST THE LIVE PLANE while a turn was running (a standalone curl, not a test):
//
//	GetConversation  → turnInFlight: true, pendingAssistantMessageId: 01M3YPHXKC32GNMXGG8ZDZXNMV,
//	                   turnProgressing: true, turnLastActivityAt: <seconds ago>
//	ListConversations → the same row, same fields
//
// while the TUI showed no activity line and the rail showed no "running" marker. So the plane is
// right and the client is wrong, and these tests pin the client half.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE RAIL MARKS A RUNNING CONVERSATION. This is the operator's "Running does NOT show up on the
// conversation in the rail", and the plane reports turnInFlight=true for the very conversation they
// were watching.
func TestRailShowsRunningFromTheServerRow(t *testing.T) {
	m, _ := newAskApp(t)
	m.conversations = []chat.Conversation{
		{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1", MessageN: 22},
	}
	m.convRailOpen = true
	m.projectScope = projectScopeAll
	// Asserted on the ROW RENDERER the rail itself uses, so this covers the meta the operator reads.
	row := m.conversationRow(m.conversations[0], 0, 40)
	if !strings.Contains(row, "running") {
		t.Errorf("the rail does not mark a running conversation: %q", row)
	}
}

// AND THE CLIENT ASKS THE PLANE DIRECTLY when its own view says nothing is running.
//
// The rail renders m.conversations, which is refreshed on a timer — so a turn the client did not
// start, or one whose list refresh has not landed, leaves the client believing nothing is running
// while the plane says otherwise. Fetching the ONE conversation's status is cheap (a single row) and
// authoritative, which is what makes the activity line and the Stop button correct in that state
// instead of silently dead.
func TestTurnStatusIsFetchedFromThePlane(t *testing.T) {
	m, stub := newAskApp(t)
	stub.conv = &apiv1.Conversation{
		Id: "c1", Title: "a chat",
		TurnInFlight: true, PendingAssistantMessageId: "m1", TurnProgressing: true,
	}
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}} // the client's view: idle

	cmd := m.refreshTurnStatus()
	if cmd == nil {
		t.Fatal("no status fetch was issued — the client has nothing but a stale list to go on")
	}
	msg := cmd()
	sm, ok := msg.(chat.ConversationStatusMsg)
	if !ok {
		t.Fatalf("status fetch produced %T, want chat.ConversationStatusMsg", msg)
	}
	if sm.Err != "" {
		t.Fatalf("status fetch failed: %s", sm.Err)
	}
	if !sm.Conv.TurnInFly {
		t.Fatalf("the fetched status lost turnInFlight: %+v", sm.Conv)
	}

	// Applying it must FIX the client's view: the row now says running, which the rail reads.
	m.onConversationStatus(sm)
	if !m.turnInFlight("c1") {
		t.Error("the row still reports an idle conversation after the plane said it was running")
	}
	if row := m.conversationRow(m.conversations[0], 0, 40); !strings.Contains(row, "running") {
		t.Errorf("the rail still does not mark it running: %q", row)
	}
}

// THE FETCH IS ONE ROW, NOT THE WHOLE LIST, and it is a no-op with nothing open.
func TestTurnStatusFetchIsScopedAndQuiet(t *testing.T) {
	m, stub := newAskApp(t)
	if cmd := m.refreshTurnStatus(); cmd != nil {
		t.Fatalf("a status fetch was issued with no conversation open (%T)", cmd())
	}
	m.chatConvID = "c1"
	stub.conv = &apiv1.Conversation{Id: "c1"}
	msg := m.refreshTurnStatus()()
	if sm, ok := msg.(chat.ConversationStatusMsg); !ok || sm.Conv.ID != "c1" {
		t.Fatalf("the fetch asked for something else: %#v", msg)
	}
	if stub.getConvID != "c1" {
		t.Errorf("GetConversation got %q, want c1 — the one open conversation", stub.getConvID)
	}
}

// A PLANE-CONFIRMED TURN RE-ATTACHES THE CLIENT: with no local stream, the poll, the watchdog and the
// countdown are all missing, and the existing re-attach path is what restores them.
func TestPlaneReportedTurnReattachesTheClient(t *testing.T) {
	m, stub := newAskApp(t)
	stub.conv = &apiv1.Conversation{Id: "c1", Title: "a chat", TurnInFlight: true, PendingAssistantMessageId: "m1"}
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}

	msg := m.refreshTurnStatus()()
	cmd := m.onConversationStatus(msg.(chat.ConversationStatusMsg))
	if cmd == nil {
		t.Fatal("a plane-reported turn produced no follow-up — the slot would stay unarmed")
	}
	// The client now KNOWS the turn is running — the state every visible consequence (the rail's marker,
	// the status line, the stop affordance) is derived from. The line itself is asserted on a real Ask
	// pane, in TestActivityLineShowsForAServerReportedTurn; this harness has no such pane.
	if !m.turnInFlight("c1") {
		t.Error("the plane-reported turn was not applied to the row")
	}
}

// AN IDLE PLANE SAYS SO: the status read must not invent a turn, or the line would never clear.
func TestIdlePlaneLeavesTheLineClear(t *testing.T) {
	m, stub := newAskApp(t)
	stub.conv = &apiv1.Conversation{Id: "c1", Title: "a chat"} // no turn
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}

	m.onConversationStatus(m.refreshTurnStatus()().(chat.ConversationStatusMsg))
	if m.turnInFlight("c1") {
		t.Error("an idle conversation was marked running")
	}
}
