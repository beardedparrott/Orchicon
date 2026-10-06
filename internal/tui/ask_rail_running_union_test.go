package tui

// ask_rail_running_union_test.go — THE RAIL ROW AND THE PANE MUST AGREE ABOUT THE SAME CONVERSATION.
//
// The operator: "I have noticed the 'running' status on the conversation rail list doesn't always show up on
// active running conversations."
//
// THE MECHANISM. Both clients rendered the row's running state from the server's POLLED turn_in_flight field
// ALONE, and never merged the locally-known turn state they already held:
//
//	TUI:  conversationRow — `if c.TurnInFly { meta = "running" }`
//	GUI:  isRunning={conv.turnInFlight ?? false}
//
// while the SAME client already unioned both elsewhere — the TUI pane's activity line keys off
// `IsStreaming || turnInFlight` (App.runningFor). So a conversation THIS client is actively streaming (the
// transcript visibly growing) still read "12 msgs" on the row beside it, because TurnInFly is only as fresh
// as the last list read and a send has no immediate rail refresh. The row and the pane disagreed about the
// SAME conversation. That is the INVERSE of the divergence already fixed at the activity line ("the rail said
// running while the pane was silent"), and it survived because the two surfaces read different fields.
//
// THESE ASSERT THE PAINTED RAIL ROW (AC 11/12/13) and, for the "send is reflected at once" half, the router's
// own dispatch result (AC 14).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// railRowFor renders the rail row the rail itself draws for the open conversation (AC 11/12: read the row,
// not an accessor).
func railRowFor(t *testing.T, m *App, convID string) string {
	t.Helper()
	m.convRailOpen = true
	m.rightRailOpen = true
	m.projectScope = projectScopeAll
	m.refreshLayout()
	for _, r := range m.railRows() {
		row := m.railLine(r, -1, ConversationsRailWidth)
		if strings.Contains(row, "running") || strings.Contains(row, "msgs") {
			// Return the row whose meta is what this test is about; the rail's rows are the conversations.
			return row
		}
	}
	t.Fatalf("fixture: no conversation row found in the rail")
	return ""
}

// AC 11 + AC 12: the row shows "running" for a conversation THIS client is streaming, with the server's list
// field FALSE — the LOCAL-state path alone must prove it. Named for the divergence it prevents.
func TestTheRowAndThePaneAgreeWhenTheClientIsStreamingWithoutAServerRow(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// The server's row does NOT say turn in flight — the stale list the operator hit.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: false, MessageN: 12}}
	// THIS client is streaming the turn (the local slot is live). chat.Send flips the slot synchronously.
	if cmd := m.chat.Send("c1", "hello", ""); cmd == nil {
		t.Fatal("fixture: the send did not arm a stream slot")
	}
	if !m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold a live stream slot for the turn")
	}

	// THE PANE already says so (it unions both) — this is the invariant the row must meet. The footer is
	// painted by onChatWake, so repaint first.
	m.onChatWake()
	if !strings.Contains(m.askStatusLine(), "Orchicon is") {
		t.Errorf("fixture: the pane does not report the turn, so there is no divergence to test (footer = %q)",
			m.askStatusLine())
	}
	row := railRowFor(t, m, "c1")
	if !strings.Contains(row, "running") {
		t.Errorf("the rail row for a conversation THIS client is streaming does not say \"running\" — the "+
			"operator's \"the 'running' status on the conversation rail list doesn't always show up on active "+
			"running conversations\", and the row contradicts the pane beside it: %q", row)
	}
}

// AC 13: the server field is not REPLACED, only supplemented. A turn started in the OTHER client (live slot
// idle, TurnInFly true) must still show "running" — so the union cannot regress into a local-only check.
func TestTheRowShowsRunningForATurnStartedElsewhere(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1", MessageN: 3}}
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold NO local slot — the turn was started elsewhere")
	}

	row := railRowFor(t, m, "c1")
	if !strings.Contains(row, "running") {
		t.Errorf("with the LOCAL slot idle but the server reporting turn_in_flight, the row must still say "+
			"\"running\" (AC 13 — the union must not become local-only): %q", row)
	}
}

// AC 15: no regression to the rail's other states. A conversation with no turn shows "N msgs"; a finished
// turn CLEARS the marker; a marked row keeps its gutter marker; the all-projects meta prefix is unchanged.
func TestTheRailRowKeepsItsOtherStates(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.projectScope = projectScopeAll

	// (a) NO TURN -> "N msgs".
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", MessageN: 12}}
	row := railRowFor(t, m, "c1")
	if !strings.Contains(row, "12 msgs") {
		t.Errorf("an idle conversation does not show its message count: %q", row)
	}
	if strings.Contains(row, "running") {
		t.Errorf("an idle conversation reads as running: %q", row)
	}

	// (b) A FINISHED TURN CLEARS THE MARKER: the same row that was streaming, once the slot is cleared, is
	//     idle again. EndStream is the real clear.
	m.chat.Send("c1", "hi", "")
	m.chat.EndStream("c1", m.chat.CurrentGen("c1"))
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: EndStream did not clear the slot")
	}
	row = railRowFor(t, m, "c1")
	if strings.Contains(row, "running") {
		t.Errorf("the marker did not clear when the turn finished: %q", row)
	}

	// (c) A MARKED ROW keeps its gutter marker AND the count (the marker must not shift the numbers).
	m.convMarked = map[string]bool{"c1": true}
	row = railRowFor(t, m, "c1")
	if !strings.Contains(row, "✓") || !strings.Contains(row, "12 msgs") {
		t.Errorf("a marked row lost its marker or its count: %q", row)
	}
	m.convMarked = map[string]bool{}
}

// AC 14: a send is reflected AT ONCE, not within the 5s tick. The mechanism is an immediate rail reload on the
// turn ack (the router's TurnAckedMsg arm). Asserted as "at once": the dispatch itself must return the list
// reload, so the row is refreshed by the time the ack is handled — no timer involved.
func TestTheTurnAckRefreshesTheRailAtOnce(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: false, MessageN: 12}}
	m.chat.Send("c1", "hi", "")

	_, cmd := m.dispatch(chat.TurnAckedMsg{ConvID: "c1"})
	if cmd == nil {
		t.Fatalf("the turn ack dispatched no command — the rail cannot be refreshed at once")
	}
	// Run the batch and require that a conversations reload is IN IT (the row must be re-read on the ack,
	// not deferred to the tick). Bounded via the shared drainer budget so a blocking waiter cannot hang.
	msg := runCmdBounded(cmd, runCtxCmdBudget)
	if !batchCarriesConversationsReload(t, msg) {
		t.Errorf("the turn ack's command batch does not carry a conversations reload — the row would wait " +
			"for the 5s tick, which is the aggravating freshness factor (AC 14)")
	}
}

// batchCarriesConversationsReload drains a tea.Batch (or a single cmd) and reports whether any produced
// message is the conversations list arriving — i.e. a rail reload was dispatched. It is bounded: the
// load runs against the stub plane, so it resolves without a live session.
func batchCarriesConversationsReload(t *testing.T, msg tea.Msg) bool {
	t.Helper()
	if msg == nil {
		return false
	}
	if bm, ok := msg.(tea.BatchMsg); ok {
		for _, c := range bm {
			if c == nil {
				continue
			}
			if batchCarriesConversationsReload(t, runCmdBounded(c, runCtxCmdBudget)) {
				return true
			}
		}
		return false
	}
	_, ok := msg.(chat.ConversationsMsg)
	return ok
}
