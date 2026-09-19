package tui

// conversation_project_test.go — PROJECTS ARE WORKSPACES: a scope you pick, and what it hides.
//
// The operator, correcting the first attempt:
//
//   "The implementation is wrong. You separated categories out as 'folders' and then you made them two
//    completely different panes. I assume you did something similar to the TUI. I wanted a hierarchy. So a
//    conversation would belong to a project and inside the project it would still have the normal categories we
//    had before. ... Projects are WORKSPACES essentially. ... you would only see THAT PROJECT'S Conversations
//    and Categories. ... In the TUI the equivalent would be /project to set the active project. A list should pop
//    up to make it easier to pick the right one when you type /project slash command."
//
// The assertions below are the four things that make that true, plus the one that keeps it from regressing:
//
//  1. a scope hides every OTHER project's conversations (the promise of a workspace);
//  2. the category folders are untouched INSIDE the scope — same folders, same collapse, same counts;
//  3. EVERY project is offered, including ones with no conversations, because a workspace you cannot select is
//     not a workspace;
//  4. "All projects" is still the default and still shows everything, so nothing that predates the project
//     column disappears on launch;
//  5. /project opens a LIST (no argument) or moves the open chat (with one).

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// seedConvCategory assigns conversation ids to a conversation category through the SAME cache the app applies a
// server response into, so the fixture exercises the real grouping path rather than a parallel one.
func seedConvCategory(t *testing.T, m *App, id, name string, convIDs ...string) string {
	t.Helper()
	cat := &apiv1.Category{
		Id: id, Name: name,
		TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
	}
	assigns := make([]*apiv1.CategoryAssignment, 0, len(convIDs))
	for _, cid := range convIDs {
		assigns = append(assigns, &apiv1.CategoryAssignment{
			EntityId:   cid,
			CategoryId: id,
			TargetType: apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION,
		})
	}
	m.applyCategorySet(apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_CONVERSATION, []*apiv1.Category{cat}, assigns)
	return id
}

// convTitleOf is the title behind a conversation row, for fixtures that need to find a specific row.
func convTitleOf(m *App, r railRow) string {
	if r.folder || r.conv < 0 || r.conv >= len(m.conversations) {
		return ""
	}
	return m.conversations[r.conv].Title
}

// scopePlane builds a rail that knows three projects and five conversations spread across them, so every branch
// of the scope logic has something in it.
func scopePlane(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.railProjects = []railProject{
		{ID: "p-1", Name: "Alpha", Status: "active"},
		{ID: "p-2", Name: "Beta", Status: "active"},
		{ID: "p-3", Name: "Gamma", Status: "archived"},
		{ID: "p-4", Name: "Delta", Status: "active"}, // no conversations at all
	}
	m.conversations = []chat.Conversation{
		{ID: "c-1", Title: "alpha chat one", ProjectID: "p-1"},
		{ID: "c-2", Title: "alpha chat two", ProjectID: "p-1"},
		{ID: "c-3", Title: "beta chat", ProjectID: "p-2"},
		{ID: "c-4", Title: "loose chat", ProjectID: ""},
	}
	m.projectScope = projectScopeAll
	m.convRailOpen = true
	m.refreshLayout()
	return m
}

// railTitles renders the rail's visible rows as strings, so a test can assert its SHAPE.
func railTitles(m *App) []string {
	rows := m.railRows()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.folder {
			out = append(out, "["+r.title+"]")
			continue
		}
		out = append(out, r.title)
	}
	return out
}

// --- the scope itself -----------------------------------------------------------------------------

// A SCOPE HIDES EVERY OTHER PROJECT'S CONVERSATIONS. That is the whole promise: the operator cannot see another
// workspace's chats without switching to it deliberately.
func TestAProjectScopeHidesOtherProjectsConversations(t *testing.T) {
	m := scopePlane(t)
	// No categories are configured, so the rail is flat and every row is a conversation.
	m.projectScope = "p-1"
	got := strings.Join(railTitles(m), " ")
	if !strings.Contains(got, "alpha chat one") || !strings.Contains(got, "alpha chat two") {
		t.Fatalf("the scoped rail lost its own conversations: %s", got)
	}
	for _, other := range []string{"beta chat", "loose chat"} {
		if strings.Contains(got, other) {
			t.Errorf("a conversation from another workspace is visible in the p-1 scope: %s", got)
		}
	}
}

// AND THE DEFAULT SCOPE SHOWS EVERYTHING, so a chat that predates the project column cannot vanish on launch.
func TestTheAllProjectsScopeShowsEveryConversation(t *testing.T) {
	m := scopePlane(t)
	m.projectScope = projectScopeAll
	if n := len(railTitles(m)); n != len(m.conversations) {
		t.Errorf("the All-projects rail lists %d rows for %d conversations", n, len(m.conversations))
	}
}

// THE UNASSIGNED SCOPE IS A REAL PLACE, not the absence of one: those chats have to stay findable, and they are
// also where a chat lands when it is moved out of a project.
func TestTheUnassignedScopeShowsOnlyUnassignedChats(t *testing.T) {
	m := scopePlane(t)
	m.projectScope = unassignedScope
	got := railTitles(m)
	if len(got) != 1 || got[0] != "loose chat" {
		t.Errorf("the No-project scope = %v, want [loose chat]", got)
	}
}

// AN EMPTY SCOPE RENDERS NOBODY. A project with no conversations must show an empty rail rather than the
// previous project's chats — the failure mode that would make the scope feel broken rather than empty.
func TestAnEmptyProjectScopeRendersNoRows(t *testing.T) {
	m := scopePlane(t)
	m.projectScope = "p-4" // Delta, which has no conversations
	if rows := m.railRows(); len(rows) != 0 {
		t.Errorf("the empty workspace rendered %d rows: %v", len(rows), railTitles(m))
	}
}

// A CHAT WHOSE PROJECT IS GONE IS STILL REACHABLE. The column carries no foreign key, so a conversation can
// outlive its project; its scope must still list it or the chat becomes unreachable with no error anywhere.
func TestAStaleProjectScopeStillListsItsConversation(t *testing.T) {
	m := scopePlane(t)
	m.conversations = append(m.conversations, chat.Conversation{ID: "c-9", Title: "orphan chat", ProjectID: "p-gone"})
	m.projectScope = "p-gone"
	got := railTitles(m)
	if len(got) != 1 || got[0] != "orphan chat" {
		t.Errorf("the orphan's scope = %v, want [orphan chat]", got)
	}
}

// --- the category layer is untouched inside the scope ---------------------------------------------------------

// THE CATEGORY FOLDERS STILL WORK INSIDE A SCOPE — the operator's "inside the project it would still have the
// normal categories we had before", and the reason the first attempt was wrong.
func TestCategoriesStillGroupInsideAProjectScope(t *testing.T) {
	m := scopePlane(t)
	catID := seedConvCategory(t, m, "cat-research", "Research", "c-1", "c-3")

	// Scoped to p-1: the category folder holds only p-1's member, and the p-2 chat inside the same category is
	// NOT shown.
	m.projectScope = "p-1"
	got := strings.Join(railTitles(m), " ")
	if !strings.Contains(got, "[Research]") {
		t.Fatalf("the category folder vanished inside a scope: %s", got)
	}
	if !strings.Contains(got, "alpha chat one") {
		t.Errorf("the scoped in-category chat is missing: %s", got)
	}
	if strings.Contains(got, "beta chat") {
		t.Errorf("a chat from another project leaked into the folder: %s", got)
	}
	_ = catID
}

// AND COLLAPSING A FOLDER STILL WORKS, because the collapse key is still the category id — the field that
// projects briefly took over.
func TestCollapsingACategoryStillHidesItsMembersInsideAScope(t *testing.T) {
	m := scopePlane(t)
	catID := seedConvCategory(t, m, "cat-research", "Research", "c-1")
	m.projectScope = "p-1"
	m.toggleConvFolder(catID)
	got := strings.Join(railTitles(m), " ")
	if strings.Contains(got, "alpha chat one") {
		t.Errorf("the collapsed folder's member is still visible: %s", got)
	}
	if !strings.Contains(got, "[Research]") {
		t.Errorf("collapsing removed the folder row itself, so it could never be expanded again: %s", got)
	}
}

// --- the picker's contents, as pure functions -----------------------------------------------------------------

// EVERY PROJECT GETS AN OPTION, counted — including an empty one, whose 0 is how you know it is empty BEFORE
// switching to it.
func TestScopeOptionsOfferEveryProjectIncludingEmptyOnes(t *testing.T) {
	opts := projectScopeOptions(scopePlane(t).railProjects, scopePlane(t).conversations)
	byVal := map[string]projectScopeOption{}
	for _, o := range opts {
		byVal[o.Value] = o
	}
	if got := byVal["p-4"].Count; got != 0 {
		t.Errorf("the empty project reports %d conversations, want 0", got)
	}
	if byVal["p-1"].Count != 2 {
		t.Errorf("p-1 count = %d, want 2", byVal["p-1"].Count)
	}
	if byVal[projectScopeAll].Count != 4 {
		t.Errorf("All projects count = %d, want 4", byVal[projectScopeAll].Count)
	}
	if byVal[unassignedScope].Count != 1 {
		t.Errorf("No project count = %d, want 1", byVal[unassignedScope].Count)
	}
}

// AN ARCHIVED PROJECT IS MARKED AND STILL SELECTABLE — the association rule is the operator's "active or
// otherwise", so an archived workspace is valid and the one fact worth knowing is that it is archived.
func TestAnArchivedProjectIsOfferedAndMarked(t *testing.T) {
	opts := projectScopeOptions(scopePlane(t).railProjects, scopePlane(t).conversations)
	var gamma *projectScopeOption
	for i := range opts {
		if opts[i].Value == "p-3" {
			gamma = &opts[i]
		}
	}
	if gamma == nil {
		t.Fatal("the archived project has no option, so it cannot be chosen as a workspace")
	}
	if !gamma.Archived {
		t.Error("the archived project is not marked")
	}
	if opts[0].Archived {
		t.Error("All projects is marked archived")
	}
}

// A PROJECT ID THAT IS ONLY REFERENCED STILL GETS AN OPTION, or its conversation would be unreachable.
func TestAReferencedButMissingProjectStillGetsAnOption(t *testing.T) {
	m := scopePlane(t)
	m.conversations = append(m.conversations, chat.Conversation{ID: "c-9", ProjectID: "p-gone"})
	opts := projectScopeOptions(m.railProjects, m.conversations)
	found := false
	for _, o := range opts {
		if o.Value == "p-gone" {
			found = true
			if o.Count != 1 {
				t.Errorf("the orphan's option counts %d, want 1", o.Count)
			}
		}
	}
	if !found {
		t.Error("a referenced-but-unlisted project id has no option — its conversation is unreachable")
	}
}

// --- /project -------------------------------------------------------------------------------------------------

// BARE /project OPENS A LIST. The operator: "A list should pop up to make it easier to pick the right one when
// you type /project slash command." A notice listing names would not be a list you can choose from.
func TestBareProjectOpensThePicker(t *testing.T) {
	m := scopePlane(t)
	if err := runSlashTUI(t, m, "/project"); err != "" {
		t.Fatalf("/project returned an error notice: %s", err)
	}
	if m.projectPick == nil {
		t.Fatal("/project with no argument did not open the picker")
	}
	if m.projectPick.moveConvID != "" {
		t.Error("the picker opened in move mode when it was choosing the WORKSPACE")
	}
	if len(m.projectPick.options) < 5 {
		t.Errorf("the picker offers %d options; expected All + 4 projects + No project",
			len(m.projectPick.options))
	}
}

// AND IT OPENS ON THE CURRENT SCOPE, so reopening it shows where you already are.
func TestThePickerOpensOnTheCurrentScope(t *testing.T) {
	m := scopePlane(t)
	m.projectScope = "p-2"
	runSlashTUI(t, m, "/project")
	opt, ok := m.projectPick.selected()
	if !ok || opt.Value != "p-2" {
		t.Errorf("the picker opened on %+v, want p-2", opt)
	}
}

// CHOOSING A SCOPE CHANGES WHAT THE RAIL SHOWS — end to end, through the real key path.
func TestChoosingAScopeRescopesTheRail(t *testing.T) {
	m := scopePlane(t)
	runSlashTUI(t, m, "/project")
	// Walk the cursor to p-1 (option index 1) and commit.
	feed(t, m, tea.KeyMsg{Type: tea.KeyDown})
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if app.projectScope != "p-1" {
		t.Fatalf("the scope is %q after choosing the first project", app.projectScope)
	}
	if app.projectPick != nil {
		t.Error("the picker stayed open after a selection")
	}
	got := strings.Join(railTitles(app), " ")
	if strings.Contains(got, "beta chat") {
		t.Errorf("the rail still shows another workspace's chat: %s", got)
	}
}

// ESCAPE CLOSES IT WITHOUT CHANGING ANYTHING — a list you cannot back out of is a trap.
func TestEscapeClosesThePickerWithoutChangingTheScope(t *testing.T) {
	m := scopePlane(t)
	m.projectScope = "p-2"
	runSlashTUI(t, m, "/project")
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if app.projectPick != nil {
		t.Error("escape did not close the picker")
	}
	if app.projectScope != "p-2" {
		t.Errorf("escape changed the scope to %q", app.projectScope)
	}
}

// THE PICKER CONSUMES KEYS WHILE IT IS OPEN, so a keystroke aimed at the list cannot act on the rail behind it —
// or reach the composer.
func TestThePickerOwnsItsKeys(t *testing.T) {
	m := scopePlane(t)
	runSlashTUI(t, m, "/project")
	before := m.dock.Value()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("xyz")})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if got := app.dock.Value(); got != before {
		t.Errorf("typing while the picker is open leaked into the composer: %q", got)
	}
}

// /projects TAKES A FILTER, NOT A SELECTION. The operator: "If I type '/projects Orch', it would show me the
// project Orchicon." So the argument narrows the list and the operator still picks from it — and a prefix that
// matches several projects shows all of them rather than guessing at one.
func TestProjectsWithAnArgumentFiltersTheList(t *testing.T) {
	m := scopePlane(t)
	runSlashTUI(t, m, "/projects beta")
	if m.projectPick == nil {
		t.Fatal("/projects with a filter did not open the picker")
	}
	if len(m.projectPick.options) != 1 || m.projectPick.options[0].Label != "Beta" {
		t.Fatalf("the filter did not narrow the list to Beta: %+v", m.projectPick.options)
	}

	// CASE-INSENSITIVE, and a partial word finds it — the operator should not have to remember the casing, or
	// type a whole name.
	runSlashTUI(t, m, "/projects ALP")
	if len(m.projectPick.options) != 1 || m.projectPick.options[0].Label != "Alpha" {
		t.Errorf("a case-insensitive partial filter did not find Alpha: %+v", m.projectPick.options)
	}

	// ALL PROJECTS AND NO PROJECT ARE NOT PROJECTS: neither may survive a filter, or "No" would match "No
	// project" and a filter for a real project could be polluted by a non-project row.
	runSlashTUI(t, m, "/projects no")
	for _, o := range m.projectPick.options {
		if o.Value == projectScopeAll || o.Value == unassignedScope {
			t.Errorf("the non-project scope %q survived a filter", o.Label)
		}
	}

	// A FILTER THAT MATCHES NOTHING SHOWS AN EMPTY LIST, and the picker says why — never a silent list that
	// looks like the tenant has no projects.
	runSlashTUI(t, m, "/projects zzzz")
	if len(m.projectPick.options) != 0 {
		t.Errorf("a non-matching filter left %d options", len(m.projectPick.options))
	}
	if view := m.projectPickerView(); !strings.Contains(view, "nothing matches") {
		t.Errorf("an empty filtered list does not say why: %q", view)
	}
}

// AND IT IS REACHABLE AS /projects AND /project — the operator typed the plural, and the singular was the
// previous name, so both resolve rather than one being a silent "unknown command".
func TestBothProjectSpellingsResolve(t *testing.T) {
	m := scopePlane(t)
	for _, name := range []string{"/projects", "/project"} {
		m.projectPick = nil
		runSlashTUI(t, m, name)
		if m.projectPick == nil {
			t.Errorf("%s did not open the picker", name)
		}
	}
}

// THE OPEN CONVERSATION CAN STILL BE MOVED, but by its OWN action rather than by overloading the workspace
// command — `m` in the picker, and /project-move to open it in that mode.
func TestTheMoveActionIsReachableAndDistinct(t *testing.T) {
	m := scopePlane(t)
	m.chatConvID = "c-1"
	runSlashTUI(t, m, "/project-move")
	if m.projectPick == nil || m.projectPick.moveConvID != "c-1" {
		t.Fatalf("/project-move did not open the picker in move mode: %+v", m.projectPick)
	}
	// Its title says it is a WRITE, which is what tells the two uses apart.
	if view := m.projectPickerView(); !strings.Contains(view, "MOVE") {
		t.Errorf("the move picker does not say it is a move: %q", view)
	}

	// With nothing open, it explains itself instead of opening a picker whose enter would do nothing.
	m.projectPick = nil
	m.chatConvID = ""
	if err := runSlashTUI(t, m, "/project-move"); !strings.Contains(err, "no conversation is open") {
		t.Errorf("moving with nothing open gave %q", err)
	}
}

// BARE /projects NEEDS NO CONVERSATION AT ALL. Choosing the workspace is a VIEW, and a fresh session has no
// conversation open — so the list must open regardless. (The MOVE form's own refusal is asserted above.)
func TestChoosingTheWorkspaceNeedsNoOpenConversation(t *testing.T) {
	m := scopePlane(t)
	m.chatConvID = ""
	runSlashTUI(t, m, "/projects")
	if m.projectPick == nil {
		t.Error("bare /projects needs an open conversation to list workspaces, which it must not")
	}
}

// THE WIRING IS THE BUG, SO THE WIRING IS TESTED.
//
// Every other test in this file covers a PURE function — the filter, the launch-directory match, the active
// project. None of them would have caught the fault the operator actually hit, which was that the project list
// was never FETCHED in a normal session: Init() loaded the conversations directly, so loadRailProjects never
// ran, and the picker offered nothing but "All projects". A test of the matching rule cannot see a missing
// request.
//
// So this drives the real message path: a conversations load on a plane whose project list has never landed
// must ALSO fetch the projects, and the fetch must actually reach the ProjectService.
func TestAConversationsLoadAlsoFetchesProjects(t *testing.T) {
	projects := &stubProjectList{names: []string{"Orchicon", "ai-tools"}}
	m := appWithProjectService(t, projects)
	m.railProjectsLoaded = false
	m.railProjects = nil

	// A conversations load, exactly as the controller delivers it.
	nm, cmd := m.Update(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "one"}}})
	app, ok := nm.(*App)
	if !ok || app == nil {
		t.Fatal("Update returned a non-App model")
	}
	if cmd == nil {
		t.Fatal("a conversations load produced no command, so the projects would never be fetched — this is " +
			"precisely the state that left the workspace picker empty")
	}
	// DRAIN IT THROUGH runCtx, NEVER BY CALLING EACH MEMBER INLINE.
	//
	// THIS IS WHERE THE SUITE HUNG, so the reason stays written down beside the code that caused it: the batch
	// is tea.Batch(onChatWake, loadRailProjects, waitChat), and waitChat is
	// `select { case <-wake: …; case c := <-cmds: … }` with no timeout and nobody sending. Calling it directly
	// blocks that goroutine FOREVER — there is no budget and no bail-out — so `make rebuild-dev` sat inside
	// internal/tui and never reached internal/version or the frontend build. runCtx runs each command under a
	// BOUNDED wait and skips one that does not answer, which is exactly what a long-lived waiter is.
	runCtx(t, app, cmd, 3*time.Second)
	if app.railProjectsLoaded != true {
		t.Error("the project list was never marked loaded, so the fetch did not complete")
	}
	if projects.calls == 0 {
		t.Error("the ProjectService was never called — the conversations load fetched no projects")
	}
	if got := len(app.railProjects); got != 2 {
		t.Errorf("railProjects = %d, want 2 (the picker would show no real projects)", got)
	}
}

// AND IT DOES NOT RE-FETCH ONCE LOADED — the self-healing retry must not become a per-load request for a
// tenant whose project list is legitimately empty.
func TestProjectsAreFetchedOnlyOnce(t *testing.T) {
	projects := &stubProjectList{}
	m := appWithProjectService(t, projects)
	m.railProjectsLoaded = true // as after a successful load

	_, cmd := m.Update(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1"}}})
	// The same BOUNDED drain as its sibling above: whatever this batch holds, one blocking waiter inside it must
	// not be able to hang the package.
	runCtx(t, m, cmd, 3*time.Second)
	if projects.calls != 0 {
		t.Errorf("the project list was re-fetched %d time(s) after it had loaded", projects.calls)
	}
}

// --- the row's project label ---------------------------------------------------------------------------------

// IN THE ALL-PROJECTS SCOPE A CHAT'S PROJECT IS NAMED ON ITS ROW, because that scope is the one where the rail
// title does NOT answer it. Inside a project scope every row IS that project, so the label would be a repeat.
func TestTheProjectLabelAppearsOnlyWhereItIsNotObvious(t *testing.T) {
	m := scopePlane(t)
	var row string
	for _, r := range m.railRows() {
		if convTitleOf(m, r) == "beta chat" {
			row = m.railLine(r, -1, ConversationsRailWidth)
		}
	}
	if row == "" {
		t.Fatal("fixture: beta chat is not in the All-projects rail")
	}
	if !strings.Contains(row, "Beta") {
		t.Errorf("with All projects active the row does not name its project: %q", row)
	}

	// Scoped to Beta, the same chat's row must NOT repeat the name.
	m.projectScope = "p-2"
	row = ""
	for _, r := range m.railRows() {
		if convTitleOf(m, r) == "beta chat" {
			row = m.railLine(r, -1, ConversationsRailWidth)
		}
	}
	if row == "" {
		t.Fatal("fixture: beta chat is not in the p-2 rail")
	}
	if strings.Contains(row, "Beta") {
		t.Errorf("the row repeats the project inside its own scope: %q", row)
	}
}

// THE RAIL TITLE NAMES THE ACTIVE WORKSPACE, so a scope you are in is one you can see.
//
// The assertion is on the TITLE LINE, not on the whole rail: in the All-projects scope the row labels name each
// chat's project (which is the point of them), so "Alpha" appearing somewhere in the panel is correct there and
// says nothing about the title.
func TestTheRailTitleNamesTheActiveWorkspace(t *testing.T) {
	titleOf := func(m *App) string {
		lines := strings.Split(m.rightRailView(), "\n")
		for _, l := range lines {
			if strings.Contains(l, "Conversations") {
				return l
			}
		}
		return ""
	}

	m := scopePlane(t)
	m.projectScope = "p-1"
	if title := titleOf(m); !strings.Contains(title, "Alpha") {
		t.Errorf("the rail title does not name the active project: %q", title)
	}

	m.projectScope = projectScopeAll
	if title := titleOf(m); strings.Contains(title, "Alpha") {
		t.Errorf("the All-projects rail title names a single project: %q", title)
	}
}

// runSlashTUI runs a slash command through the REAL registry and returns its error notice ("" when it did not
// set one). Going through the registry means an unregistered command — or a wrong MinArgs — fails here rather
// than silently doing nothing in the app.
func runSlashTUI(t *testing.T, m *App, text string) string {
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
	m.dock.SetError("")
	m.dock.SetNotice("")
	_ = cmd.Run(m, args)
	return m.dock.Err
}
