package tui

// project_move_test.go — MOVING A CONVERSATION, end to end, and the two ways it used to lie about it.
//
// The operator: "please also check the project-move slash command in the TUI is working properly. I did that on a
// conversation and it is still just sitting there in the same list. I would have expected it to disappear because
// I moved it to a project that was not currently selected."
//
// The WRITE and the reload were wired correctly — SetConversationProject → ConversationProjectSetMsg →
// reloadConversations → onConversations → the rail re-filters. What was wrong was the TARGET, in two ways that
// both end with the chat exactly where it was:
//
//  1. the picker OFFERED "All projects", whose value is the sentinel __all__ rather than a project id, so the
//     server rejects the write (one row Up from the first project — in a picker whose cursor opens on the
//     conversation's current project); and
//  2. picking the conversation's CURRENT project — the row the cursor already sits on, so Enter alone does it —
//     wrote, reported "moved to <the project it was already in>", and reloaded to a list where the chat had
//     not moved at all.
//
// AND NONE OF IT COULD BE TESTED, because stubAskParity did not implement SetConversationProject at all. The
// missing fixture was the missing coverage, which is why this shipped.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// appWithBothServices builds a shell whose Ask AND Project services are stubs on ONE server.
//
// IT HAS TO BE ONE SERVER: a move is a write followed by a conversations reload, so the fixture needs both
// services reachable from the same client. The other fixtures have one or the other, which is exactly why the
// move path had no end-to-end test.
func appWithBothServices(t *testing.T, ask apiv1connect.AskOrchiconServiceHandler) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(ask))
	mux.Handle(apiv1connect.NewProjectServiceHandler(&stubProjectList{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.SwitchTo(TabAsk)
	// AS AFTER A PROJECT LOAD: otherwise the next conversations load starts the self-healing project fetch, and
	// the assertions below would be racing a request this test is not about.
	m.railProjectsLoaded = true
	return m
}

// movePlane builds a shell backed by a REAL Ask + Project service pair, so a move can be driven through the
// whole chain: the picker's key, the RPC, the outcome message, the reload, and the rail that results.
func movePlane(t *testing.T) (*App, *stubAskParity) {
	t.Helper()
	stub := &stubAskParity{convs: []*apiv1.Conversation{
		{Id: "c-1", Title: "orch chat", ProjectId: "p-1"},
		{Id: "c-2", Title: "tools chat", ProjectId: "p-2"},
		{Id: "c-3", Title: "loose chat", ProjectId: ""},
	}}
	m := appWithBothServices(t, stub)
	m.railProjects = []railProject{
		{ID: "p-1", Name: "Alpha", Status: "active"},
		{ID: "p-2", Name: "Beta", Status: "active"},
	}
	m.conversations = []chat.Conversation{
		{ID: "c-1", Title: "orch chat", ProjectID: "p-1"},
		{ID: "c-2", Title: "tools chat", ProjectID: "p-2"},
		{ID: "c-3", Title: "loose chat", ProjectID: ""},
	}
	m.projectScope = "p-1"
	m.projectScopeChosen = true
	m.chatConvID = "c-1"
	m.refreshLayout()
	return m, stub
}

// railHasConv reports whether a conversation id is among the rows the rail would DRAW — the question the
// operator asked ("still just sitting there").
func railHasConv(m *App, convID string) bool {
	for _, c := range m.scopedConversations() {
		if c.ID == convID {
			return true
		}
	}
	return false
}

// THE MOVE LEAVES THE RAIL. This is the operator's report, as a test: moving a conversation to a project that is
// not the active scope must remove it from the list, through the real RPC AND the real reload.
func TestMovingAConversationOutOfTheScopeRemovesItFromTheRail(t *testing.T) {
	m, stub := movePlane(t)
	if !railHasConv(m, "c-1") {
		t.Fatal("fixture: c-1 is not in the p-1 rail to begin with")
	}

	runSlashTUI(t, m, "/project-move")
	if m.projectPick == nil {
		t.Fatal("/project-move opened no picker")
	}
	// Choose Beta — a project that is NOT the active scope.
	m.projectPick.selectByValue("p-2")
	_, cmd := m.projectPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in move mode produced no command, so the write never happens")
	}

	// Run the write, then let the shell process its outcome and the reload it triggers.
	runCtx(t, m, cmd, runCtxCmdBudget)

	if stub.projectMoveFor != "c-1" || stub.projectMoveDest != "p-2" {
		t.Errorf("the write went to (%q → %q), want (c-1 → p-2)", stub.projectMoveFor, stub.projectMoveDest)
	}
	if railHasConv(m, "c-1") {
		t.Errorf("the moved conversation is STILL in the p-1 rail — this is the operator's \"it is still just "+
			"sitting there in the same list\": %v", m.scopedConversations())
	}
	// AND IT LANDED SOMEWHERE, rather than merely vanishing: scoped to Beta it must now be present.
	m.projectScope = "p-2"
	if !railHasConv(m, "c-1") {
		t.Errorf("the conversation is in neither workspace: %v", m.scopedConversations())
	}
}

// THE MOVE PICKER OFFERS NO "ALL PROJECTS". Its value is the sentinel __all__, not a project id, so choosing it
// is a write the server rejects — and the failure reads as a broken app rather than as a bad target. The GUI's
// own move control filters it out for exactly this reason; this pins the TUI to the same rule.
func TestTheMovePickerOffersNoAllProjectsTarget(t *testing.T) {
	m, _ := movePlane(t)

	for _, o := range m.projectMoveOptions() {
		if o.Value == projectScopeAll {
			t.Errorf("the move target list offers %q (\"All projects\") — it is a way to LOOK at the list, not a "+
				"place to put a conversation, and the server rejects it as a project id", o.Label)
		}
	}
	// "No project" STAYS: a conversation has to be able to leave a project.
	var hasUnassigned bool
	for _, o := range m.projectMoveOptions() {
		if o.Value == unassignedScope {
			hasUnassigned = true
		}
	}
	if !hasUnassigned {
		t.Error("the move target list has no \"No project\" row, so a conversation could never leave a project")
	}

	// AND THE OPENED PICKER USES THAT LIST, not the scope list — the difference is one row and it is the row that
	// breaks the write.
	runSlashTUI(t, m, "/project-move")
	if m.projectPick == nil {
		t.Fatal("/project-move opened no picker")
	}
	for _, o := range m.projectPick.options {
		if o.Value == projectScopeAll {
			t.Errorf("the open move picker still offers %q", o.Label)
		}
	}
}

// PICKING THE PROJECT IT IS ALREADY IN SAYS SO, and writes nothing.
//
// The cursor OPENS ON the conversation's current project, so Enter alone selects it — and it used to write,
// report "moved to <that same project>", and reload to a list where the chat had not moved. That is the most
// likely thing the operator actually did, and it was indistinguishable from success.
func TestMovingToTheCurrentProjectSaysSoInsteadOfWriting(t *testing.T) {
	m, stub := movePlane(t)

	runSlashTUI(t, m, "/project-move")
	if m.projectPick == nil {
		t.Fatal("/project-move opened no picker")
	}
	if got := m.projectPick.options[m.projectPick.sel].Value; got != "p-1" {
		t.Fatalf("fixture: the cursor opened on %q, want the conversation's own project p-1", got)
	}

	_, cmd := m.projectPickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("picking the conversation's current project issued a write — it changes nothing, and reporting " +
			"\"moved\" for it is exactly what made the operator think the move worked")
	}
	if !strings.Contains(m.dock.Notice, "already in") {
		t.Errorf("the no-op move was not explained: notice = %q", m.dock.Notice)
	}
	if stub.projectMoveFor != "" {
		t.Errorf("the RPC was called for a no-op move (%q → %q)", stub.projectMoveFor, stub.projectMoveDest)
	}
}

// AND "ALL PROJECTS" IS REFUSED BY THE WRITE ITSELF, for the route that can still reach it: the `m` key works on
// the SCOPE picker, whose option list legitimately contains it.
func TestMovingToAllProjectsIsRefusedWithAnExplanation(t *testing.T) {
	m, stub := movePlane(t)

	if cmd := m.moveConversationTo(m.chatConvID, projectScopeAll); cmd != nil {
		t.Error("a move onto \"All projects\" issued a write the server rejects")
	}
	if !strings.Contains(m.dock.Err, "All projects") {
		t.Errorf("the refusal does not name the problem: err = %q", m.dock.Err)
	}
	if stub.projectMoveFor != "" {
		t.Errorf("the RPC was called with the sentinel as a project id (%q)", stub.projectMoveDest)
	}
}

// AND A REAL MOVE STILL WRITES. The guards must not have swallowed the case the feature exists for.
func TestARealMoveStillWrites(t *testing.T) {
	m, stub := movePlane(t)
	cmd := m.moveConversationTo("c-1", "p-2")
	if cmd == nil {
		t.Fatal("moving to a different project issued no command")
	}
	// THE COMMAND HAS TO BE RUN: it is a closure, so the RPC happens when the tea runtime invokes it, not when
	// it is returned. Asserting on the stub before running it would pass against a command that does nothing.
	runCtx(t, m, cmd, runCtxCmdBudget)

	if stub.projectMoveFor != "c-1" || stub.projectMoveDest != "p-2" {
		t.Errorf("the write went to (%q → %q), want (c-1 → p-2)", stub.projectMoveFor, stub.projectMoveDest)
	}
}
