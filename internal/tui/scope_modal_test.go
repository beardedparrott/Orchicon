package tui

// scope_modal_test.go — THE CONVERSATION SCOPE MODAL: /scope, /mcp and /skills open ONE list of this
// conversation's MCP definitions and skill files, with the project's shown read-only, and every verb is
// a key on a visible row.
//
// The operator: "The MCP tools that were added for the TUI is abysmal. There are too many commands and
// the shortcuts go off screen. I think simply type /mcp should pop up a modal that mimics what the gui
// has for scope. Same with /skills. It should pop up the same modal. Or /scope."
//
// So these tests assert three separate claims, and the third is the one that was missing before:
//
//	1. ONE SURFACE — all three names open the same modal, and the palette row fits on screen.
//	2. THE LIST IS VISIBLE — the conversation's definitions and skill files, and the PROJECT's
//	   contributions labelled read-only, are what the modal shows.
//	3. THE WRITES ARE OWNER-SCOPED AND ROW-ADDRESSED — driven from the modal, through a fake plane that
//	   records what was asked of it: a definition created here carries the CONVERSATION's id (never the
//	   project's, never nothing), and delete/install/credential address the row's ID rather than a name
//	   resolved by re-listing.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

// stubMCP is the MCP service the scope modal talks to: it answers the owner-scoped list and records
// every write, so a test can assert WHICH row and WHICH owner a keystroke produced.
type stubMCP struct {
	apiv1connect.UnimplementedMCPServiceHandler

	mu             sync.Mutex
	servers        []*apiv1.MCPServer // the conversation's own
	projectServers []*apiv1.MCPServer // the project's (the read-only half)
	listReqs       []*apiv1.MCPServerListRequest
	created        []*apiv1.MCPServerCreateRequest
	deleted        []string
	installed      []string
	secrets        []*apiv1.MCPServerSetSecretRequest
}

func (s *stubMCP) ListMCPServers(_ context.Context, req *connect.Request[apiv1.MCPServerListRequest]) (*connect.Response[apiv1.MCPServerListResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listReqs = append(s.listReqs, req.Msg)
	// THE FAKE HONOURS THE OWNER FILTER, like the plane does: a fake that returned everything would let
	// a client that forgot to scope its request pass this suite while showing the whole tenant.
	if req.Msg.GetConversationId() != "" {
		return connect.NewResponse(&apiv1.MCPServerListResponse{Servers: s.servers}), nil
	}
	if req.Msg.GetProjectId() != "" {
		return connect.NewResponse(&apiv1.MCPServerListResponse{Servers: s.projectServers}), nil
	}
	return connect.NewResponse(&apiv1.MCPServerListResponse{Servers: append(append([]*apiv1.MCPServer{}, s.servers...), s.projectServers...)}), nil
}

func (s *stubMCP) CreateMCPServer(_ context.Context, req *connect.Request[apiv1.MCPServerCreateRequest]) (*connect.Response[apiv1.MCPServerCreateResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, req.Msg)
	return connect.NewResponse(&apiv1.MCPServerCreateResponse{
		Server: &apiv1.MCPServer{Id: "created-" + req.Msg.GetName(), Name: req.Msg.GetName()},
	}), nil
}

func (s *stubMCP) DeleteMCPServer(_ context.Context, req *connect.Request[apiv1.MCPServerDeleteRequest]) (*connect.Response[apiv1.MCPServerDeleteResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, req.Msg.GetId())
	return connect.NewResponse(&apiv1.MCPServerDeleteResponse{}), nil
}

func (s *stubMCP) InstallMCPRuntime(_ context.Context, req *connect.Request[apiv1.MCPServerInstallRequest]) (*connect.Response[apiv1.MCPServerInstallResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installed = append(s.installed, req.Msg.GetId())
	return connect.NewResponse(&apiv1.MCPServerInstallResponse{}), nil
}

func (s *stubMCP) SetMCPServerSecret(_ context.Context, req *connect.Request[apiv1.MCPServerSetSecretRequest]) (*connect.Response[apiv1.MCPServerSetSecretResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = append(s.secrets, req.Msg)
	return connect.NewResponse(&apiv1.MCPServerSetSecretResponse{}), nil
}

// newScopeApp builds a shell whose MCP client is the stub, with one open conversation (c1) inside a
// project (p1) that owns a definition of its own.
func newScopeApp(t *testing.T) (*App, *stubMCP) {
	t.Helper()
	stub := &stubMCP{
		servers: []*apiv1.MCPServer{{
			Id: "mcp-conv", Name: "github", Enabled: true,
			Transport: apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
			Command:   "npx", Args: []string{"-y", "server-github"},
		}},
		projectServers: []*apiv1.MCPServer{{
			Id: "mcp-proj", Name: "postgres", Enabled: true,
			Transport: apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STDIO,
			Command:   "npx", Args: []string{"-y", "server-postgres"},
		}},
	}
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewMCPServiceHandler(stub))
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(&stubAskParity{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	m := NewApp(client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client()),
		&config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.chatConvID = "c1"
	m.conversations = []chat.Conversation{{
		ID: "c1", Title: "a chat", ProjectID: "p1", SkillFiles: []string{"/skills/conv.md"},
	}}
	m.railProjects = []railProject{{
		ID: "p1", Name: "Orchicon", Status: "active",
		Dir: "/home/x/Orchicon", SkillFiles: []string{"/skills/project.md"},
	}}
	m.railProjectsLoaded = true
	return m, stub
}

// openScopeFrom opens the modal through a slash line, letting the fetch command run so the modal's data
// lands the way it does in the shell.
func openScopeFrom(t *testing.T, m *App, line string) {
	t.Helper()
	cmd := mustSlash(t, m, line)
	if m.scope == nil {
		t.Fatalf("%s did not open the scope modal", line)
	}
	if cmd == nil {
		t.Fatalf("%s opened the modal without fetching anything", line)
	}
	// The shell runs the returned cmds and feeds their messages back in; batching means the fetch may
	// be one of several, so every msg is applied until the modal reports it is loaded.
	applyFetch(t, m, cmd)
	if m.scope.loading {
		t.Fatal("the modal never finished loading")
	}
	if m.scope.err != "" {
		t.Fatalf("the modal's fetch failed: %s", m.scope.err)
	}
}

// applyFetch runs a (possibly batched) command and applies its messages to the shell until none are
// left, which is what the bubbletea runtime does with a returned cmd.
func applyFetch(t *testing.T, m *App, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		switch msg := next().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case scopeDataMsg:
			m.onScopeData(msg)
		case railProjectsMsg:
			m.onRailProjects(msg)
		}
	}
}

// press sends a key to the shell and returns the cmd the modal produced.
func pressScope(t *testing.T, m *App, key string) tea.Cmd {
	t.Helper()
	var k tea.KeyMsg
	switch key {
	case "up", "down", "enter", "esc", "tab":
		k = tea.KeyMsg{Type: map[string]tea.KeyType{
			"up": tea.KeyUp, "down": tea.KeyDown, "enter": tea.KeyEnter,
			"esc": tea.KeyEsc, "tab": tea.KeyTab,
		}[key]}
	default:
		k = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	_, cmd := m.scopeKey(k)
	return cmd
}

// scopeText renders the modal and strips the styling, so a test asserts what is READ rather than the
// escape sequences that draw it.
func scopeText(m *App) string { return stripANSI(m.scopeBody()) }

// rowIndexOf finds a row by its text fragment.
func rowIndexOf(t *testing.T, m *App, fragment string) int {
	t.Helper()
	for i, r := range m.scope.rows(m) {
		if strings.Contains(r.text, fragment) {
			return i
		}
	}
	t.Fatalf("no scope row contains %q:\n%s", fragment, scopeText(m))
	return -1
}

// putCursor parks the cursor on a row (bypassing the movement rules, so a test can drive a SPECIFIC
// row's verb without depending on list order).
func putCursor(m *App, i int) { m.scope.cursor = i }

// --- 1. one surface ---------------------------------------------------------

// ALL THREE NAMES OPEN THE SAME MODAL, and the modal is about the OPEN conversation.
func TestScopeNamesOpenOneModal(t *testing.T) {
	for _, line := range []string{"/scope", "/mcp", "/skills"} {
		t.Run(line, func(t *testing.T) {
			m, _ := newScopeApp(t)
			openScopeFrom(t, m, line)
			if got := m.scopeConversation(); got != "c1" {
				t.Fatalf("%s scoped %q, want the open conversation (c1)", line, got)
			}
		})
	}
}

// THE PALETTE ROW FITS. This is the "the shortcuts go off screen" half of the report, pinned: the old
// /mcp usage string was 76 cells against a 70-cell cap, so the palette had to clip its last verbs.
// EVERY registered command is checked, not just this one — the next long usage string is the same bug.
func TestEverySlashUsageFitsThePalette(t *testing.T) {
	m, _ := newScopeApp(t)
	cap := m.paletteContentWidth()
	if cap < 70 {
		t.Fatalf("fixture: the palette cap is %d, expected the production 70 at this width", cap)
	}
	for _, name := range m.slash.names {
		c := m.slash.byName[name]
		if c == nil || c.Name != name {
			continue // an alias, rendered with its primary
		}
		row := "  " + c.Usage + c.AliasLabel()
		if len([]rune(row)) > cap {
			t.Errorf("%s's palette row is %d cells against a %d-cell cap — its own usage string is "+
				"what gets clipped (row: %q)", name, len([]rune(row)), cap, row)
		}
	}
}

// --- 2. the list is visible -------------------------------------------------

// THE MODAL SHOWS BOTH HALVES OF THE SCOPE: the conversation's OWN definitions (with the facts the GUI's
// card shows) and the PROJECT's, labelled read-only — because the server renders the UNION of the two
// into the turn, and a scope pane that showed only half of it would answer "what runs here?" wrongly.
func TestScopeShowsTheConversationsAndTheProjectsContributions(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	body := scopeText(m)

	for _, want := range []string{
		"github",                               // the conversation's own definition
		"npx -y server-github",                 // where it points
		"postgres",                             // the project's
		"npx -y server-postgres",               // where THAT points
		"Inherited from project " + "Orchicon", // named, so "why is this here?" is answerable
		"read-only",
		"/skills/conv.md",    // the conversation's own skill file
		"/skills/project.md", // the project's
		"Skill files — this conversation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the modal does not show %q:\n%s", want, body)
		}
	}
}

// THE LIST IS OWNER-SCOPED, and both reads carry their owner: the conversation's definitions by
// ConversationId, the project's by ProjectId. An unscoped list would be the tenant-wide read the
// owner-scoped model removed.
func TestScopeListRequestsAreOwnerScoped(t *testing.T) {
	m, stub := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	stub.mu.Lock()
	defer stub.mu.Unlock()
	var sawConv, sawProject bool
	for _, r := range stub.listReqs {
		if r.GetConversationId() == "c1" {
			sawConv = true
		}
		if r.GetProjectId() == "p1" {
			sawProject = true
		}
		if r.GetConversationId() == "" && r.GetProjectId() == "" {
			t.Errorf("the modal made an UNSCOPED list request — that is the tenant-wide read")
		}
	}
	if !sawConv || !sawProject {
		t.Errorf("list requests = %+v, want one per owner (conversation c1, project p1)", stub.listReqs)
	}
}

// A conversation with NO project inherits nothing, so there is no second read to make.
func TestScopeSkipsTheProjectReadWhenUnassigned(t *testing.T) {
	m, stub := newScopeApp(t)
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", SkillFiles: []string{"/a.md"}}}
	openScopeFrom(t, m, "/scope")
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, r := range stub.listReqs {
		if r.GetProjectId() != "" {
			t.Errorf("an unassigned conversation asked for a project's definitions: %+v", r)
		}
	}
}

// --- 3. the writes ----------------------------------------------------------

// `a` OPENS THE OWNED DEFINITION FORM, and saving it creates a CONVERSATION-owned row. This is the
// owner-stamping rule (a definition belongs to exactly one scope) asserted end to end, through the
// request the plane actually receives.
func TestScopeAddDefinesAConversationOwnedServer(t *testing.T) {
	m, stub := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	pressScope(t, m, "a")
	if m.convScopeForm == nil {
		t.Fatal("`a` opened no definition form")
	}
	// The define path is COMPLETE: command/args/env for stdio and url/headers for http, owner-stamped by
	// mcpforms. A form missing any of them would be a capability the old grammar had and the modal lost.
	for _, name := range []string{"name", "transport", "command", "args", "env", "url", "headers"} {
		if !formHasField(m.convScopeForm, name) {
			t.Errorf("the definition form offers no %q field — the define path is incomplete", name)
		}
	}
	// Fill it the way the operator would, then submit.
	m.convScopeForm.Set("name", "slack")
	m.convScopeForm.Set("command", "npx")
	m.convScopeForm.Set("args", "-y server-slack")
	cmd, err := m.convScopeForm.OnSubmit(m.convScopeForm.Values, nil)
	if err != nil {
		t.Fatalf("the definition form refused a complete entry: %v", err)
	}
	// The form's cmd IS the write; run it and inspect what it asked for.
	msg := cmd()
	if sc, ok := msg.(convScopeMsg); !ok || sc.err != "" {
		t.Fatalf("the definition write produced %#v", msg)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.created) != 1 {
		t.Fatalf("the plane received %d creates, want 1", len(stub.created))
	}
	got := stub.created[0]
	if got.GetConversationId() != "c1" {
		t.Errorf("the created definition carries conversation_id %q, want c1 — a definition belongs to "+
			"exactly ONE scope and this one is not tenant- or project-owned", got.GetConversationId())
	}
	if got.GetProjectId() != "" {
		t.Errorf("the created definition also carries project_id %q — an owner XOR is required",
			got.GetProjectId())
	}
	if got.GetName() != "slack" || got.GetCommand() != "npx" {
		t.Errorf("the created definition = %+v, want the typed name/command", got)
	}
}

// `d` on an owned definition raises the shell's CONFIRM (a stray keypress must not delete), and the
// affirmative answer deletes THE ROW'S ID — not a name re-resolved from a re-listing.
func TestScopeDeleteConfirmsThenDeletesTheRow(t *testing.T) {
	m, stub := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	putCursor(m, rowIndexOf(t, m, "github"))
	pressScope(t, m, "d")
	if m.bulkConfirm == nil {
		t.Fatal("`d` deleted without confirming — a stray keypress must not remove a definition")
	}
	if !strings.Contains(m.bulkConfirm.Body, "github") {
		t.Errorf("the confirm does not name what it will delete: %q", m.bulkConfirm.Body)
	}
	// Dismiss first: the confirm is the shell's dialog, and answering it `esc` must leave the
	// definition alone.
	m.bulkConfirmKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.bulkConfirm != nil {
		t.Fatal("esc did not dismiss the confirm")
	}
	stub.mu.Lock()
	if len(stub.deleted) != 0 {
		t.Fatalf("a dismissed confirm deleted %v", stub.deleted)
	}
	stub.mu.Unlock()

	// Now confirm for real: the confirm's affirmative button runs the write.
	openScopeFrom(t, m, "/scope")
	putCursor(m, rowIndexOf(t, m, "github"))
	pressScope(t, m, "d")
	if m.bulkConfirm == nil {
		t.Fatal("`d` raised no confirm the second time")
	}
	run := m.bulkConfirmRun
	if run == nil {
		t.Fatal("the confirm carries no action")
	}
	msg := run()()
	if sc, ok := msg.(convScopeMsg); !ok || sc.err != "" {
		t.Fatalf("the delete produced %#v", msg)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deleted) != 1 || stub.deleted[0] != "mcp-conv" {
		t.Errorf("deleted %v, want the selected row's id (mcp-conv)", stub.deleted)
	}
}

// THE PROJECT'S DEFINITIONS ARE NOT DELETABLE FROM HERE, and the refusal says why: they belong to the
// project, which is where they must be edited. A silent no-op would read as a broken key.
func TestScopeRefusesToWriteAProjectOwnedDefinition(t *testing.T) {
	m, stub := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	for _, key := range []string{"d", "i", "k"} {
		m.dock.SetError("")
		putCursor(m, rowIndexOf(t, m, "postgres"))
		pressScope(t, m, key)
		if m.bulkConfirm != nil || m.convScopeForm != nil {
			t.Fatalf("`%s` acted on a project-owned definition", key)
		}
		if m.dock.Err == "" {
			t.Errorf("`%s` on a project-owned definition said nothing — a key that appears to do "+
				"nothing reads as broken", key)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.deleted)+len(stub.installed)+len(stub.secrets) != 0 {
		t.Error("a project-owned definition was written from the conversation scope")
	}
}

// `i` INSTALLS THE ROW'S ID (the explicit auto-install must not re-resolve a name), and `k` opens the
// write-only credential form for it.
func TestScopeInstallAndCredentialAddressTheRow(t *testing.T) {
	m, stub := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	putCursor(m, rowIndexOf(t, m, "github"))

	pressScope(t, m, "i")
	if m.convScopeForm == nil {
		t.Fatal("`i` opened no install form")
	}
	cmd, err := m.convScopeForm.OnSubmit(map[string]string{"confirm": "yes"}, nil)
	if err != nil {
		t.Fatalf("the install form refused: %v", err)
	}
	if msg := cmd(); msg == nil {
		t.Fatal("the install confirmed but produced no write")
	}
	stub.mu.Lock()
	installed := append([]string{}, stub.installed...)
	stub.mu.Unlock()
	if len(installed) != 1 || installed[0] != "mcp-conv" {
		t.Errorf("installed %v, want the selected row's id (mcp-conv)", installed)
	}

	m.convScopeForm = nil
	putCursor(m, rowIndexOf(t, m, "github"))
	pressScope(t, m, "k")
	if m.convScopeForm == nil {
		t.Fatal("`k` opened no credential form")
	}
	cmd, err = m.convScopeForm.OnSubmit(map[string]string{"name": "GH_TOKEN", "value": "s3cret"}, nil)
	if err != nil {
		t.Fatalf("the credential form refused: %v", err)
	}
	cmd()
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.secrets) != 1 || stub.secrets[0].GetId() != "mcp-conv" || stub.secrets[0].GetName() != "GH_TOKEN" {
		t.Errorf("the credential write = %+v, want the selected row's id and the typed key", stub.secrets)
	}
}

// `s` OPENS THE SKILL-FILE LIST PREFILLED with what the conversation holds, and saving it writes the
// list — the TUI's control for skill_files, in the same modal as the MCP half.
func TestScopeSkillFilesFormIsPrefilledAndWritesTheList(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	pressScope(t, m, "s")
	if m.convScopeForm == nil {
		t.Fatal("`s` opened no skill-file form")
	}
	if got := m.convScopeForm.Values["paths"]; got != "/skills/conv.md" {
		t.Errorf("the skill-file field is prefilled %q, want the conversation's current list — a "+
			"removal must be an edit, not a retype", got)
	}
	m.convScopeForm.Set("paths", "/skills/conv.md, /skills/other.md")
	cmd, err := m.convScopeForm.OnSubmit(m.convScopeForm.Values, nil)
	if err != nil {
		t.Fatalf("the skill-file form refused: %v", err)
	}
	msg := cmd()
	cm, ok := msg.(chat.ConversationMutatedMsg)
	if !ok || cm.Err != "" {
		t.Fatalf("the skill-file write produced %#v", msg)
	}
	if cm.ID != "c1" {
		t.Errorf("the write targeted %q, want the open conversation", cm.ID)
	}
}

// A SKILL ROW'S `d` removes THAT path from the list and confirms first.
func TestScopeRemovesOneSkillPath(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	putCursor(m, rowIndexOf(t, m, "/skills/conv.md"))
	pressScope(t, m, "d")
	if m.bulkConfirm == nil {
		t.Fatal("`d` on a skill path removed it without confirming")
	}
	if !strings.Contains(m.bulkConfirm.Body, "/skills/conv.md") {
		t.Errorf("the confirm does not name the path it will remove: %q", m.bulkConfirm.Body)
	}
	if run := m.bulkConfirmRun; run != nil {
		if msg := run()(); msg == nil {
			t.Error("the confirmed removal produced no write")
		}
	}
}

// --- the modal's own behaviour ----------------------------------------------

// ESC CLOSES IT, and nothing else does: the modal stays up while the operator works through it.
func TestScopeEscCloses(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	pressScope(t, m, "esc")
	if m.scope != nil {
		t.Error("esc did not close the scope modal")
	}
}

// THE CURSOR SKIPS HEADINGS AND NOTES: it can only rest on a real row, so a verb always has a subject.
func TestScopeCursorOnlyRestsOnRealRows(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	for i := 0; i < 12; i++ {
		row, ok := m.scope.selected(m)
		if !ok {
			t.Fatal("the cursor left the row list")
		}
		if !row.selectable {
			t.Fatalf("the cursor rested on a non-selectable row: %+v", row)
		}
		pressScope(t, m, "down")
	}
}

// SWITCHING CONVERSATION CLOSES IT. A modal that outlived its conversation would edit one chat's scope
// while another was open — the same reason the GUI's disclosure closes on a switch.
func TestScopeClosesWhenTheConversationChanges(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")

	// A different conversation is opened.
	m.chatConvID = "c2"
	m.scopeConversationChanged()
	if m.scope != nil {
		t.Error("the modal survived a conversation switch")
	}

	// And a new chat closes it too.
	openScopeFrom(t, m, "/scope")
	m.newChat()
	if m.scope != nil {
		t.Error("the modal survived /new")
	}
}

// A FETCH FOR A CONVERSATION THE MODAL IS NO LONGER ABOUT IS DISCARDED: the reply is asynchronous, so
// applying it would put one chat's definitions in another's scope pane.
func TestScopeDiscardsAStaleFetch(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	m.onScopeData(scopeDataMsg{
		convID: "some-other-chat",
		mcp:    []*apiv1.MCPServer{{Id: "x", Name: "not-this-conversation"}},
	})
	if body := scopeText(m); strings.Contains(body, "not-this-conversation") {
		t.Errorf("a stale fetch was applied to the modal:\n%s", body)
	}
}

// THE MODAL OWNS THE KEYBOARD BUT NOT THE WORLD: it consumes keys and the mouse, and lets everything
// else through — so the chat waiter behind it keeps running (see the router's note).
func TestScopeDoesNotSwallowNonKeyMessages(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	opened := m.scope != nil
	// A transcript message must reach the shell's handler, which re-arms the waiter. If the modal ate
	// it, the chat pane would freeze for the rest of the session.
	next, cmd := m.Update(chat.TranscriptMsg{ConvID: "c1"})
	mm, ok := next.(*App)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	if !opened || mm.scope == nil {
		t.Fatal("fixture: the modal should still be open")
	}
	if cmd == nil {
		t.Error("a transcript message was swallowed by the modal — the chat waiter would starve")
	}
}

// --- what the operator can SEE (the layer the report is true at) -------------

// PRESSING A VERB PUTS ITS RESULT IN THE PAINTED FRAME, not merely in a field.
//
// The operator: "none of the buttons within the modal does anything excepet for ESC".
//
// The keys were reaching the shell — the form WAS created — but the shell painted it UNDERNEATH the
// scope modal: viewFrame composited the scope view AFTER convScopeForm, so the form the verb had just
// opened was covered by the list it was opened from. Every assertion in this file checked a FIELD
// (m.convScopeForm != nil), which is exactly the layer that was fine; the operator judges m.View().
// This is the same lesson as the transcript's tail-clipping test, and the reason both now assert on the
// frame.
func TestScopeVerbShowsItsFormInThePaintedFrame(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")

	pressScope(t, m, "a")
	if m.convScopeForm == nil {
		t.Fatal("fixture: `a` built no form")
	}
	frame := stripANSI(m.View())
	if !strings.Contains(frame, "Define an MCP server") {
		t.Errorf("the definition form the operator just asked for is NOT in the painted frame — it is "+
			"being drawn under the scope modal, so the key reads as dead:\n%s", tailOf(frame, 1600))
	}
	for _, want := range []string{"Name", "Transport", "Command"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the definition form's %q field is not visible:\n%s", want, tailOf(frame, 1600))
		}
	}
}

// AND THE MODAL IS BACK WHEN THE FORM IS DISMISSED. Reversal of the same ordering: after esc the list
// must be what the operator sees, with the form gone.
func TestScopeModalIsVisibleAgainAfterTheFormCloses(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	pressScope(t, m, "a")

	m.convScopeKey(tea.KeyMsg{Type: tea.KeyEsc}) // the modal host's esc: closes the form
	if m.convScopeForm != nil {
		t.Fatal("esc did not close the definition form")
	}
	frame := stripANSI(m.View())
	// The title NAMES THE OWNER now (the modal serves three scopes), so the assertion follows the
	// surface rather than a fixed string.
	if !strings.Contains(frame, "MCP + skills — this conversation") {
		t.Errorf("the scope list is not what the operator sees after closing the form:\n%s", tailOf(frame, 1200))
	}
	if strings.Contains(frame, "Define an MCP server") {
		t.Errorf("the definition form outlived its dismissal:\n%s", tailOf(frame, 1200))
	}
}

// THE KEY HINT SURVIVES IN FULL — the operator: "the shortcut advice is also a bit cut off".
//
// The hint is the ONLY place the verbs are named, so a cut-off hint hides exactly the capabilities the
// modal exists to advertise. It was rendered through truncateRight (ONE line, hard cut), which always
// kills the END — and the end is "esc: close".
//
// Asserted two ways, because there are two things to be wrong: the WRAP must lose nothing at any width,
// and the VIEW must actually use the wrapped form.
func TestScopeKeyHintIsNotCutOff(t *testing.T) {
	m, _ := newScopeApp(t)
	openScopeFrom(t, m, "/scope")
	// The MCP row has the LONGEST hint (it names install, credential and delete as well), so a width
	// that fits it fits every other row's.
	putCursor(m, rowIndexOf(t, m, "github"))
	items := m.scopeHintItems()

	for _, w := range []int{120, 80, 72, 60, 48, 40, 24} {
		m.width = w
		inner := m.modalInnerWidth()
		lines := wrapHintItems(items, inner)
		if len(lines) == 0 {
			t.Fatalf("width %d: the hint produced no lines", w)
		}
		for i, l := range lines {
			if n := len([]rune(l)); n > inner {
				t.Errorf("width %d: hint line %d is %d cells, over the %d-cell interior — the panel "+
					"will cut it: %q", w, i, n, inner, l)
			}
		}
		// NOTHING IS LOST. The lines were broken at the separators, so concatenating them reproduces the
		// items contiguously (a hard-split item is contiguous across the two lines it spans).
		flat := strings.Join(lines, "")
		for _, it := range items {
			if !strings.Contains(flat, it) {
				t.Errorf("width %d: the hint dropped %q — a verb the operator is looking for:\n%q",
					w, it, flat)
			}
		}

		// AND THE VIEW USES IT: the last verb must be in the rendered modal, not merely in a helper.
		body := stripANSI(m.scopeBody())
		if !strings.Contains(body, "esc: close") {
			t.Errorf("width %d: the rendered modal lost the hint's last verb:\n%s", w, body)
		}
		if !strings.Contains(body, "↑/↓ move") {
			t.Errorf("width %d: the rendered modal lost the hint entirely:\n%s", w, body)
		}
	}
}

// THE EMPTY SCOPE STILL NAMES WHAT CAN BE DONE. The operator's screenshot is this state — no
// definitions, no skill files — and every row is a heading or a note, so nothing is selectable and the
// cursor has nowhere to go. The keys that CREATE something must still be advertised, or there is no way
// out of the state at all.
func TestEmptyScopeStillAdvertisesTheCreateKeys(t *testing.T) {
	m, stub := newScopeApp(t)
	stub.servers = nil
	stub.projectServers = nil
	// Unassigned, so there is no inherited half either: this is the screenshot's shape.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.railProjects = nil
	openScopeFrom(t, m, "/scope")

	body := scopeText(m)
	for _, want := range []string{"add", "catalog", "skill files"} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty scope does not advertise %q — the operator's screenshot is this state, "+
				"and nothing in it is selectable:\n%s", want, body)
		}
	}
	// And the hint must not be cut, here too: this is where the operator saw it clipped.
	if strings.Contains(body, "\u2026") {
		t.Errorf("the empty scope's hint is truncated:\n%s", body)
	}
}
