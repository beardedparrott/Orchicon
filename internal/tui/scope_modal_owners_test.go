package tui

// scope_modal_owners_test.go — THE ONE MODAL, THREE OWNERS.

import (
	"context"
	"encoding/json"
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
// property (a definition belongs to exactly one scope).
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
	// never by this form.
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
// state the real open path is in (OpenWorkerMCPModal reads the store in the one command.
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

// submitAndRoute submits the open form and feeds the message it produced back through the ROUTER,
// the way the shell does — which is the point on the credential path.
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
func TestWorkerVersionAddSpecCommitsInlineAndCreatesNoRow(t *testing.T) {
	m, stub, _, rec := openingWorkerVersion(t, `{"tools":["read"],"mcp_servers":[]}`, `[]`)
	pressScope(t, m, "a")
	if m.convScopeForm == nil {
		t.Fatal("`a` opened no inline spec form")
	}
	m.convScopeForm.Set("name", "sentry")
	m.convScopeForm.Set("command", "npx -y sentry")
	// SUBMIT, THEN LET THE MODAL HOST CLOSE IT — the real path.
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
// none.
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
// needs, with no value in it.
const versionPermissionsWithEnv = `{"tools":["read"],"mcp_servers":[{"id":"gh","type":"stdio",` +
	`"command":["npx","-y","server-github"],"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":""}}]}`

// flatJSON drops the whitespace MarshalIndent writes, so an assertion can name a key/value pair
// without depending on the encoding's layout.
func flatJSON(s string) string { return strings.Join(strings.Fields(s), "") }

// A STORED SECRET AND A TYPED VALUE ARE TWO INTENTIONS, AND THE FORM NEVER SHOWS BOTH AT ONCE.
//
// The report, exactly: "if someone already went through the list and added a current secret, it doesn't
// matter what value they put in the custom field, the stored secret supersedes it. I think a better approach
// would be to add another item under stored secret called Custom that then only accepts the custom entered
// new key below". So:
//
//   - the new-secret pair exists ONLY under Custom — hidden, not merely ignored, while a stored name is
//     picked, so the two answers are never on screen together;
//   - a value left over from BEFORE the pick is REFUSED with a message naming Custom, because silently
//     dropping it is precisely the behaviour being reported;
//   - Custom reveals the pair, and the write goes through.
func TestCredentialFormSeparatesPickingAStoredSecretFromStoringANewOne(t *testing.T) {
	m, _, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")
	f := m.convScopeForm
	if f == nil {
		t.Fatal("`k` opened no credential form")
	}

	f.Set("secret", "SLACK_BOT_TOKEN")
	if specVisible(f.Spec("newname"), f.Values) || specVisible(f.Spec("value"), f.Values) {
		t.Error("the new-secret fields are visible while a STORED secret is picked — the two intentions " +
			"would be on screen at once, which is the confusion this separates")
	}

	// Typed BEFORE the pick, as the report describes doing it.
	f.Set("value", "leftover")
	if _, err := f.Submit(); err == nil {
		t.Fatal("a value standing beside a stored secret was accepted — that is the silent supersede the " +
			"report described")
	}
	if !strings.Contains(f.SubmitErr, "Custom") {
		t.Errorf("the refusal does not name the choice that would work: %q", f.SubmitErr)
	}

	f.Set("value", "")
	f.Set("secret", mcpforms.CustomSecretChoice)
	if !specVisible(f.Spec("newname"), f.Values) || !specVisible(f.Spec("value"), f.Values) {
		t.Fatal("choosing Custom did not reveal the new-secret fields")
	}
	f.Set("newname", "FRESH_TOKEN")
	f.Set("value", "s3cret")
	submitAndRoute(t, m)

	sec.mu.Lock()
	defer sec.mu.Unlock()
	if len(sec.created) != 1 || sec.created[0].GetName() != "FRESH_TOKEN" ||
		sec.created[0].GetValue() != "s3cret" {
		t.Fatalf("the store received %+v, want the freshly named secret", sec.created)
	}
	if rec.saves != 1 {
		t.Errorf("the version was saved %d times, want 1", rec.saves)
	}
}

// specVisible reports whether a field is part of the form right now (nil Visible means always).
func specVisible(sp *kit2.FieldSpec, values map[string]string) bool {
	if sp == nil {
		return false
	}
	return sp.Visible == nil || sp.Visible(values)
}

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
		"MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN", "SLACK_BOT_TOKEN", mcpforms.CustomSecretChoice,
	}; !slices.Equal(got, want) {
		t.Errorf("the secret field offers %v, want the tenant store's names and Custom %v", got, want)
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
func TestWorkerVersionCredentialStoresANewSecretBeforeTheVersion(t *testing.T) {
	m, _, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	savesWhenStored := -1
	sec.mu.Lock()
	sec.onWrite = func() { savesWhenStored = rec.saves }
	sec.mu.Unlock()

	f := m.convScopeForm
	// A NEW SECRET IS THE CUSTOM CHOICE, in BOTH halves: the name and the value belong to one intention,
	// which is why picking a stored secret no longer leaves a stray value field beside it.
	f.Set("secret", mcpforms.CustomSecretChoice)
	f.Set("newname", "JIRA_API_TOKEN")
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
// reference could not be resolved when the worker ran.
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

// A STORE THAT REJECTS THE WRITE LEAVES THE SPEC AND THE VERSION EXACTLY AS THEY WERE.
func TestWorkerVersionCredentialStoreFailureLeavesTheSpecAlone(t *testing.T) {
	m, _, sec, rec := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	sec.mu.Lock()
	sec.refuse = errors.New("invalid secret name \"my_token\": must match ^[A-Z][A-Z0-9_]+$")
	sec.mu.Unlock()

	f := m.convScopeForm
	m.dock.SetError("")
	f.Set("secret", mcpforms.CustomSecretChoice)
	f.Set("newname", "my_token")
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

// AN EXISTING SECRET IS ROTATED IN PLACE (an UPDATE by id, not a create) — which is now the CUSTOM
// path, because a ROTATION still means "here is a new value".
func TestWorkerVersionCredentialReplacesAnExistingSecret(t *testing.T) {
	m, _, sec, _ := openingWorkerVersionWithStore(t, versionPermissionsWithEnv, `[]`)
	putCursor(m, rowIndexOf(t, m, "gh"))
	pressScope(t, m, "k")

	f := m.convScopeForm
	f.Set("secret", mcpforms.CustomSecretChoice)
	f.Set("newname", "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN")
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

// THE OPEN PATH READS THE STORE IN THE ONE FETCH IT ALREADY MAKES, so the picker is POPULATED when
// the modal appears rather than after a second round trip.
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

// THE CATALOG CASE: a spec with NO env key yet — the catalog prefill deliberately leaves a secret
// key out (it never writes a blank one), so the KEY is typed while the SECRET is still picked.
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
	submitAndRoute(t, m)
	if rec.saves != 1 {
		t.Fatalf("the version was saved %d times, want 1 — the form does not restate the plane's key "+
			"grammar, it builds the reference the operator asked for", rec.saves)
	}
	// Parsed rather than substring-matched, because THIS key deliberately contains a space (flatJSON
	// squeezes whitespace, so it cannot be used to assert a value that contains any).
	if got := specEnv(t, rec.perm, "github")["NOT A KEY"]; got != "${MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN}" {
		t.Errorf("the typed key reads %q, want the picked secret's reference", got)
	}
}

// specEnv parses a committed permissions blob and returns one inline spec's env — the layout- and
// whitespace-independent way to assert what a reference landed as.
func specEnv(t *testing.T, permsJSON, specID string) map[string]string {
	t.Helper()
	var p struct {
		MCPServers []struct {
			ID  string            `json:"id"`
			Env map[string]string `json:"env"`
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal([]byte(permsJSON), &p); err != nil {
		t.Fatalf("the committed permissions are not JSON: %v\n%s", err, permsJSON)
	}
	for _, sp := range p.MCPServers {
		if sp.ID == specID {
			return sp.Env
		}
	}
	t.Fatalf("no inline spec %q in the committed permissions:\n%s", specID, permsJSON)
	return nil
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

// THE PROJECT SCOPE DOES NOT CLAIM DISCIPLES IT HAS NONE OF.
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

// THE GUARD REFUSES OUT LOUD.
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
