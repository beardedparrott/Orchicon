package tui

// project_launch_scope_test.go — THE WORKSPACE COMES FROM WHERE YOU LAUNCHED, AND NEW CHATS LAND IN IT.
//
// The operator, after finding the picker offered nothing:
//
//	"Projects should be pulled from the actual projects list in Orchicon. The default project in the TUI should
//	 be the one based on the directory you launched it from and any conversation you make should be tied to the
//	 project that is currently set across all of orch. In the conversations list rail at the top, it should say
//	 the project name that is currently selected."
//
// The first half of that was a WIRING bug, not a design one: `Init()` loaded the conversations directly, so
// `loadRailProjects` never ran in a normal session and the picker had no projects to offer. The self-healing
// load and the launch-directory default are what these tests pin.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// scopePlane2 is scopePlane with two projects that have real DIRECTORIES, so a launch directory can be matched
// against them — which is the whole subject of these tests.
func scopePlane2(t *testing.T) *App {
	t.Helper()
	m := scopePlane(t)
	m.railProjects = []railProject{
		{ID: "p-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
		{ID: "p-tools", Name: "ai-tools", Status: "active", Dir: "/home/me/ai-tools"},
	}
	m.conversations = []chat.Conversation{
		{ID: "c-1", Title: "orch chat", ProjectID: "p-orch"},
		{ID: "c-2", Title: "tools chat", ProjectID: "p-tools"},
		{ID: "c-3", Title: "loose chat", ProjectID: ""},
	}
	m.projectScope = projectScopeAll
	m.projectScopeChosen = false
	m.refreshLayout()
	return m
}

// THE LAUNCH DIRECTORY'S PROJECT BECOMES THE WORKSPACE — the operator's "the default project in the TUI should
// be the one based on the directory you launched it from".
func TestTheLaunchDirectoryBecomesTheWorkspace(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/home/me/projects/Orchicon"
	m.applyLaunchDirScope()
	if m.projectScope != "p-orch" {
		t.Errorf("the scope is %q, want p-orch (the launch directory's project)", m.projectScope)
	}
}

// LAUNCHING FROM INSIDE IT COUNTS TOO, using the SAME boundary rule as the launch prompt — a subdirectory of a
// project is still that project, which is the monorepo/package case.
func TestLaunchingFromASubdirectoryStillMatches(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/home/me/projects/Orchicon/internal/tui"
	m.applyLaunchDirScope()
	if m.projectScope != "p-orch" {
		t.Errorf("a subdirectory did not resolve to its project, scope = %q", m.projectScope)
	}
}

// A SIBLING WITH A SHARED PREFIX IS NOT INSIDE IT. /home/me/projects/Orchicon-notes must not resolve to
// Orchicon — the boundary is a path separator, and a string-prefix test would say it is.
func TestASiblingWithASharedPrefixDoesNotMatch(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/home/me/projects/Orchicon-notes"
	m.applyLaunchDirScope()
	if m.projectScope != projectScopeAll {
		t.Errorf("a sibling directory resolved to %q; the containment boundary must be a path separator",
			m.projectScope)
	}
}

// AND A DIRECTORY THAT MATCHES NOTHING LEAVES THE SCOPE ALONE — an unmatched launch directory is not a reason
// to hide the conversation list, and inventing a default workspace would be worse than none.
func TestAnUnmatchedLaunchDirectoryLeavesAllProjects(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/tmp/somewhere-else"
	m.applyLaunchDirScope()
	if m.projectScope != projectScopeAll {
		t.Errorf("an unmatched directory chose the scope %q, want All projects", m.projectScope)
	}
}

// A DELIBERATE CHOICE WINS, even over a matching directory — including one made in the window before the
// project list arrived.
func TestADeliberateChoiceIsNotOverruledByTheLaunchDirectory(t *testing.T) {
	m := scopePlane2(t)
	m.launchDir = "/home/me/projects/Orchicon"
	m.setProjectScope("p-tools") // the operator chose
	m.projectScope = "p-tools"
	m.applyLaunchDirScope()
	if m.projectScope != "p-tools" {
		t.Errorf("the launch directory overruled a deliberate choice: scope = %q", m.projectScope)
	}
}

// THE DEEPEST MATCH WINS, for the nested case: a directory inside two projects belongs to the more specific
// one, which is the workspace the operator is actually in.
func TestTheDeepestMatchWins(t *testing.T) {
	m := scopePlane2(t)
	m.railProjects = append(m.railProjects,
		railProject{ID: "p-inner", Name: "inner", Status: "active", Dir: "/home/me/projects/Orchicon/vendor/x"})
	m.launchDir = "/home/me/projects/Orchicon/vendor/x/pkg"
	m.applyLaunchDirScope()
	if m.projectScope != "p-inner" {
		t.Errorf("scope = %q, want p-inner (the deepest project containing the directory)", m.projectScope)
	}
}

// A NEW CONVERSATION IS TIED TO THE ACTIVE WORKSPACE — the operator's "any conversation you make should be tied
// to the project that is currently set across all of orch".
func TestANewConversationTakesTheActiveProject(t *testing.T) {
	m := scopePlane2(t)
	m.setProjectScope("p-orch")
	if got := m.activeProjectID(); got != "p-orch" {
		t.Errorf("activeProjectID() = %q, want p-orch — a new chat would be created unassigned", got)
	}
}

// AND THE NON-PROJECT SCOPES TIE NOTHING. "All projects" is a way to look at the list and "No project" is the
// absence of a workspace; neither is a project to file a chat under.
func TestTheNonProjectScopesTieNothing(t *testing.T) {
	m := scopePlane2(t)
	for _, scope := range []string{projectScopeAll, unassignedScope} {
		m.setProjectScope(scope)
		if got := m.activeProjectID(); got != "" {
			t.Errorf("activeProjectID() = %q in scope %q, want empty", got, scope)
		}
	}
}

// A STALE SCOPE TIES NOTHING EITHER, rather than failing the create: the server REJECTS an unknown project id,
// so forwarding one would make the send fail outright instead of filing the chat as unassigned.
func TestAStaleScopeDoesNotBreakTheCreate(t *testing.T) {
	m := scopePlane2(t)
	m.projectScope = "p-gone" // selected, then the project vanished from the list
	m.projectScopeChosen = true
	if got := m.activeProjectID(); got != "" {
		t.Errorf("activeProjectID() = %q for a scope the rail does not know; the create would be rejected",
			got)
	}
}

// THE RAIL TITLE NAMES THE SELECTED PROJECT — the operator's "in the conversations list rail at the top, it
// should say the project name that is currently selected".
func TestTheRailTitleNamesTheSelectedProject(t *testing.T) {
	m := scopePlane2(t)
	m.setProjectScope("p-orch")
	title := ""
	for _, l := range strings.Split(m.rightRailView(), "\n") {
		if strings.Contains(l, "Conversations") {
			title = l
			break
		}
	}
	if !strings.Contains(title, "Orchicon") {
		t.Errorf("the rail title does not name the selected project: %q", title)
	}
}
