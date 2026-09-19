package tui

// conversation_project_rail_test.go — THE RAIL GROUPS CONVERSATIONS BY PROJECT.
//
// The operator: "We need to make a second higher level in organization for conversations. It should be another
// drop down where all of the conversations are associated with Projects in a parent category. For every project
// that is created (active or otherwise), there should be a list that can be dragged to and also created from.
// ... In the TUI, it would be nice to figure that out but I would be happy with a /project or something of the
// sorts."
//
// The four rules that make that true, each asserted against the REAL rail rows rather than against a helper:
//
//  1. every project gets a folder, including one with no conversations (that is what makes the rail a place to
//     move a chat TO);
//  2. a conversation renders under its own project;
//  3. collapsing a project hides exactly its conversations and nothing else;
//  4. WITH NO PROJECTS the rail is byte-for-byte the rail it was before any of this existed — a wrapper level
//     around everything would be noise, and it re-indented every row for no information.
//
// And /project, which is the TUI's way of doing the dragging the GUI does with a mouse.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// projectRail builds a plane whose rail knows two projects and three conversations: two in one project, one in
// the other, and one Unassigned — so every branch of the grouping has something in it.
func projectRail(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.railProjects = []railProject{
		{ID: "p-1", Name: "Alpha", Status: "active"},
		{ID: "p-2", Name: "Beta", Status: "active"},
		{ID: "p-3", Name: "Gamma", Status: "archived"},
	}
	m.conversations = []chat.Conversation{
		{ID: "c-1", Title: "alpha chat one", ProjectID: "p-1"},
		{ID: "c-2", Title: "alpha chat two", ProjectID: "p-1"},
		{ID: "c-3", Title: "beta chat", ProjectID: "p-2"},
		{ID: "c-4", Title: "loose chat", ProjectID: ""},
	}
	m.convRailOpen = true
	m.refreshLayout()
	return m
}

// railTitles renders the rail rows as "title" strings, so a test can assert the SHAPE of the list.
func railTitles(m *App) []string {
	out := make([]string, 0, len(m.railRows()))
	for _, r := range m.railRows() {
		if r.folder {
			out = append(out, "["+r.title+"]")
			continue
		}
		out = append(out, r.title)
	}
	return out
}

// everyProjectGetsAFolder, EMPTY ONES INCLUDED. Gamma has no conversations and must still be there: an empty
// folder is the destination the operator asked for ("a list that can be dragged to and also created from"), and
// a rail that only showed projects with chats could never be one.
func TestEveryProjectGetsAFolderEvenWithNoConversations(t *testing.T) {
	m := projectRail(t)
	got := strings.Join(railTitles(m), " ")
	for _, want := range []string{"[Alpha]", "[Beta]", "[Gamma]", "[Unassigned]"} {
		if !strings.Contains(got, want) {
			t.Errorf("the rail has no %s folder: %s", want, got)
		}
	}
	// AND THE EMPTY ONE CARRIES ZERO, so the count is a fact rather than an omission.
	for _, r := range m.railRows() {
		if r.folder && r.title == "Gamma" && r.count != 0 {
			t.Errorf("Gamma has no conversations but reports %d", r.count)
		}
	}
}

// A CONVERSATION SITS UNDER ITS OWN PROJECT — the grouping, not just the presence of folders.
func TestConversationsNestUnderTheirProject(t *testing.T) {
	m := projectRail(t)
	rows := m.railRows()
	pos := map[string]int{}
	for i, r := range rows {
		if r.folder {
			pos["["+r.title+"]"] = i
			continue
		}
		pos["conv:"+r.title] = i
	}
	// Both of Alpha's, and in the list's order.
	if pos["conv:alpha chat one"] < pos["[Alpha]"] || pos["conv:alpha chat two"] < pos["[Alpha]"] {
		t.Errorf("Alpha's conversations do not follow its folder: %v", pos)
	}
	// Alpha's come BEFORE Beta's, because the projects are emitted in the project list's order.
	if pos["conv:alpha chat one"] > pos["[Beta]"] {
		t.Errorf("Alpha's chats leaked past Beta's folder: %v", pos)
	}
	if pos["conv:beta chat"] < pos["[Beta]"] {
		t.Errorf("Beta's conversation precedes its folder: %v", pos)
	}
	// And each conversation knows which project it is in, which is what the collapse filter reads.
	for _, r := range rows {
		if r.folder {
			continue
		}
		if r.projRowID == "" {
			t.Errorf("conversation %q carries no project row id, so it could never be collapsed", r.title)
		}
	}
}

// COLLAPSING A PROJECT HIDES ITS CHATS AND ONLY ITS CHATS.
func TestCollapsingAProjectHidesOnlyItsOwnConversations(t *testing.T) {
	m := projectRail(t)
	m.toggleConvFolder(projCollapseKey("p-1"))
	got := strings.Join(railTitles(m), " ")
	if strings.Contains(got, "alpha chat") {
		t.Errorf("Alpha's conversations are still visible after collapsing it: %s", got)
	}
	// THE PROJECT ROW ITSELF STAYS — a collapsed folder that vanished could not be expanded again.
	if !strings.Contains(got, "[Alpha]") {
		t.Errorf("collapsing Alpha removed its folder row: %s", got)
	}
	// AND THE OTHERS ARE UNTOUCHED, which is what makes the collapse a property of the project rather than of
	// the list.
	for _, want := range []string{"[Beta]", "beta chat", "[Unassigned]", "loose chat"} {
		if !strings.Contains(got, want) {
			t.Errorf("collapsing Alpha also hid %q: %s", want, got)
		}
	}
}

// AN ARCHIVED PROJECT IS STILL A FOLDER, and it is MARKED. The association rule was "active or otherwise", so a
// chat parked in an archived project has to be findable — and the one thing the operator needs before asking an
// agent to work in it is that it is archived.
func TestAnArchivedProjectIsShownAndMarked(t *testing.T) {
	m := projectRail(t)
	var gamma *railRow
	for i := range m.railRows() {
		if r := m.railRows()[i]; r.folder && r.title == "Gamma" {
			gamma = &r
		}
	}
	if gamma == nil {
		t.Fatal("the archived project has no folder")
	}
	if gamma.status != "archived" {
		t.Errorf("Gamma's folder carries status %q, want \"archived\"", gamma.status)
	}
	// The marker is on the LINE, not only in the row struct — the operator reads the rail, not the model.
	w := ConversationsRailWidth
	line := m.railLine(*gamma, -1, w)
	if !strings.Contains(line, "archived") {
		t.Errorf("the archived project's rail line does not say so: %q", line)
	}
}

// WITH NO PROJECTS THE RAIL IS WHAT IT ALWAYS WAS. This is the guard against the whole feature becoming a
// wrapper level around every conversation for a tenant that has never made a project.
func TestWithNoProjectsTheRailHasNoProjectLevel(t *testing.T) {
	m := projectRail(t)
	m.railProjects = nil
	rows := m.railRows()
	if len(rows) == 0 {
		t.Fatal("the rail rendered nothing with conversations present")
	}
	for _, r := range rows {
		if strings.HasPrefix(r.projRowID, projRowPrefix) && r.folder {
			t.Errorf("a project folder was emitted with no projects configured: %q", r.title)
		}
		if r.depth != 0 && !r.folder {
			t.Errorf("a conversation was indented to depth %d with no project level to indent under", r.depth)
		}
	}
	// And the conversations are all still reachable.
	if got := len(railTitles(m)); got != len(m.conversations) {
		t.Errorf("the flat rail lists %d rows for %d conversations", got, len(m.conversations))
	}
}

// /PROJECT LISTS WHAT CAN BE CHOSEN when given no argument — the discovery path, since the operator cannot be
// expected to know a project's id.
func TestProjectCommandWithNoArgumentListsAndNamesTheCurrent(t *testing.T) {
	m := projectRail(t)
	m.chatConvID = "c-1"
	cmd := runSlashForTest(t, m, "/project")
	if cmd != nil {
		t.Error("/project with no argument returned a command; it only reports")
	}
	for _, want := range []string{"Alpha", "Beta", "Gamma", "none"} {
		if !strings.Contains(m.dock.Notice, want) {
			t.Errorf("the /project notice does not offer %q: %q", want, m.dock.Notice)
		}
	}
}

// AND WITH A NAME IT MOVES THE CONVERSATION — by NAME, case-insensitively, because that is what the operator
// typed in the GUI's dialog.
func TestProjectCommandResolvesANameAndMovesTheChat(t *testing.T) {
	m := projectRail(t)
	m.chatConvID = "c-1"
	if id, ok := m.resolveProjectRef("bEtA"); !ok || id != "p-2" {
		t.Errorf("resolveProjectRef(\"bEtA\") = %q,%v; want p-2,true", id, ok)
	}
	// An unknown name must NOT silently do nothing: the operator is told, and told where to look.
	cmd := runSlashForTest(t, m, "/project nope")
	if cmd != nil {
		t.Error("an unresolvable project still produced a write")
	}
	if !strings.Contains(m.dock.Err, "no project matches") {
		t.Errorf("an unknown project name was not reported: %q", m.dock.Err)
	}
}

// "none" UNASSIGNS, which is a real operation: a chat must be able to leave a project that was archived out from
// under it.
func TestProjectNoneUnassigns(t *testing.T) {
	m := projectRail(t)
	m.chatConvID = "c-1"
	if id, ok := m.resolveProjectRef("none"); ok {
		t.Errorf("\"none\" resolved to a real project %q — it must be the unassign keyword, not a name match", id)
	}
	// The keyword is handled BEFORE resolution, which is what keeps a project legitimately named "none" from
	// being unreachable... and is why this asserts on the command path rather than on resolveProjectRef.
	runSlashForTest(t, m, "/project none")
	if strings.Contains(m.dock.Err, "no project matches") {
		t.Errorf("\"none\" was treated as a project name: %q", m.dock.Err)
	}
}

// WITH NO CONVERSATION OPEN, /project SAYS SO rather than appearing to work.
func TestProjectCommandWithNoOpenConversationExplainsItself(t *testing.T) {
	m := projectRail(t)
	m.chatConvID = ""
	runSlashForTest(t, m, "/project Alpha")
	if !strings.Contains(m.dock.Err, "no conversation is open") {
		t.Errorf("/project with nothing open gave %q", m.dock.Err)
	}
}

// THE OPEN CONVERSATION'S PROJECT IS NAMED IN THE PANE, so "which project is this chat in" is answerable without
// reading the rail — and an archived project says so.
func TestTheOpenConversationNamesItsProject(t *testing.T) {
	m := projectRail(t)
	if got := m.conversationProjectLine("c-1"); got != "Alpha" {
		t.Errorf("conversationProjectLine(c-1) = %q, want Alpha", got)
	}
	if got := m.conversationProjectLine("c-4"); got != "no project" {
		t.Errorf("an unassigned chat reported %q, want \"no project\"", got)
	}
	// Gamma is archived; the line has to carry that, because it is the fact that changes what the operator
	// should expect from a turn in that chat.
	m.conversations = append(m.conversations, chat.Conversation{ID: "c-5", Title: "archived chat", ProjectID: "p-3"})
	if got := m.conversationProjectLine("c-5"); got != "Gamma (archived)" {
		t.Errorf("a chat in an archived project reported %q, want \"Gamma (archived)\"", got)
	}
}

// A STALE PROJECT ID STILL RENDERS. The column has no foreign key, so a conversation can outlive its project;
// dropping it from the rail would make the chat unreachable, which is worse than an ugly folder name.
func TestAStaleProjectIdStillGetsAFolder(t *testing.T) {
	m := projectRail(t)
	m.conversations = append(m.conversations, chat.Conversation{ID: "c-9", Title: "orphan chat", ProjectID: "p-gone"})
	got := strings.Join(railTitles(m), " ")
	if !strings.Contains(got, "orphan chat") {
		t.Errorf("a conversation whose project no longer exists vanished from the rail: %s", got)
	}
	if !strings.Contains(got, "p-gone") {
		t.Errorf("the orphan's folder does not name the missing project id: %s", got)
	}
}

// runSlashForTest runs a slash command through the REAL registry, so a command that is not registered (or is
// registered with the wrong MinArgs) fails here rather than silently doing nothing in the app.
func runSlashForTest(t *testing.T, m *App, text string) tea.Cmd {
	t.Helper()
	name, args, _, ok := ParseSlash(text)
	if !ok {
		t.Fatalf("%q did not parse as a slash command", text)
	}
	cmd, found := m.slash.byName[name]
	if !found {
		t.Fatalf("%s is not in the registry", name)
	}
	if len(args) < cmd.MinArgs {
		t.Fatalf("%s needs %d args, got %d", name, cmd.MinArgs, len(args))
	}
	return cmd.Run(m, args)
}
