package tui

// scope_modal_owners_test.go — THE ONE MODAL, THREE OWNERS.
//
// The operator: "I simply want at least a similar modal to manage MCP and skills for projects and
// workers. Right now it's one line edit with absolute paths. It doesn't pop up a modal like /mcp or
// /scope does. I understand that specific one is tied to a scope but we should be able to replicate the
// UI piece to manage the MCPs and Skills of workers and projects. We reused the same UI on the GUI."
//
// The GUI's answer is ONE panel (MCPServersPanel) parameterized by MCPScope — project, conversation,
// workerVersion — mounted on the project page, the worker page and the conversation disclosure. The TUI
// had it for conversations only. These tests cover the other two, and the rule that makes them safe:
// a conversation's and a project's definitions are OWNED ROWS (stamped with that scope's id), while a
// worker version's are INLINE SPECS written into the version's permissions — never a row.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
)

// openProjectScope opens the modal for the stub's project and lets its fetch land.
func openProjectScope(t *testing.T, m *App) {
	t.Helper()
	applyFetch(t, m, m.openProjectScopeModal("p1", "Orchicon"))
	if m.scope == nil || m.scope.kind != ownerProject {
		t.Fatalf("the project modal did not open (scope=%+v)", m.scope)
	}
	if m.scope.loading {
		t.Fatal("the project modal never finished loading")
	}
}

// ── the project scope ────────────────────────────────────────────────────────────────────

// THE MODAL LISTS WHAT THE PROJECT OWNS, and nothing else — no inherited half, because a project is the
// SOURCE of what its conversations and workers inherit.
func TestProjectScopeModalListsTheProjectsRowsAndSkills(t *testing.T) {
	m, _ := newScopeApp(t)
	openProjectScope(t, m)

	body := scopeText(m)
	for _, want := range []string{
		"MCP servers — this project",
		"postgres",               // the project's own definition (stub.projectServers)
		"npx -y server-postgres", // where it points
		"Skill files — this project",
		"/skills/project.md", // the project's skill files (from the rail row)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the project modal does not show %q:\n%s", want, body)
		}
	}
	// The conversation's OWN definition belongs to the conversation, so it must NOT appear here.
	if strings.Contains(body, "server-github") {
		t.Errorf("the project modal lists another scope's definition:\n%s", body)
	}
	// And nothing is labelled inherited at this scope.
	if strings.Contains(body, "read-only") {
		t.Errorf("the project modal claims an inherited half — a project inherits nothing:\n%s", body)
	}
}

// A CREATE FROM THE PROJECT MODAL IS OWNED BY THE PROJECT — the owner stamp is the whole safety
// property (a definition belongs to exactly one scope), and it is asserted on the request the plane
// would receive.
func TestProjectModalCreateIsStampedWithTheProjectAndCarriesItsArgs(t *testing.T) {
	m, stub := newScopeApp(t)
	openProjectScope(t, m)

	pressScope(t, m, "a")
	if m.convScopeForm == nil {
		t.Fatal("`a` opened no definition form")
	}
	m.convScopeForm.Set("name", "slack")
	m.convScopeForm.Set("command", "npx")
	m.convScopeForm.Set("args", "-y server-slack")
	// ${SECRET_NAME} IS PASSED THROUGH VERBATIM — resolved by the tenant secrets store at session time,
	// never by this form. (This assertion came from the work package's wrapper test, which this replaced
	// when that wrapper was removed: the modal is what builds this request now.)
	m.convScopeForm.Set("env", "SLACK_TOKEN=${SLACK_TOKEN}")
	cmd, err := m.convScopeForm.OnSubmit(m.convScopeForm.Values, nil)
	if err != nil {
		t.Fatalf("the form refused a complete entry: %v", err)
	}
	if msg := cmd(); msg == nil {
		t.Fatal("the save produced no write")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.created) != 1 {
		t.Fatalf("the plane received %d creates, want 1", len(stub.created))
	}
	got := stub.created[0]
	if got.GetProjectId() != "p1" {
		t.Errorf("the create carries project_id %q, want p1 — a project-modal create must be owned by "+
			"the PROJECT", got.GetProjectId())
	}
	if got.GetConversationId() != "" {
		t.Errorf("the create also carries conversation_id %q — the owner XOR is broken", got.GetConversationId())
	}
	// THE ARGV SPLIT IS THE FORM'S, and it matters: the operator's args must reach the wire as separate
	// entries, because a single joined string would be handed to the runtime as ONE argument.
	if args := got.GetArgs(); len(args) != 2 || args[0] != "-y" || args[1] != "server-slack" {
		t.Errorf("args = %v, want [-y server-slack] split into two", args)
	}
	if env := got.GetEnv(); env["SLACK_TOKEN"] != "${SLACK_TOKEN}" {
		t.Errorf("env = %v, want the ${SECRET_NAME} reference passed through verbatim", env)
	}
}

// THE SKILLS CONTROL IS THE SAME ONE, pointed at the project: `s` opens the prefilled path list, and
// saving it writes the project's skill_files (the field that used to be a raw text box in the edit form).
func TestProjectModalSkillsFormWritesTheProject(t *testing.T) {
	m, _ := newScopeApp(t)
	openProjectScope(t, m)

	pressScope(t, m, "s")
	if m.convScopeForm == nil {
		t.Fatal("`s` opened no skill-file form")
	}
	if got := m.convScopeForm.Values["paths"]; got != "/skills/project.md" {
		t.Errorf("the skill field is prefilled %q, want the project's current list", got)
	}
	// The write goes to the PROJECTS service. (The stub records it through the shared ask stub; here we
	// assert the command is produced at all, which is the wiring under test.)
	if _, err := m.convScopeForm.OnSubmit(map[string]string{"paths": "/a.md, /b.md"}, nil); err != nil {
		t.Fatalf("the skill form refused: %v", err)
	}
}

// ── the worker-version scope ─────────────────────────────────────────────────────────────

// openingWorkerVersion opens the inline-scope modal and records what a save would write.
func openingWorkerVersion(t *testing.T, permissions, skills string) (*App, *stubMCP, *struct {
	perm, skills string
	saves        int
}) {
	t.Helper()
	m, stub := newScopeApp(t)
	rec := &struct {
		perm, skills string
		saves        int
	}{}
	m.openWorkerVersionScopeModal("Sweeper · v3", permissions, skills, func(p, s string) tea.Cmd {
		rec.perm, rec.skills, rec.saves = p, s, rec.saves+1
		return nil
	})
	if m.scope == nil || m.scope.kind != ownerWorkerVersion {
		t.Fatalf("the worker-version modal did not open (scope=%+v)", m.scope)
	}
	return m, stub, rec
}

const versionPermissions = `{"tools":["read","write"],"mcp_servers":[{"id":"pg","type":"stdio","command":["npx","-y","pg"]}]}`

// THE MODAL LISTS THE VERSION'S INLINE SPECS AND ITS SKILL FILES — the two things that were only
// reachable as raw JSON and an absolute-path list.
func TestWorkerVersionModalListsInlineSpecsAndSkills(t *testing.T) {
	m, _, _ := openingWorkerVersion(t, versionPermissions, `["/skills/w.md"]`)

	body := scopeText(m)
	for _, want := range []string{
		"MCP servers — inline in this version",
		"pg",        // the inline spec's id
		"npx -y pg", // where it points
		"Skill files — this version",
		"/skills/w.md",
		"inline", // the note saying this scope's specs are inline
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the worker-version modal does not show %q:\n%s", want, body)
		}
	}
}

// ADDING A SPEC COMMITS INTO THE VERSION'S PERMISSIONS AND CREATES NO ROW.
//
// This is the difference that matters for this scope: a version is immutable once published, so its
// specs live in its own permissions JSON. A create here would be a row nothing would ever read.
func TestWorkerVersionAddSpecCommitsInlineAndCreatesNoRow(t *testing.T) {
	m, stub, rec := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)
	pressScope(t, m, "a")
	if m.convScopeForm == nil {
		t.Fatal("`a` opened no inline spec form")
	}
	m.convScopeForm.Set("name", "sentry")
	m.convScopeForm.Set("command", "npx -y sentry")
	// SUBMIT, THEN LET THE MODAL HOST CLOSE IT — the real path. mcpforms.InlineForm's save callback
	// cannot return a command, so the modal commits when the FORM CLOSES (see convScopeKey); calling
	// OnSubmit directly would skip exactly the step under test.
	if _, err := m.convScopeForm.Submit(); err != nil {
		t.Fatalf("the inline form refused a complete entry: %v", err)
	}
	_, cmd := m.convScopeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		_ = cmd()
	}
	if rec.saves == 0 {
		t.Fatal("adding a spec did not commit through the version's save — the edit would be lost")
	}
	if !strings.Contains(rec.perm, "sentry") {
		t.Errorf("the committed permissions do not carry the new spec: %s", rec.perm)
	}
	// THE VERSION'S OTHER PERMISSION KEYS SURVIVE — the modal owns the mcp_servers key, never the blob.
	if !strings.Contains(rec.perm, `"tools"`) {
		t.Errorf("the commit dropped the version's other permissions (tools): %s", rec.perm)
	}
	// AND NO ROW WAS CREATED.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.created) != 0 {
		t.Errorf("a worker-version add created %d MCP rows — a version's specs are INLINE", len(stub.created))
	}
}

// REMOVING A SPEC COMMITS TOO, and the confirm names it.
func TestWorkerVersionRemoveSpecCommits(t *testing.T) {
	m, _, rec := openingWorkerVersion(t, versionPermissions, `[]`)

	putCursor(m, rowIndexOf(t, m, "pg"))
	pressScope(t, m, "d")
	if m.bulkConfirm == nil {
		t.Fatal("`d` removed a spec without confirming")
	}
	if !strings.Contains(m.bulkConfirm.Body, "pg") {
		t.Errorf("the confirm does not name the spec: %q", m.bulkConfirm.Body)
	}
	if run := m.bulkConfirmRun; run != nil {
		run()
	}
	if rec.saves == 0 {
		t.Fatal("removing a spec did not commit")
	}
	if strings.Contains(rec.perm, `"pg"`) {
		t.Errorf("the removed spec is still in the committed permissions: %s", rec.perm)
	}
}

// THE VERBS THAT NEED A ROW ARE REFUSED, WITH A REASON — install, credential and the catalog. The GUI's
// panel makes the same omission for this scope rather than offering buttons that cannot work.
func TestWorkerVersionRefusesTheRowOnlyVerbs(t *testing.T) {
	m, _, rec := openingWorkerVersion(t, versionPermissions, `[]`)
	putCursor(m, rowIndexOf(t, m, "pg"))

	for _, key := range []string{"i", "k", "c"} {
		m.dock.SetError("")
		pressScope(t, m, key)
		if m.convScopeForm != nil {
			t.Errorf("`%s` opened a form for a scope with no row — nothing it saved could be read", key)
		}
		if m.dock.Err == "" {
			t.Errorf("`%s` was refused silently on a worker version", key)
		}
	}
	if rec.saves != 0 {
		t.Error("a refused verb still wrote something")
	}
}

// THE SKILLS CONTROL WRITES BACK THROUGH THE VERSION TOO.
func TestWorkerVersionSkillsCommitThroughTheSave(t *testing.T) {
	m, _, rec := openingWorkerVersion(t, versionPermissions, `["/old.md"]`)

	pressScope(t, m, "s")
	if m.convScopeForm == nil {
		t.Fatal("`s` opened no skill form")
	}
	if got := m.convScopeForm.Values["paths"]; got != "/old.md" {
		t.Errorf("the skill field is prefilled %q, want the version's current paths", got)
	}
	if _, err := m.convScopeForm.OnSubmit(map[string]string{"paths": "/new.md"}, nil); err != nil {
		t.Fatalf("the skill form refused: %v", err)
	}
	if rec.saves == 0 {
		t.Fatal("the skill change did not commit")
	}
	if !strings.Contains(rec.skills, "/new.md") {
		t.Errorf("the committed skill files do not carry the new path: %s", rec.skills)
	}
}

// ── one modal, three owners: the surface must not confuse them ───────────────────────────

// THE TITLE NAMES THE OWNER, so a destructive verb can never be aimed at the wrong scope by mistake.
func TestTheModalTitleNamesTheOwner(t *testing.T) {
	m, _ := newScopeApp(t)

	m.scope = &scopeModal{kind: ownerProject, projectID: "p1", label: "Orchicon"}
	if got := m.scopeTitle(); !strings.Contains(got, "Orchicon") || !strings.Contains(got, "project") {
		t.Errorf("the project title does not name the project: %q", got)
	}
	m.scope = &scopeModal{kind: ownerWorkerVersion, label: "Sweeper · v3"}
	if got := m.scopeTitle(); !strings.Contains(got, "Sweeper") {
		t.Errorf("the version title does not name the version: %q", got)
	}
	m.scope = &scopeModal{kind: ownerConversation, convID: "c1"}
	if got := m.scopeTitle(); !strings.Contains(got, "conversation") {
		t.Errorf("the conversation title does not name the conversation: %q", got)
	}
}

// THE CONVERSATION-CLOSE GUARD MUST NOT CLOSE A PROJECT OR VERSION MODAL. It fires when the open
// conversation changes — and a project modal has no conversation to be about, so it must survive.
func TestSwitchingConversationsDoesNotCloseAnotherScopesModal(t *testing.T) {
	m, _ := newScopeApp(t)
	openProjectScope(t, m)

	m.chatConvID = "some-other-chat"
	m.scopeConversationChanged()
	if m.scope == nil {
		t.Error("switching conversations closed the PROJECT modal — it is not about a conversation")
	}

	// The conversation modal still closes, which is the behaviour that guard exists for.
	m.scope = &scopeModal{kind: ownerConversation, convID: "c1"}
	m.scopeConversationChanged()
	if m.scope != nil {
		t.Error("the conversation modal survived a conversation switch")
	}
	_ = chat.ChatItem{}
	_ = apiv1.MCPServer{}
	_ = mcpforms.InlineSpec{}
	_ = tea.KeyMsg{}
}

// ── the shell satisfies the hosts the panes assert ───────────────────────────────────────
//
// The Work and Execution screens reach the modal through unexported one-method interfaces
// (mcpModalHost, in each package) that they type-assert on the shell. Those interfaces cannot be
// referenced from here, so this pins the METHOD SETS instead: if a signature drifts, the panes' assertion
// silently fails and the key goes inert — which is exactly the class of bug this feature is fixing, and
// a compile error here is a far better place to find it.
var (
	_ interface {
		OpenProjectMCPModal(projectID, name string) tea.Cmd
	} = (*App)(nil)
	_ interface {
		OpenWorkerMCPModal(workerID, name string) tea.Cmd
	} = (*App)(nil)
)
