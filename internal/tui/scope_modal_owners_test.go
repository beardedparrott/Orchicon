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
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
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
	m, _, _ := newScopeApp(t)
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
	m, stub, _ := newScopeApp(t)
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
	m, _, _ := newScopeApp(t)
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

// versionSaveRec records what a worker version's own save was handed: the two fields the modal writes,
// and how many times it wrote them (a count, because "committed twice" is a defect a bool would hide).
type versionSaveRec struct {
	perm, skills string
	saves        int
}

// openingWorkerVersion opens the inline-scope modal and records what a save would write.
func openingWorkerVersion(t *testing.T, permissions, skills string) (*App, *stubMCP, *stubSecrets, *versionSaveRec) {
	t.Helper()
	m, stub, sec := newScopeApp(t)
	rec := &versionSaveRec{}
	m.openWorkerVersionScopeModal("Sweeper · v3", permissions, skills, func(p, s string) tea.Cmd {
		rec.perm, rec.skills, rec.saves = p, s, rec.saves+1
		return nil
	})
	if m.scope == nil || m.scope.kind != ownerWorkerVersion {
		t.Fatalf("the worker-version modal did not open (scope=%+v)", m.scope)
	}
	return m, stub, sec, rec
}

// openingWorkerVersionWithStore is the same modal WITH the tenant secrets store read applied — the
// state the real open path is in (OpenWorkerMCPModal reads the store in the one command that fetches
// the version, and `r` re-reads it).
func openingWorkerVersionWithStore(t *testing.T, permissions, skills string) (*App, *stubMCP, *stubSecrets, *versionSaveRec) {
	t.Helper()
	m, stub, sec, rec := openingWorkerVersion(t, permissions, skills)
	applyFetch(t, m, m.loadScopeSecrets())
	if m.scope.secrets == nil {
		t.Fatal("the tenant secrets store read landed nothing — the credential picker would be empty " +
			"for a store that holds secrets")
	}
	return m, stub, sec, rec
}

// submitAndRoute submits the open form and feeds the message it produced back through the ROUTER, the
// way the shell does — which is the point on the credential path: its message arrives while the form
// is still up, so it must be routed above the form guard (see router.go's form-guard note).
func submitAndRoute(t *testing.T, m *App) {
	t.Helper()
	cmd, err := m.convScopeForm.Submit()
	if err != nil {
		t.Fatalf("the credential form refused a complete entry: %v", err)
	}
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	_, after := m.dispatch(msg)
	if after != nil {
		_ = after()
	}
}

const versionPermissions = `{"tools":["read","write"],"mcp_servers":[{"id":"pg","type":"stdio","command":["npx","-y","pg"]}]}`

// THE MODAL LISTS THE VERSION'S INLINE SPECS AND ITS SKILL FILES — the two things that were only
// reachable as raw JSON and an absolute-path list.
func TestWorkerVersionModalListsInlineSpecsAndSkills(t *testing.T) {
	m, _, _, _ := openingWorkerVersion(t, versionPermissions, `["/skills/w.md"]`)

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
	m, stub, _, rec := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)
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
	m, _, _, rec := openingWorkerVersion(t, versionPermissions, `[]`)

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

// `i` IS REFUSED, WITH A REASON: an INSTALL STATUS can only live on a row, and a version's spec has
// none. `k` IS NOT — a credential needs no row, it needs a place in the spec's own config, which is
// what the credential form builds (below).
func TestWorkerVersionRefusesInstallButNotTheCredential(t *testing.T) {
	m, _, _, rec := openingWorkerVersion(t, versionPermissions, `[]`)
	putCursor(m, rowIndexOf(t, m, "pg"))

	m.dock.SetError("")
	pressScope(t, m, "i")
	if m.convScopeForm != nil {
		t.Error("`i` opened a form for a spec with no row — an install status has nowhere to live")
	}
	if m.dock.Err == "" {
		t.Error("`i` was refused silently on a worker version")
	}

	m.dock.SetError("")
	pressScope(t, m, "k")
	if m.convScopeForm == nil {
		t.Error("`k` opened no credential form — a credential is built INTO the spec, so the lack of " +
			"a row is not a reason to refuse it")
	}
	if rec.saves != 0 {
		t.Error("a refused verb still wrote something")
	}
}

// THE SKILLS CONTROL WRITES BACK THROUGH THE VERSION TOO.
func TestWorkerVersionSkillsCommitThroughTheSave(t *testing.T) {
	m, _, _, rec := openingWorkerVersion(t, versionPermissions, `["/old.md"]`)

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

// ── a worker version's CREDENTIAL: selected, and built into the spec ─────────────────────

// versionPermissionsWithEnv is a version whose spec ALREADY carries the env key a GitHub MCP server
// needs, with no value in it — the shape a catalog pick leaves behind (the prefill never writes a
// blank secret) and the shape an operator hand-adds when they intend to reference a stored one.
const versionPermissionsWithEnv = `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio",` +
	`"command":["npx","-y","server-github"],"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":""}}]}`

// flatJSON drops the whitespace MarshalIndent writes, so an assertion can name a key/value pair
// without depending on the encoding's layout. The fixtures hold no value containing a space, so
// squeezing whitespace cannot make two different documents look alike.
func flatJSON(s string) string { return strings.Join(strings.Fields(s), "") }

// optionValues lists a select/picker field's option values, in order.
func optionValues(spec *kit2.FieldSpec) []string {
	if spec == nil {
		return nil
	}
	out := make([]string, 0, len(spec.Options))
	for _, o := range spec.Options {
		out = append(out, o.Value)
	}
	return out
}

// THE CREDENTIAL IS SELECTED FROM THE TENANT STORE AND WRITTEN INTO THE SPEC'S OWN CONFIG.
//
// The operator: "When adding an MCP server for a worker either through catalog or manually, the
// credentials should be able to be selected rather than typed just like it does for projects and
// conversations. I understand that it is inline but that doesn't mean that info can't be built on top
// and passed to the config."
//
// This is that sentence, asserted end to end: the field offers the STORE'S names (selection), the
// spec's own env key is what it points at, the reference lands in the version's permissions, and
// nothing is created in mcp_servers (there is no row to create).
func TestWorkerVersionCredentialIsSelectedAndBuiltIntoTheSpec(t *testing.T) {
	m, stub, _, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))

	pressScope(t, m, "k")
	f := m.convScopeForm
	if f == nil {
		t.Fatal("`k` opened no credential form on an inline spec")
	}
	// SELECTION: the picker's options ARE the store's names — never a value, and never empty for a
	// store that holds secrets (the names are the whole point of the control).
	if got, want := optionValues(f.Spec("secret")), []string{
		"MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN", "SLACK_BOT_TOKEN",
	}; !slices.Equal(got, want) {
		t.Errorf("the secret field offers %v, want the tenant store's names %v", got, want)
	}
	// THE KEY IS THE SPEC'S OWN, offered and preselected because it is the only one — so the common
	// case is one pick, not a decision about a name that has a single answer.
	key := f.Spec("key")
	if key == nil {
		t.Fatal("the credential form has no key field")
	}
	if key.Initial != "GITHUB_PERSONAL_ACCESS_TOKEN" {
		t.Errorf("the key field starts at %q, want the spec's own env key", key.Initial)
	}
	if got := optionValues(key); !slices.Contains(got, "GITHUB_PERSONAL_ACCESS_TOKEN") {
		t.Errorf("the key field does not offer the spec's existing env key: %v", got)
	}

	f.Set("secret", "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN")
	submitAndRoute(t, m)

	if rec.saves != 1 {
		t.Fatalf("the version was saved %d times, want exactly 1", rec.saves)
	}
	if !strings.Contains(flatJSON(rec.perm), `"GITHUB_PERSONAL_ACCESS_TOKEN":"${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}"`) {
		t.Errorf("the committed spec does not reference the stored secret: %s", rec.perm)
	}
	// THE VERSION'S OTHER PERMISSION KEYS SURVIVE (the modal owns mcp_servers, never the blob).
	if !strings.Contains(rec.perm, `"tools"`) {
		t.Errorf("the commit dropped the version's other permissions: %s", rec.perm)
	}
	// NO ROW, AND NO OWNED-ROW CREDENTIAL RPC: this scope's credential is a reference in the spec.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.created) != 0 {
		t.Errorf("attaching a credential created %d MCP rows — a version's specs are INLINE", len(stub.created))
	}
	if len(stub.secrets) != 0 {
		t.Errorf("attaching a credential sent %d SetMCPServerSecret calls, which address a ROW this "+
			"scope does not have", len(stub.secrets))
	}
}

// THE REFERENCE LANDS IN THE MAP THE SPEC'S TRANSPORT READS: headers for streamable HTTP, env for
// stdio. A credential written into the wrong map would be silently never sent.
func TestWorkerVersionCredentialWritesHeadersForAnHTTPspec(t *testing.T) {
	const httpPerms = `{"mcp_servers":[{"id":"remote","type":"http","url":"https://mcp.example/sse",` +
		`"headers":{"Authorization":""}}]}`
	m, _, _, rec := openingWorkerVersionWithStore(t, httpPerms, `[]`)
	putCursor(m, rowIndexOf(t, m, "remote"))

	pressScope(t, m, "k")
	f := m.convScopeForm
	if f == nil {
		t.Fatal("`k` opened no credential form on an http spec")
	}
	if got := optionValues(f.Spec("key")); !slices.Contains(got, "Authorization") {
		t.Errorf("the key field does not offer the spec's existing header: %v", got)
	}
	f.Set("secret", "SLACK_BOT_TOKEN")
	submitAndRoute(t, m)

	if !strings.Contains(flatJSON(rec.perm), `"headers":{"Authorization":"${SLACK_BOT_TOKEN}"}`) {
		t.Errorf("the header was not pointed at the stored secret: %s", rec.perm)
	}
	if !strings.Contains(rec.perm, `"url"`) {
		t.Errorf("the commit dropped the spec's url: %s", rec.perm)
	}
}

// A NAME THE STORE DOES NOT HOLD IS STORED FIRST, AND ONLY THEN IS THE VERSION WRITTEN.
//
// The order is the contract: a version written first would carry a reference nothing resolves, and
// the operator would find out when the worker ran. The store's write is observed to happen while the
// version has NOT been saved yet.
func TestWorkerVersionCredentialStoresANewSecretBeforeTheVersion(t *testing.T) {
	m, _, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	savesWhenStored := -1
	sec.mu.Lock()
	sec.onWrite = func() { savesWhenStored = rec.saves }
	sec.mu.Unlock()

	f := m.convScopeForm
	f.Set("secret", "JIRA_API_TOKEN")
	f.Set("value", "s3cret")
	submitAndRoute(t, m)

	if savesWhenStored != 0 {
		t.Errorf("the version had already been saved %d time(s) when the secret was stored — the "+
			"reference must not reach the version before the secret it points at exists", savesWhenStored)
	}
	sec.mu.Lock()
	defer sec.mu.Unlock()
	if len(sec.created) != 1 || sec.created[0].GetName() != "JIRA_API_TOKEN" ||
		sec.created[0].GetValue() != "s3cret" {
		t.Fatalf("the store received %+v, want the typed name and value", sec.created)
	}
	if rec.saves != 1 {
		t.Fatalf("the version was saved %d times, want 1", rec.saves)
	}
	if !strings.Contains(flatJSON(rec.perm), `"GITHUB_PERSONAL_ACCESS_TOKEN":"${JIRA_API_TOKEN}"`) {
		t.Errorf("the spec does not reference the secret just stored: %s", rec.perm)
	}
}

// A NAME THAT IS NOT STORED, WITH NO VALUE, IS REFUSED BEFORE ANYTHING IS WRITTEN — because the
// reference could not be resolved when the worker ran, and a form that accepted it would be promising
// something the store cannot keep.
func TestWorkerVersionCredentialRefusesAnUnstoredNameWithoutAValue(t *testing.T) {
	m, stub, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	f := m.convScopeForm
	f.Set("secret", "NOT_A_STORED_SECRET")
	if _, err := f.Submit(); err == nil {
		t.Fatal("the form accepted a secret name that is not in the store, with no value to store it")
	}
	if f.Submitted {
		t.Error("the form closed on a rejected submit — it must stay open so the reason can be read")
	}
	if f.SubmitErr == "" {
		t.Error("the form rejected the credential without saying why")
	}
	sec.mu.Lock()
	created, updated := len(sec.created), len(sec.updated)
	sec.mu.Unlock()
	stub.mu.Lock()
	rows := len(stub.created)
	stub.mu.Unlock()
	if created+updated+rows != 0 || rec.saves != 0 {
		t.Errorf("a refused credential wrote something (store creates=%d updates=%d, rows=%d, version "+
			"saves=%d)", created, updated, rows, rec.saves)
	}
	if got := m.scope.inline[0].Env["GITHUB_PERSONAL_ACCESS_TOKEN"]; got != "" {
		t.Errorf("the refused credential still changed the spec's env: %q", got)
	}
}

// A STORE THAT REJECTS THE WRITE LEAVES THE SPEC AND THE VERSION EXACTLY AS THEY WERE. This is the
// failure the ordering exists for: the operator sees the store's own message, and the version does not
// end up referencing a secret that is not there.
func TestWorkerVersionCredentialStoreFailureLeavesTheSpecAlone(t *testing.T) {
	m, _, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	sec.mu.Lock()
	sec.refuse = errors.New("invalid secret name \"my_token\": must match ^[A-Z][A-Z0-9_]+$")
	sec.mu.Unlock()

	f := m.convScopeForm
	m.dock.SetError("")
	f.Set("secret", "my_token")
	f.Set("value", "s3cret")
	submitAndRoute(t, m)

	if rec.saves != 0 {
		t.Errorf("the version was saved %d time(s) after the store refused the credential — it now "+
			"references a secret nothing resolves", rec.saves)
	}
	if m.dock.Err == "" {
		t.Error("a rejected credential was accepted silently")
	}
	if got := m.scope.inline[0].Env["GITHUB_PERSONAL_ACCESS_TOKEN"]; got != "" {
		t.Errorf("the spec was changed by a store write that failed: %q", got)
	}
}

// AN EXISTING SECRET IS REPLACED IN PLACE (by its ID), which is how a rotated token is stored from the
// scope that references it.
func TestWorkerVersionCredentialReplacesAnExistingSecret(t *testing.T) {
	m, _, sec, _ := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	f := m.convScopeForm
	f.Set("secret", "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN")
	f.Set("value", "ghp_rotated")
	submitAndRoute(t, m)

	sec.mu.Lock()
	defer sec.mu.Unlock()
	if len(sec.created) != 0 {
		t.Errorf("replacing a stored secret created %d rows, want an UPDATE of the existing one", len(sec.created))
	}
	if len(sec.updated) != 1 || sec.updated[0].GetId() != "sec-gh" ||
		sec.updated[0].GetValue() != "ghp_rotated" {
		t.Fatalf("the store received %+v, want an update of sec-gh", sec.updated)
	}
}

// `r` RE-READS THE STORE, because the picker's list is as live as the store: a secret the operator has
// just created in the Control screen must be selectable here without reopening the modal.
func TestWorkerVersionScopeRefreshRereadsTheStore(t *testing.T) {
	m, _, sec, _ := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))

	sec.mu.Lock()
	sec.rows = append(sec.rows, &apiv1.TenantSecret{Id: "sec-new", Name: "NEWLY_STORED_TOKEN"})
	sec.mu.Unlock()

	cmd := pressScope(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` fetched nothing at the worker-version scope — the credential picker cannot be " +
			"refreshed")
	}
	applyFetch(t, m, cmd)
	if !slices.Contains(secretChoiceNames(m.scope.secrets), "NEWLY_STORED_TOKEN") {
		t.Errorf("`r` did not re-read the store: %v", secretChoiceNames(m.scope.secrets))
	}

	pressScope(t, m, "k")
	if got := optionValues(m.convScopeForm.Spec("secret")); !slices.Contains(got, "NEWLY_STORED_TOKEN") {
		t.Errorf("the re-read names are not what the form offers: %v", got)
	}
}

// THE OPEN PATH READS THE STORE IN THE ONE FETCH IT ALREADY MAKES, so the picker is POPULATED when the
// modal appears rather than after a second round trip — and so the wiring that carries the names from
// the fetch onto the modal cannot come loose unnoticed (it is two assignments, which is exactly the
// kind of thing that silently stops happening).
func TestOpeningAWorkerVersionCarriesTheStoresNames(t *testing.T) {
	m, _, _ := newScopeApp(t)
	cmd := m.OpenWorkerMCPModal("w1", "Sweeper")
	if cmd == nil {
		t.Fatal("the open produced no fetch")
	}
	msg, ok := cmd().(workerVersionScopeMsg)
	if !ok {
		t.Fatalf("the open produced %#v, want a workerVersionScopeMsg", msg)
	}
	if msg.err != "" {
		t.Fatalf("the fetch failed: %s", msg.err)
	}
	m.dispatch(msg)
	if m.scope == nil || m.scope.kind != ownerWorkerVersion {
		t.Fatalf("the modal did not open on the version (scope=%+v)", m.scope)
	}
	if got := secretChoiceNames(m.scope.secrets); !slices.Contains(got, "SLACK_BOT_TOKEN") {
		t.Errorf("the open did not carry the store's names onto the modal: %v", got)
	}
	// AND THE FORM IS READY TO PICK FROM THEM, on the version's own spec.
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")
	if m.convScopeForm == nil {
		t.Fatal("`k` opened no credential form after the real open path")
	}
	if got := optionValues(m.convScopeForm.Spec("secret")); !slices.Contains(got, "SLACK_BOT_TOKEN") {
		t.Errorf("the form does not offer the names the open read: %v", got)
	}
}

// THE CATALOG CASE: a spec with NO env key yet — the catalog prefill deliberately leaves a secret key
// out (it never writes a blank one), so the KEY is typed while the SECRET is still picked. The
// reference lands the same way, and a key the transport could not hand to a child process is refused
// before anything is written (nothing else validates an inline spec's keys — see mcpforms.envKeyRE).
func TestWorkerVersionCredentialHandlesASpecWithNoKeysYet(t *testing.T) {
	const freshCatalogSpec = `{"mcp_servers":[{"id":"github","type":"stdio",` +
		`"command":["npx","-y","@modelcontextprotocol/server-github"]}]}`
	m, _, _, rec := openingWorkerVersionWithStore(t, freshCatalogSpec, `[]`)
	putCursor(m, rowIndexOf(t, m, "github"))

	pressScope(t, m, "k")
	f := m.convScopeForm
	if f == nil {
		t.Fatal("`k` opened no credential form on a spec with no env key")
	}
	if got := optionValues(f.Spec("key")); len(got) != 0 {
		t.Errorf("the key field offers %v — its options are the spec's OWN keys, and this spec has "+
			"none yet", got)
	}

	f.Set("key", "NOT A KEY")
	f.Set("secret", "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN")
	if _, err := f.Submit(); err == nil {
		t.Error("a key that no environment can carry was accepted — the spec's keys reach a child " +
			"process as k=v, so this is the only gate on the inline path")
	}
	if rec.saves != 0 {
		t.Error("the rejected key still saved the version")
	}

	// The typed key is the answer for this spec: put a real one in and it lands.
	f.Set("key", "GITHUB_PERSONAL_ACCESS_TOKEN")
	submitAndRoute(t, m)
	if rec.saves != 1 {
		t.Fatalf("the version was saved %d times, want 1", rec.saves)
	}
	if !strings.Contains(flatJSON(rec.perm), `"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}"}`) {
		t.Errorf("the typed key was not pointed at the picked secret: %s", rec.perm)
	}
}

// secretChoiceNames lists the choices' names, for assertions.
func secretChoiceNames(choices []mcpforms.SecretChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.Name)
	}
	return out
}

// ── one modal, three owners: the surface must not confuse them ───────────────────────────

// THE TITLE NAMES THE OWNER, so a destructive verb can never be aimed at the wrong scope by mistake.
func TestTheModalTitleNamesTheOwner(t *testing.T) {
	m, _, _ := newScopeApp(t)

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
	m, _, _ := newScopeApp(t)
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
	m, _, _ := newScopeApp(t)
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

// A WORKER VERSION ADVERTISES WHAT IT CAN DO AND NOT WHAT IT CANNOT.
//
// `i: install` IS ABSENT, because an install status can only live on a row this scope does not have.
// `c: catalog` and `k: credential` ARE PRESENT, and both were once refused here for the same wrong
// reason — that a version's specs are INLINE. Inline decides WHERE a thing is written (the version's
// own permissions, the spec's own env), not whether it can be done: the catalog fills the add form,
// and a credential is built into the spec's config.
func TestTheVersionScopeAdvertisesOnlyWhatItCanDo(t *testing.T) {
	m, _, _, _ := openingWorkerVersion(t, versionPermissions, `[]`)
	putCursor(m, rowIndexOf(t, m, "pg"))
	hint := strings.Join(m.scopeHintItems(), " · ")
	if strings.Contains(hint, "i: install") {
		t.Errorf("the worker-version scope offers %q, which needs a ROW it does not have: %q",
			"i: install", hint)
	}
	for _, present := range []string{
		"enter/e: edit", "d: remove", "k: credential", "a: add", "c: catalog", "s: skill files",
	} {
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
	m, _, _ := newScopeApp(t)
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
	m2, _, _, _ := openingWorkerVersion(t, versionPermissions, `[]`)
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
	m, stub, _, _ := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)

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
	m, _, _ := newScopeApp(t)
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
	m2, _, _, _ := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)
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

	m, _, _ := newScopeApp(t)
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
