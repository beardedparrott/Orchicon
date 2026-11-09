package tui

// project_scope_rebuild_test.go — THE END-TO-END PATH THE OPERATOR ACTUALLY WALKS.
//
// The operator, on a build that already contained the project-scope work:
//
//	"When I typed /projects it showed me All Projects and none. Projects should be pulled from the actual
//	 projects list in Orchicon."
//
// Every test in the sibling files covers a RULE — the scope filter, the launch-directory match, the picking of
// an active project, and (in one case) that a CONVERSATIONS load also fetches the projects. None of them drives
// the path a real session takes: `Init()` is what runs at startup, and the reported fault was that the project
// list was never fetched on that path. A test of a rule cannot observe a request that was never made, and a
// test of a different entry point cannot observe a missing one.
//
// So these drive `Init()` itself, through the real message loop, against a real ProjectService.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// runCtx's three hard bounds. See its doc comment for why each exists.
const (
	// runCtxMaxSteps bounds how many commands ONE drain will walk, so a graph that fans out without a timer
	// cannot spin the loop.
	runCtxMaxSteps = 500
	// runCtxTotalBudget bounds the WALL CLOCK of one drain. It is needed because the per-command budget is only
	// paid by commands that DO NOT ANSWER, so a chain of several long-lived waiters adds up rather than ending.
	runCtxTotalBudget = 30 * time.Second
	// runCtxCmdBudget is how long ONE command may take before the drain calls it a long-lived waiter
	// and moves on.
	runCtxCmdBudget = 250 * time.Millisecond
)

// runCmdBounded invokes ONE command and returns the message it produced, or nil when it did not
// answer within the budget.
func runCmdBounded(cmd tea.Cmd, budget time.Duration) tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg
	case <-time.After(budget):
		return nil // a blocking waiter, not a load
	}
}

// runCtx drains a command the way bubbletea's runtime does.
func runCtx(t *testing.T, m *App, cmd tea.Cmd, budget time.Duration) {
	t.Helper()
	if cmd == nil {
		return
	}
	deadline := time.Now().Add(runCtxTotalBudget)
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0 && steps < runCtxMaxSteps; steps++ {
		if time.Now().After(deadline) {
			return
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func(fn tea.Cmd) { done <- fn() }(c)
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(budget):
			continue // a blocking waiter, not a load
		}
		if msg == nil {
			continue
		}
		// THE ROLLING WINDOW'S TICK IS TERMINAL — see the doc comment. Following its re-arm never returns.
		if _, isTick := msg.(refreshTickMsg); isTick {
			continue
		}
		// A batch is delivered as both the BatchMsg and its members; expanding here mirrors the runtime.
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, flattenCmds(batch)...)
			continue
		}
		nm, next := m.Update(msg)
		if app, ok := nm.(*App); ok {
			*m = *app
		}
		queue = append(queue, next)
	}
}

// A FRESH SHELL OPENS ON ALL PROJECTS, so nothing that predates the project column is hidden on
// launch.
func TestAFreshShellOpensOnAllProjects(t *testing.T) {
	m := appWithProjectService(t, &stubProjectList{})
	if m.projectScope != projectScopeAll {
		t.Errorf("a fresh shell opened on scope %q, want %q (All projects) — anything else hides conversations on "+
			"launch and blocks the launch-directory default", m.projectScope, projectScopeAll)
	}
}

// AND IT STAYS THERE WHEN NOTHING MATCHES, rather than falling to "No project": an unmatched launch directory is
// not a reason to hide the list, and inventing a workspace is worse than none.
func TestAnUnmatchedLaunchDirectoryStillOpensOnAllProjects(t *testing.T) {
	m := appWithProjectService(t, &stubProjectList{
		names:  []string{"Orchicon"},
		direcs: []string{"/home/me/projects/Orchicon"},
	})
	m.launchDir = "/tmp/somewhere-else"
	runCtx(t, m, m.Init(), runCtxCmdBudget)

	if m.projectScope != projectScopeAll {
		t.Errorf("scope = %q after launching from an unmatched directory, want All projects", m.projectScope)
	}
}

// A NORMAL SESSION FETCHES THE PROJECT LIST. This is the operator's report at the level it happened: not "the
// picker showed the wrong list" but "the request was never made".
func TestInitFetchesTheProjectList(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon", "ai-tools"},
		direcs: []string{"/home/me/projects/Orchicon", "/home/me/ai-tools"},
	}
	m := appWithProjectService(t, projects)
	m.launchDir = "/home/me/projects/Orchicon"

	runCtx(t, m, m.Init(), runCtxCmdBudget)

	if projects.callCount() == 0 {
		t.Fatal("Init() never called ListProjects — the workspace picker would offer nothing but \"All \"+\n" +
			"projects\", which is exactly what the operator saw")
	}
	if len(m.railProjects) != 2 {
		t.Fatalf("railProjects = %d after Init, want 2 (names: %v)", len(m.railProjects), m.railProjects)
	}
	if !m.railProjectsLoaded {
		t.Error("the project list landed but was not marked loaded, so every conversations load would re-fetch it")
	}
}

// AND THE PICKER SHOWS THEM — the operator's literal report: "/projects showed me All Projects and none".
func TestThePickerFromInitOffersTheRealProjects(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon", "ai-tools"},
		direcs: []string{"/home/me/projects/Orchicon", "/home/me/ai-tools"},
	}
	m := appWithProjectService(t, projects)
	runCtx(t, m, m.Init(), runCtxCmdBudget)

	runSlashTUI(t, m, "/project")
	if m.projectPick == nil {
		t.Fatal("/project opened no picker")
	}
	labels := make([]string, 0, len(m.projectPick.options))
	for _, o := range m.projectPick.options {
		labels = append(labels, o.Label)
	}
	for _, want := range []string{"Orchicon", "ai-tools"} {
		found := false
		for _, got := range labels {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the picker does not offer %q: %v — this is the operator's \"it showed me All Projects "+
				"and none\"", want, labels)
		}
	}
}

// THE LAUNCH DIRECTORY SELECTS THE WORKSPACE, end to end from Init: the operator's "the default project in the
// TUI should be the one based on the directory you launched it from".
func TestInitScopesToTheLaunchDirectory(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon", "ai-tools"},
		direcs: []string{"/home/me/projects/Orchicon", "/home/me/ai-tools"},
	}
	m := appWithProjectService(t, projects)
	m.launchDir = "/home/me/projects/Orchicon/src/nested"

	runCtx(t, m, m.Init(), runCtxCmdBudget)

	if m.projectScope != "prj-Orchicon" {
		t.Errorf("projectScope = %q after launching from inside the Orchicon project, want prj-Orchicon — "+
			"the rail would open on All projects", m.projectScope)
	}
	// AND A NEW CHAT IS TIED TO IT: "any conversation you make should be tied to the project that is currently
	// set across all of orch".
	if got := m.activeProjectID(); got != "prj-Orchicon" {
		t.Errorf("activeProjectID() = %q, want prj-Orchicon — a new conversation would be created unassigned", got)
	}
}

// AND THE RAIL TITLE NAMES IT: "in the conversations list rail at the top, it should say the project name that
// is currently selected".
func TestTheRailTitleNamesTheLaunchWorkspace(t *testing.T) {
	projects := &stubProjectList{
		names:  []string{"Orchicon"},
		direcs: []string{"/home/me/projects/Orchicon"},
	}
	m := appWithProjectService(t, projects)
	m.launchDir = "/home/me/projects/Orchicon"

	runCtx(t, m, m.Init(), runCtxCmdBudget)

	view := m.rightRailView()
	if !strings.Contains(view, "Orchicon") {
		t.Errorf("the rail does not name the selected project: %q", view)
	}
}

// A FILTER SHOWS THE MATCHING PROJECT — the operator's "/projects Orch would show me the project Orchicon".
func TestTheFilterNarrowsToTheMatchingProject(t *testing.T) {
	projects := &stubProjectList{names: []string{"Orchicon", "ai-tools", "Cigar Tracker"}}
	m := appWithProjectService(t, projects)
	runCtx(t, m, m.Init(), runCtxCmdBudget)

	runSlashTUI(t, m, "/project Orch")

	if m.projectPick == nil {
		t.Fatal("/project with a filter opened no picker")
	}
	labels := make([]string, 0, len(m.projectPick.options))
	for _, o := range m.projectPick.options {
		labels = append(labels, o.Label)
	}
	if len(labels) != 1 || labels[0] != "Orchicon" {
		t.Errorf("filter \"Orch\" produced %v, want exactly [Orchicon]", labels)
	}
}

// A FILTER THAT MATCHES NOTHING SAYS SO rather than presenting an empty box the operator has to interpret.
func TestAFilterWithNoMatchExplainsItself(t *testing.T) {
	projects := &stubProjectList{names: []string{"Orchicon"}}
	m := appWithProjectService(t, projects)
	runCtx(t, m, m.Init(), runCtxCmdBudget)

	runSlashTUI(t, m, "/project zzzz")

	if m.projectPick == nil {
		t.Fatal("/project with an unmatched filter opened no picker")
	}
	if len(m.projectPick.options) != 0 {
		t.Errorf("an unmatched filter produced %d options", len(m.projectPick.options))
	}
	// The empty state is only honest if the view says why, which renders from the filter.
	if !strings.Contains(m.projectPickerView(), "zzzz") {
		t.Errorf("the empty picker does not name the filter that emptied it: %q", m.projectPickerView())
	}
}

// A FAILED PROJECT LOAD IS RETRIED BY THE NEXT CONVERSATIONS LOAD rather than leaving the picker empty for the
// rest of the session — the transient-failure case that would otherwise look exactly like this bug.
func TestAFailedProjectLoadIsRetried(t *testing.T) {
	projects := &stubProjectList{names: []string{"Orchicon"}, failFirst: true}
	m := appWithProjectService(t, projects)
	runCtx(t, m, m.Init(), runCtxCmdBudget)

	if m.railProjectsLoaded {
		t.Fatal("fixture: the first load was supposed to fail")
	}
	if len(m.railProjects) != 0 {
		t.Fatal("fixture: a failed load populated the list")
	}
	// The next conversations load must retry it — the self-healing path that stops one transient failure from
	// leaving the picker empty for the rest of the session.
	nm, cmd := m.Update(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "one"}}})
	if app, ok := nm.(*App); ok {
		*m = *app
	}
	runCtx(t, m, cmd, runCtxCmdBudget)

	if len(m.railProjects) != 1 {
		t.Errorf("railProjects = %d after a retry, want 1 — a transient failure left the picker empty for the "+
			"rest of the session", len(m.railProjects))
	}
}
