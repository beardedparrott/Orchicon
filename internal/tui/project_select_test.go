package tui

// project_select_test.go — THE TWO GAPS THE OPERATOR FOUND IN THE LAUNCH SCOPE, and the one that was never
// implemented at all.
//
// Already working before this file: launching orch inside a directory covered by a project made that project
// the workspace (railprojects.go applyLaunchDirScope), pinned by project_launch_scope_test.go. The operator's
// "I thought this was supposed to already be implemented" was right about that half.
//
// What was NOT working:
//
//  1. CREATING a project from the launch prompt did not make it the workspace. The prompt exists because the
//     operator wants to work in this directory; it created and activated the project, then dropped the scope
//     back where it was, so the first conversation they sent landed unassigned.
//  2. Nothing set the scope from a NAME, which is what both the launch directory and the create form have in
//     hand before any project id exists — and what "/project <name> on launch" would have needed.
//
// And the Work screen never filtered its WORK ITEMS pane by project at all: ListWorkItemsRequest carries
// project_id, the screen simply never set it. Those assertions live in the work package, where a plane exists
// to record what the fetch actually asks for — see screens/work/projectfilter_test.go.

import "testing"

// A CREATED PROJECT BECOMES THE WORKSPACE — the operator's first gap.
func TestACreatedProjectBecomesTheWorkspace(t *testing.T) {
	m := scopePlane2(t)
	// The rail's snapshot predates the create, exactly as it does in a real session.
	if m.activeProjectID() != "" {
		t.Fatalf("fixture: the scope already names a project (%q)", m.activeProjectID())
	}
	cmd := m.applyCreatedProjectScope(launchCreatedMsg{projectID: "p-new", name: "brand-new"})
	if cmd == nil {
		t.Fatal("applyCreatedProjectScope returned no refresh — the rail's list never learns about the new " +
			"project, so the parked selection is never applied")
	}
	// THE RELOAD IS WHAT APPLIES IT: the list did not contain the project when the create returned.
	m.onRailProjects(railProjectsMsg{Projects: append(m.railProjects,
		railProject{ID: "p-new", Name: "brand-new", Status: "active", Dir: "/home/me/brand-new"})})
	if got := m.activeProjectID(); got != "p-new" {
		t.Errorf("activeProjectID() = %q after creating %q, want p-new — the first conversation would be "+
			"created unassigned despite the operator having just named the workspace", got, "brand-new")
	}
}

// A PARKED SELECTION IS CONSUMED ONCE, MATCHED OR NOT.
func TestAParkedSelectionIsConsumedEvenWhenItMisses(t *testing.T) {
	m := scopePlane2(t)
	m.pendingProjectName = "no-such-project"
	m.onRailProjects(railProjectsMsg{Projects: m.railProjects})
	if m.pendingProjectName != "" {
		t.Errorf("the parked selection %q survived a miss; it would re-fire on every later reload",
			m.pendingProjectName)
	}
	if m.projectScope != projectScopeAll {
		t.Errorf("scope = %q after a missed selection, want unchanged All projects", m.projectScope)
	}
}

// AND A PARKED SELECTION THAT MATCHES IS APPLIED, which is the whole path a create relies on.
func TestAParkedSelectionIsAppliedWhenTheListLands(t *testing.T) {
	m := scopePlane2(t)
	m.pendingProjectName = "ai-tools"
	m.onRailProjects(railProjectsMsg{Projects: m.railProjects})
	if got := m.activeProjectID(); got != "p-tools" {
		t.Errorf("activeProjectID() = %q after a parked selection of ai-tools, want p-tools", got)
	}
}

// A SELECTION BY NAME MATCHES CASE-INSENSITIVELY, because the create form lets a person type whatever case
// they like and "orchicon" for a project called "Orchicon" is not an error.
func TestSelectionByNameIsCaseInsensitive(t *testing.T) {
	m := scopePlane2(t)
	if !m.selectProjectByName("orchICON") {
		t.Fatal("selectProjectByName(\"orchICON\") found nothing, want p-orch")
	}
	if m.projectScope != "p-orch" {
		t.Errorf("scope = %q, want p-orch", m.projectScope)
	}
}

// AN AMBIGUOUS NAME CHANGES NOTHING. Two projects containing the text is not a decision, and guessing would put
// the operator in a workspace they did not choose — which is worse than leaving it alone.
func TestAnAmbiguousNameIsRefused(t *testing.T) {
	m := scopePlane2(t)
	m.railProjects = append(m.railProjects,
		railProject{ID: "p-orch2", Name: "Orchicon Two", Status: "active", Dir: "/home/me/o2"})
	before := m.projectScope
	// "orch" contains-matches both "Orchicon" and "Orchicon Two"; neither is an exact match.
	if m.selectProjectByName("orch") {
		t.Fatalf("an ambiguous name selected %q; it must refuse rather than guess", m.projectScope)
	}
	if m.projectScope != before {
		t.Errorf("scope moved from %q to %q on an ambiguous match", before, m.projectScope)
	}
}

// AN EXACT MATCH WINS OVER A CONTAINING ONE — "Orchicon" must not land in "Orchicon Two" because it came first.
func TestAnExactNameBeatsAContainingOne(t *testing.T) {
	m := scopePlane2(t)
	m.railProjects = []railProject{
		{ID: "p-orch2", Name: "Orchicon Two", Status: "active", Dir: "/home/me/o2"},
		{ID: "p-orch", Name: "Orchicon", Status: "active", Dir: "/home/me/projects/Orchicon"},
	}
	// "Orch" contains both; "Orchicon" is exact on p-orch only.
	if !m.selectProjectByName("Orchicon") {
		t.Fatal("selectProjectByName found nothing for an exact name")
	}
	if m.projectScope != "p-orch" {
		t.Errorf("scope = %q, want p-orch (the exact match)", m.projectScope)
	}
}
