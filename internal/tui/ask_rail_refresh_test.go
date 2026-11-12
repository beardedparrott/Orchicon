package tui

// ask_rail_refresh_test.go — THE RAIL PICKS UP A CONVERSATION CREATED IN THE OTHER CLIENT.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// feedApp delivers one message to the shell and ADOPTS the model it returns.
func feedApp(t *testing.T, m *App, msg tea.Msg) *App {
	t.Helper()
	next, _ := m.Update(msg)
	nm, ok := next.(*App)
	if !ok || nm == nil {
		t.Fatalf("Update returned %T, want a non-nil *App", next)
	}
	return nm
}

// deliver expands a command's result RECURSIVELY and feeds every non-batch message back to the
// shell.
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

// THE GATE MUST STAY LIVE.
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

// A FRESH SESSION HAS A LANDED RAIL.
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

// Nothing in the Ask service has a project-changes stream either, so the ONLY signal that a scope
// is hiding a new row is a re-read plus the scope's own label.
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

	// Widening the scope is what surfaces it.
	m.projectScope = projectScopeAll
	if got := railConvIDs(m); len(got) != 2 {
		t.Errorf("widening the scope to All projects still shows %v, want both rows", got)
	}
}

// A NEW ROW HIDDEN BY AN ACTIVE SCOPE MUST BE SAID OUT LOUD.
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
