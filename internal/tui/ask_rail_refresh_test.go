package tui

// ask_rail_refresh_test.go — THE RAIL PICKS UP A CONVERSATION CREATED IN THE OTHER CLIENT.
//
// The operator: "If you create a conversation in the GUI, it doesn't show up in the TUI
// automatically. You have to leave orch and come back into it."
//
// THE MECHANISM, and why it is exactly the class of bug the rolling window exists for. The Ask
// service has NO conversation-changes stream: ListConversations and GetConversation are unary reads,
// and the per-turn streams are scoped to ONE conversation's turn. So nothing PUSHES a new row to
// this client, and a conversation created in the GUI is discoverable only by RE-READING the list.
// The tick is therefore the entire mechanism that makes the two clients agree — there is no second
// path, and no event that could arrive late to rescue it.
//
// The tick's rail reload is gated on `convLoaded` (refresh.go), which is set by a SUCCESSFUL
// onConversations and, per AC 16, is deliberately NOT cleared by a re-read. These tests pin the
// other half of that contract — the half a latch-style regression would break silently: that a
// successful first load keeps the window LIVE for the rest of the session, so a row that appears
// later is picked up without the operator leaving the pane.
//
// Everything runs through the REAL load path (chat.LoadConversations, a connect RPC against the
// httptest plane), so a test cannot pass on a helper the production code does not use.
//
// NOTE ON ADOPTING THE MODEL. App.Update has a VALUE receiver: it returns a NEW *App, and the fields
// a rail reload sets (convLoaded, conversations) live on that copy. A test that discards the return
// mutates a model nobody sees and asserts on the old one — which is exactly how the first draft of
// this file failed. `feedApp` adopts the returned model.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// feedApp delivers one message to the shell and ADOPTS the model it returns. The command the shell
// produces is deliberately NOT run: it is where `waitChat` lives (the blocking waiter that appMsg
// pairs with a ConversationsMsg), and a test that invoked it would hang. Only the commands this file
// creates for itself are ever called.
func feedApp(t *testing.T, m *App, msg tea.Msg) *App {
	t.Helper()
	next, _ := m.Update(msg)
	nm, ok := next.(*App)
	if !ok || nm == nil {
		t.Fatalf("Update returned %T, want a non-nil *App", next)
	}
	return nm
}

// deliver expands a command's result RECURSIVELY and feeds every non-batch message back to the shell.
//
// RECURSION IS THE POINT, and getting it wrong is what the first version of this file did: the tick
// returns a batch that CONTAINS a batch (handleRefreshTick batches the refresh command with the
// re-arm), so a one-level expansion hands the shell a tea.BatchMsg — the runtime's own transport,
// which the shell does not handle — and silently drops the reload inside the inner batch. Only a
// leaf message is something the shell can act on.
//
// A batch member that produces nothing (nil) is skipped; the shell's own timer member produces a
// tick message, which is fed back (that is harmless here — the cmd it returns is dropped) and is why
// the tests call brisk() to keep the timer from costing five seconds.
func deliver(t *testing.T, m *App, msg tea.Msg) *App {
	t.Helper()
	if bm, ok := msg.(tea.BatchMsg); ok {
		for _, c := range bm {
			if c == nil {
				continue
			}
			m = deliver(t, m, c())
		}
		return m
	}
	if msg == nil {
		return m
	}
	return feedApp(t, m, msg)
}

// railTickOnce runs ONE turn of the rolling refresh window and feeds back what it produced.
//
// IT DRIVES THE REAL TICK, generation gate included: calling refreshActiveView directly would skip
// handleRefreshTick's `msg.gen != m.refreshGen` check — the one path that can discard a tick outright
// — and would therefore test a route production never takes.
func railTickOnce(t *testing.T, m *App) *App {
	t.Helper()
	nm, cmd := m.handleRefreshTick(refreshTickMsg{gen: m.refreshGen})
	if nm != nil {
		m = nm
	}
	if cmd == nil {
		return m
	}
	return deliver(t, m, cmd())
}

// railFirstLoad performs the rail's FIRST load through the real command and lets the resulting message
// land, which is what sets `convLoaded` and arms the window's reload gate.
func railFirstLoad(t *testing.T, m *App) *App {
	t.Helper()
	msg := m.chat.LoadConversations()()
	if msg == nil {
		t.Fatal("the rail's first load produced no message")
	}
	return feedApp(t, m, msg)
}

// askRailApp builds a shell on the Ask tab with the conversations rail open, backed by a real
// (httptest) plane whose conversation list the test controls.
func askRailApp(t *testing.T) (*App, *stubAskParity) {
	t.Helper()
	m, stub := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.rightRailOpen = true
	return m, stub
}

func convRow(id, title string) *apiv1.Conversation {
	return &apiv1.Conversation{Id: id, Title: title}
}

// railConvIDs reads the rail's OWN rows — the thing the operator sees — rather than the backing slice,
// so "it did not show up" is asserted where it is reported. Folder rows are skipped.
func railConvIDs(m *App) []string {
	var out []string
	for _, r := range m.railRows() {
		if r.folder {
			continue
		}
		if id := m.railConvID(r); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// THE REPORTED BUG. One conversation exists; the operator creates a second in the GUI while the TUI
// sits on the Ask tab; the next tick of the window must show it.
func TestRailPicksUpAConversationCreatedElsewhere(t *testing.T) {
	brisk(t)
	m, stub := askRailApp(t)
	stub.convs = []*apiv1.Conversation{convRow("c1", "first")}

	// The rail's FIRST load — the successful load that sets convLoaded. This is what a session looks
	// like when the operator opens the TUI: an established, loaded rail.
	m = railFirstLoad(t, m)
	if !m.convLoaded {
		t.Fatal("fixture: the first load did not land, so the window's reload gate is unarmed")
	}
	if got := railConvIDs(m); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("fixture: rail rows = %v, want exactly [c1]", got)
	}

	// THE GUI CREATES ONE. Nothing is pushed to this client — a re-read is the only discovery path
	// (there is no conversation-changes stream), so this is the whole of the change.
	stub.convs = append(stub.convs, convRow("c2", "created in the GUI"))

	m = railTickOnce(t, m)

	got := railConvIDs(m)
	if len(got) != 2 {
		t.Fatalf("the rail still shows %v after a tick — a conversation created in the GUI does not "+
			"appear until the operator leaves and re-enters, which is the reported bug", got)
	}
	if !strings.Contains(strings.Join(got, ","), "c2") {
		t.Errorf("the rail picked up %v but not the new conversation c2", got)
	}
	// And by TITLE, which is what the operator reads.
	var sawTitle bool
	for _, r := range m.railRows() {
		if r.title == "created in the GUI" {
			sawTitle = true
		}
	}
	if !sawTitle {
		t.Error("the new conversation's TITLE is not among the rail's rows")
	}
}

// THE GATE MUST STAY LIVE. The reload is gated on convLoaded; if anything clears that latch — the exact
// regression AC 16 records — the window stops re-reading the rail for the REST OF THE SESSION and every
// later conversation is missed. This drives the window repeatedly with a list that keeps growing, so a
// one-shot reload cannot pass it.
func TestRailReloadKeepsWorkingAcrossTicks(t *testing.T) {
	brisk(t)
	m, stub := askRailApp(t)
	stub.convs = []*apiv1.Conversation{convRow("c1", "first")}
	m = railFirstLoad(t, m)

	for _, id := range []string{"c2", "c3", "c4"} {
		stub.convs = append(stub.convs, convRow(id, "created "+id))
		m = railTickOnce(t, m)
		if !m.convLoaded {
			t.Fatalf("convLoaded was cleared by the tick that discovered %s — the rail reload is now "+
				"disabled for the rest of the session", id)
		}
		if got := len(railConvIDs(m)); got != len(stub.convs) {
			t.Fatalf("after discovering %s the rail shows %d rows, want %d", id, got, len(stub.convs))
		}
	}
}

// A FRESH SESSION HAS A LANDED RAIL. This is the precondition the reported case runs from, and it is
// asserted separately because a gate that is never armed would make the two tests above fail for a
// reason that has nothing to do with the reload.
func TestRailReloadIsGatedOnASuccessfulFirstLoad(t *testing.T) {
	m, _ := askRailApp(t)

	if m.convLoaded {
		t.Fatal("fixture: a fresh app must not report a landed rail load")
	}

	// The tick still runs before a first load — it must never die — it just does not re-read the rail.
	if cmd := m.refreshActiveView(); cmd == nil {
		t.Fatal("the tick produced nothing at all; the window must stay armed even before a first load")
	}
	if m.convLoaded {
		t.Error("convLoaded became true without a successful load")
	}
}

// THE SCOPE FILTER IS THE OTHER HALF OF "it did not show up", and it is the half a list-refresh test
// cannot see: a conversation created in the GUI that lands OUTSIDE the rail's active project scope is
// reloaded correctly, sits in m.conversations, and is still not drawn.
//
// The observed data makes this concrete rather than hypothetical: the newest conversation on this
// tenant — created from the GUI an hour after the one this report was filed from — carries
// project_id "" (No project), while a conversation created from within a project carries that
// project's id. Nothing in the Ask service has a project-changes stream either, so the ONLY signal
// that a scope is hiding a new row is a re-read plus the scope's own label.
//
// This test states the boundary rather than assuming it: with the rail scoped to a project, an
// unassigned conversation must NOT appear, and switching the scope to All projects must reveal it.
func TestRailScopeHidesAConversationFromAnotherProject(t *testing.T) {
	brisk(t)
	m, stub := askRailApp(t)
	stub.convs = []*apiv1.Conversation{
		{Id: "in-project", Title: "inside the scope", ProjectId: "proj-1"},
		{Id: "unassigned", Title: "created in the GUI"},
	}
	m = railFirstLoad(t, m)

	// Scoped to the project the operator is working in.
	m.projectScope = "proj-1"
	got := railConvIDs(m)
	if len(got) != 1 || got[0] != "in-project" {
		t.Fatalf("fixture: scoped rail = %v, want exactly [in-project]", got)
	}

	// The GUI-created row is LOADED but filtered. This is the state the operator reports as "it does
	// not show up", and it is not a staleness bug — the reload did its job.
	m = railTickOnce(t, m)
	for _, id := range railConvIDs(m) {
		if id == "unassigned" {
			t.Fatal("an unassigned conversation leaked into a project-scoped rail")
		}
	}
	if len(m.conversations) != 2 {
		t.Fatalf("the reload did not land: %d conversations held, want 2", len(m.conversations))
	}
	// AND THE HIDING IS REPORTED, which is what makes it recoverable: scopeHiddenCount is the number
	// the notice quotes so the operator is told how many rows the scope is hiding and how to widen it.
	if hidden := m.scopeHiddenCount(); hidden != 1 {
		t.Errorf("scopeHiddenCount = %d, want 1 — a silent narrowing reads as data loss", hidden)
	}

	// Widening the scope is what surfaces it. This is the documented recovery (/project), and it is
	// why "leave and come back" appeared to fix it: the re-entry path re-derives scope, so the
	// operator's mental model was a refresh when the mechanism was the filter.
	m.projectScope = projectScopeAll
	if got := railConvIDs(m); len(got) != 2 {
		t.Errorf("widening the scope to All projects still shows %v, want both rows", got)
	}
}

// A NEW ROW HIDDEN BY AN ACTIVE SCOPE MUST BE SAID OUT LOUD.
//
// This is the second half of the reported symptom, and the half the scope test above only describes: the
// scope is set ONCE (applyLaunchDirScope) and its notice quotes the list at that moment. A conversation
// that appears in the list LATER — created in the GUI, which is the reported case — is hidden with no
// notice at all, so the operator cannot tell a filter from a conversation that never arrived. The row is
// loaded, the rail is correct, and the screen is silent.
//
// It must also stay QUIET when nothing new is hidden: re-noticing on every five-second reload would turn
// the status line into noise the operator stops reading.
func TestRailAnnouncesANewRowItsScopeIsHiding(t *testing.T) {
	brisk(t)
	m, stub := askRailApp(t)
	stub.convs = []*apiv1.Conversation{
		{Id: "in-project", Title: "inside", ProjectId: "proj-1"},
	}
	m = railFirstLoad(t, m)
	m.projectScope = "proj-1"
	m.dock.SetNotice("")

	// Nothing is hidden yet, so the reload has nothing to say.
	m = railTickOnce(t, m)
	if strings.Contains(m.dock.Notice, "not shown") {
		t.Fatalf("the rail reported hidden conversations when it hid none: %q", m.dock.Notice)
	}

	// THE GUI CREATES ONE, and it is NOT in the active scope — so this reload newly hides a row the
	// operator has never been told about.
	stub.convs = append(stub.convs, convRow("created-in-gui", "created in the GUI"))
	m.dock.SetNotice("")
	m = railTickOnce(t, m)

	if !strings.Contains(m.dock.Notice, "not shown") {
		t.Fatalf("a conversation created in the GUI was hidden by the active scope with NO notice — "+
			"the operator sees a row that never appears and nothing to explain it: %q", m.dock.Notice)
	}
	if !strings.Contains(m.dock.Notice, "/project") {
		t.Errorf("the notice does not say how to widen the scope: %q", m.dock.Notice)
	}

	// AND IT STAYS QUIET ON THE NEXT RELOAD, because nothing changed: the same one row is still hidden.
	m.dock.SetNotice("")
	m = railTickOnce(t, m)
	if strings.Contains(m.dock.Notice, "not shown") {
		t.Errorf("the rail re-noticed an unchanged scope, which would make the status line noise: %q",
			m.dock.Notice)
	}

	// WIDENING THE SCOPE IS THE RECOVERY, and then there is nothing hidden to warn about.
	m.projectScope = projectScopeAll
	m.dock.SetNotice("")
	m = railTickOnce(t, m)
	if strings.Contains(m.dock.Notice, "not shown") {
		t.Errorf("the scope was widened but the rail still reports hidden rows: %q", m.dock.Notice)
	}
	if got := len(railConvIDs(m)); got != 2 {
		t.Errorf("after widening to All projects the rail shows %d rows, want 2", got)
	}
}
