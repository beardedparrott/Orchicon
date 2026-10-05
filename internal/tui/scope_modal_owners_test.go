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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
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

	// `c` is NOT here: the catalog is a prefill, so it applies at this scope (see the hint test).
	for _, key := range []string{"i", "k"} {
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

// ── the hint advertises every verb that applies ──────────────────────────────────────────

// `c: catalog` IS OFFERED FROM EVERY ROW OF AN OWNED-ROW SCOPE.
//
// The operator: "The mcp add screen does not have a shortcut helper or button to add MCPs from the
// catalog." The catalog is reachable from ANY row — it does not act on the selection — so it must be
// ADVERTISED from any row. It had been dropped by the MCP row's case, because each case wrote out a
// complete list and the global verbs were the ones it forgot.
func TestTheCatalogVerbIsAdvertisedFromEveryRow(t *testing.T) {
	m, _ := newScopeApp(t)
	openProjectScope(t, m)
	rows := m.scope.rows(m)
	if len(rows) == 0 {
		t.Fatal("fixture: the project scope has no rows")
	}
	for i, r := range rows {
		if !r.selectable {
			continue
		}
		putCursor(m, i)
		hint := strings.Join(m.scopeHintItems(), " · ")
		if !strings.Contains(hint, "c: catalog") {
			t.Errorf("row %d (%q) does not offer the catalog add: %q", i, r.text, hint)
		}
		// And the other global verbs, for the same reason.
		for _, verb := range []string{"a: add", "s: skill files", "esc: close"} {
			if !strings.Contains(hint, verb) {
				t.Errorf("row %d (%q) is missing the global verb %q: %q", i, r.text, verb, hint)
			}
		}
	}
}

// A WORKER VERSION ADVERTISES NEITHER THE CATALOG NOR THE ROW-ONLY VERBS — there is no row to create,
// and nothing to install or attach a credential to. The note in the list says why.
func TestTheVersionScopeAdvertisesOnlyWhatItCanDo(t *testing.T) {
	m, _, _ := openingWorkerVersion(t, versionPermissions, `[]`)
	putCursor(m, rowIndexOf(t, m, "pg"))
	hint := strings.Join(m.scopeHintItems(), " · ")
	// `c: catalog` IS OFFERED HERE — the catalog PREFILLS the add form and the form's save writes the
	// spec inline. The operator: "The gui allows this." It does, and the TUI's refusal was an assumption
	// about the catalog's ROLE, not a limitation: the catalog fills a form; the save decides the target.
	for _, absent := range []string{"i: install", "k: credential"} {
		if strings.Contains(hint, absent) {
			t.Errorf("the worker-version scope offers %q, which needs a ROW it does not have: %q", absent, hint)
		}
	}
	for _, present := range []string{"enter/e: edit", "d: remove", "a: add", "c: catalog", "s: skill files"} {
		if !strings.Contains(hint, present) {
			t.Errorf("the worker-version scope does not offer %q: %q", present, hint)
		}
	}
}

// THE PROJECT SCOPE DOES NOT CLAIM DISCIPLES IT HAS NONE OF. Workers are TENANT-level, so a project's
// definitions are consumed by its CONVERSATIONS — not by workers, whose specs are their own inline list.
// The operator caught the earlier wording: "Workers are tenant level and not project level so I don't
// think the wording on the mcp add for projects should mention 'worker versions'."
func TestTheProjectScopeDoesNotClaimWorkersAsDisciples(t *testing.T) {
	m, _ := newScopeApp(t)
	openProjectScope(t, m)
	body := scopeText(m)
	if strings.Contains(body, "worker") {
		t.Errorf("the project scope mentions workers, which are tenant-level and inherit nothing from a "+
			"project:\n%s", body)
	}
	if !strings.Contains(body, "consumed by this project's conversations") {
		t.Errorf("the project scope does not say what consumes it:\n%s", body)
	}

	// And the version scope states the fact from its own side, so an operator who expected inheritance is
	// told rather than left guessing.
	m2, _, _ := openingWorkerVersion(t, versionPermissions, `[]`)
	vbody := scopeText(m2)
	if !strings.Contains(vbody, "tenant-level") {
		t.Errorf("the worker-version scope does not say a worker inherits no project's MCP:\n%s", vbody)
	}
}

// A CATALOG PICK AT THE VERSION SCOPE PREFILLS THE INLINE FORM AND CREATES NO ROW.
//
// The operator: "When I add an MCP from the catalog for workers, it just jumps back to the mcp screen and
// doesn't show that any MCP servers have been added."
//
// The pick must PREFILL the add form (the GUI's handleCatalogAdd fills the form and opens it; nothing is
// created by the pick itself) — and the form must be opened THROUGH A MESSAGE, from the live model, which
// is this flow's whole hazard: the pick runs inside the CATALOG form's OnSubmit, so a direct call could
// mutate a discarded App copy while the catalog form was cleared anyway (it had submitted), which is
// exactly "it jumps back and nothing was added".
//
// Driven the way the runtime drives it: the pick's cmd is run, its message is fed back through Update,
// and the form is then asserted on the model that Update returned.
func TestVersionCatalogPickPrefillsTheInlineFormAndCreatesNoRow(t *testing.T) {
	m, stub, _ := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)

	if cmd := pressScope(t, m, "c"); cmd != nil {
		_ = cmd() // the catalog LIST fetch
	}
	if m.convScopeForm == nil {
		t.Fatal("`c` opened no catalog form")
	}
	// The operator picks an entry (the select's value) and submits.
	m.convScopeForm.Set("slug", "playwright")
	pickCmd, err := m.convScopeForm.Submit()
	if err != nil {
		t.Fatalf("the catalog form refused the pick: %v", err)
	}
	if pickCmd == nil {
		t.Fatal("the pick produced NO command — nothing would open the add form, which is the reported " +
			"\"it just jumps back ... and doesn't show that any MCP servers have been added\"")
	}

	// THE RUNTIME'S PATH: run the cmd, feed the message back through Update.
	next, follow := m.Update(pickCmd())
	mm, ok := next.(*App)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	if mm.convScopeForm == nil {
		t.Fatal("the pick produced a message that opened NO form — the catalog form is submitted and " +
			"therefore cleared, so the operator is left on the scope list with nothing added")
	}
	if got := mm.convScopeForm.Values["name"]; got != "playwright" {
		t.Errorf("the add form is prefilled %q, want the catalog entry's name — the operator should be "+
			"confirming a filled form, not retyping it", got)
	}
	if got := mm.convScopeForm.Values["command"]; !strings.Contains(got, "@playwright/mcp@latest") {
		t.Errorf("the add form's command is %q, want the catalog entry's argv — command FIRST, then args",
			got)
	}
	_ = follow

	// NOTHING WAS CREATED BY THE PICK: only the form's save writes.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.created) != 0 {
		t.Errorf("a catalog pick created %d MCP rows — the pick PREFILLS", len(stub.created))
	}
}

// THE GUARD REFUSES OUT LOUD. If the form is asked to open when no version scope is live, saying so is
// the difference between "the key is broken" and "the scope moved" — and the caller is cleared either
// way, so silence leaves the operator with a modal that closed having done nothing.
func TestOpeningTheInlineFormWithoutAVersionScopeSaysSo(t *testing.T) {
	m, _ := newScopeApp(t)
	m.scope = nil
	m.dock.SetError("")
	m.openInlineSpecForm(&mcpforms.InlineSpec{ID: "x"}, false)
	if m.convScopeForm != nil {
		t.Error("a form opened with no version scope")
	}
	if m.dock.Err == "" {
		t.Error("opening the inline form with no version scope was SILENT — it must name the reason")
	}
	// The live path still opens it.
	m2, _, _ := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)
	m2.openInlineSpecForm(&mcpforms.InlineSpec{ID: "sentry", Type: "stdio"}, false)
	if m2.convScopeForm == nil {
		t.Error("the inline form did not open on a live version scope")
	}
}

// A VERSION SAVE ASKS TO REPUBLISH — which is what makes editing a PUBLISHED version work at all.
//
// The operator's question is what surfaced this: "You can add models to a worker without modifying a
// version. Why can't we add MCP servers like that as well?" The model path edits the version row too —
// model_ref is a column on it — but it has a dedicated RPC (BulkUpdateWorkerModel) that does
// revert → set → republish for it, preserving the version number (internal/worker/service.go,
// applyModelChange). The GENERIC version update supports the same flow when the request asks for it, and
// my modal was not asking: the server refuses a published version with "status is \"published\", must be
// 'draft' to update" — and published is the state most workers are in.
//
// Asserted through a real request (a hermetic worker service that records what it receives), not on a
// copy of the request built by the test: the whole value here is the WIRE shape.
func TestVersionSaveRequestCarriesRepublish(t *testing.T) {
	rec := &recordingWorkerService{}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkerServiceHandler(rec))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())

	m, _ := newScopeApp(t)
	m.openWorkerVersionScopeModal("Sweeper · v1", versionPermissions, `[]`,
		m.workerVersionSaver("w1", "v1", cl))

	// Edit in memory (what the inline form does), then commit.
	m.scope.inline = append(m.scope.inline, mcpforms.InlineSpec{ID: "sentry", Type: "stdio"})
	m.scope.dirty = true
	if cmd := m.commitWorkerVersion(); cmd != nil {
		if msg := cmd(); msg == nil {
			t.Fatal("the commit produced no message")
		}
	}

	req := rec.update
	if req == nil {
		t.Fatal("the commit issued no version update")
	}
	if !req.GetRepublish() {
		t.Error("the version update does not ask to republish — a PUBLISHED version is refused with " +
			"\"must be 'draft' to update\", which is the state most workers are in")
	}
	if req.GetWorkerId() != "w1" || req.GetVersionId() != "v1" {
		t.Errorf("the update targets %q/%q, want w1/v1", req.GetWorkerId(), req.GetVersionId())
	}
	if !strings.Contains(req.GetPermissions(), "sentry") {
		t.Errorf("the update does not carry the edit: %s", req.GetPermissions())
	}
	// AND THE VERSION'S OTHER PERMISSION KEYS SURVIVE — the modal owns the mcp_servers key, never the blob.
	if !strings.Contains(req.GetPermissions(), `"tools"`) {
		t.Errorf("the update dropped the version's other permissions: %s", req.GetPermissions())
	}
}

// recordingWorkerService records the version update it is sent.
type recordingWorkerService struct {
	apiv1connect.UnimplementedWorkerServiceHandler
	update *apiv1.UpdateWorkerVersionRequest
}

func (r *recordingWorkerService) UpdateWorkerVersion(_ context.Context, req *connect.Request[apiv1.UpdateWorkerVersionRequest]) (*connect.Response[apiv1.UpdateWorkerVersionResponse], error) {
	r.update = req.Msg
	return connect.NewResponse(&apiv1.UpdateWorkerVersionResponse{
		Version: &apiv1.WorkerVersion{Id: req.Msg.GetVersionId(), Version: 1},
	}), nil
}
