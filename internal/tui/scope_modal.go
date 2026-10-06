package tui

// scope_modal.go — THE conversation scope modal: this conversation's MCP servers and skill files in ONE
// surface, opened by /scope, /mcp and /skills.
//
// The operator: "The MCP tools that were added for the TUI is abysmal. There are too many commands and
// the shortcuts go off screen. I think simply type /mcp should pop up a modal that mimics what the gui
// has for scope. Same with /skills. It should pop up the same modal. Or /scope."
//
// WHAT WAS WRONG, MEASURED. The old surface was a subcommand grammar:
//
//	/mcp [define | edit <name> | delete <name> | secret <name> | install <name>]
//
// — 76 cells, which the command palette (capped at 70, paletteContentWidth) had to CLIP, so the last
// verbs were literally off the edge of the row ("the shortcuts go off screen"). Worse, every one of the
// five capabilities was a command the operator had to already know, with no way to SEE what the
// conversation held: `/mcp` with no argument printed one dock line and vanished. A list of things is a
// LIST, and the GUI's Scope disclosure (frontend/src/components/ask/ConversationScopeDisclosure.tsx)
// proves it — one panel with the conversation's servers (add/edit/delete/install/credential) and its
// skill files, with the PROJECT's contributions shown read-only above, because a conversation's scope is
// the union of the two.
//
// So the modal mirrors that panel, and the names collapse to one: /scope is the honest name for what it
// edits (the conversation's scope), and /mcp and /skills remain as the words an operator will actually
// type — all three open THIS. The capability is unchanged; the control is now visible, discoverable and
// keyed to named actions instead of to a grammar.
//
// IT IS NOT A SECOND MCP SURFACE. Every write still goes through screens/mcpforms (the ONE TUI MCP
// surface, shared with the project's panes and the worker-version editor), owner-stamped with
// Owner{ConversationID}; this file is the LIST and the key routing, and it adds no form of its own.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// scopeRowKind is what a modal row IS, which decides both how it draws and what the keys do to it.
type scopeRowKind int

const (
	scopeRowSection      scopeRowKind = iota // a section heading (not selectable)
	scopeRowMCP                              // a definition THIS conversation owns (selectable, actionable)
	scopeRowInheritedMCP                     // a definition the PROJECT owns (selectable to read the note, not writable)
	scopeRowSkill                            // a skill path THIS surface carries (selectable, removable)
	scopeRowInlineMCP                        // an INLINE spec (a worker version's) — editable in memory, no row
	scopeRowProjectSkill                     // a skill path the PROJECT contributes (read-only)
	scopeRowNote                             // an empty-state sentence or an explanation (not selectable)
)

// scopeRow is one line of the modal.
type scopeRow struct {
	kind scopeRowKind
	// id is the definition's id (MCP rows) or the path (skill rows) — the thing the keys act on.
	id string
	// name is the definition's name / the path, used to build messages.
	name string
	// text is what the row DRAWs (already composed; the keys use id/name).
	text string
	// selectable reports whether the cursor may rest here.
	selectable bool
}

// scopeModal is the open modal's state: what was FETCHED, plus where the cursor is.
//
// The ROWS are rebuilt from current shell state on every render rather than stored, because two of the
// three things they show do not come from the modal's own fetch: the conversation's skill files ride on
// the rail's conversation list, and the project's contributions ride on the rail's project list. Caching
// rows would leave the pane showing a skill path the operator just removed until the next fetch landed.
// scopeOwnerKind is WHERE the entries the modal manages live and how they persist — the TUI mirror of
// the GUI's MCPScope (frontend/src/components/MCPServersPanel.tsx), which is ONE panel parameterized by
// exactly these three cases.
//
// THE TUI ALREADY HAD THE PANEL FOR ONE OF THEM. /scope (and /mcp, /skills) opens it for a CONVERSATION;
// a project and a worker version were left with raw text fields — an absolute-path list for skills and a
// JSON blob for MCP — so the operator had a modal on one surface and hand-editing on the other two. The
// operator: "I simply want at least a similar modal to manage MCP and skills for projects and workers.
// Right now it's one line edit with absolute paths. It doesn't pop up a modal like /mcp or /scope does."
//
// The three cases differ in exactly two ways, and both are the GUI's:
//
//	conversation    OWNED ROWS through the MCP service (conversation_id), skills on the conversation row.
//	                Also shows the PROJECT's contributions read-only, because a conversation consumes
//	                the union of the two.
//	project         OWNED ROWS through the MCP service (project_id), skills on the project row. Nothing
//	                is inherited at this scope — the project IS the source.
//	workerVersion   INLINE SPECS with no row at all: a published version is immutable, so the specs live
//	                in the version's permissions JSON. NO INSTALL (an install status can only live on a
//	                row), but the CATALOG and a CREDENTIAL both apply here — the catalog prefills the
//	                add form, and a credential is SELECTED from the tenant secrets store and built into
//	                the spec's own env/headers as ${SECRET_NAME} (mcpforms.CredentialForm).
type scopeOwnerKind int

const (
	ownerConversation scopeOwnerKind = iota
	ownerProject
	ownerWorkerVersion
)

type scopeModal struct {
	// kind is the scope this modal manages. See scopeOwnerKind.
	kind scopeOwnerKind
	// convID / projectID identify the surface for the OWNED-ROW scopes. The fetch result is discarded
	// when it does not match the open surface, so a reply that lands after a switch cannot scope the
	// wrong thing.
	convID    string
	projectID string
	// label names the surface in the modal's title (a project's name, a worker's name + version).
	label string

	// mcp and inherited are the OWNED definitions at this scope, and the read-only contributions from
	// an outer scope (a conversation's project). A project scope has no inherited half.
	mcp       []*apiv1.MCPServer
	inherited []*apiv1.MCPServer

	// secrets is the tenant secrets store's NAMES, and secretsNote explains why that list may be
	// incomplete (the store could not be read, or it is longer than one page). A WORKER VERSION needs
	// them and the other two scopes do not: an owned row stores a credential against its ID, while an
	// inline spec has no row and can only REFERENCE one — which is a selection, so it needs the list.
	secrets     []mcpforms.SecretChoice
	secretsNote string

	// ── the WORKER-VERSION half, which is IN MEMORY ───────────────────────────────────────
	//
	// A version has no MCP row to create or delete: its specs live in the version's permissions JSON, so
	// the modal EDITS AN ARRAY and commits it. These fields hold that array and the two things needed to
	// write it back losslessly — the base permissions blob (the modal owns ONE key of it, never the
	// whole blob) and the version's skill paths.
	inline     []mcpforms.InlineSpec
	permBase   string
	skills     []string
	saveInline func(permJSON, skillsJSON string) tea.Cmd

	// loading is true until the first fetch lands, and err carries a failed one.
	loading bool
	err     string
	// dirty marks an IN-MEMORY edit that has not been committed yet. The inline form's save callback
	// cannot return a command (mcpforms.InlineForm's contract), so a worker-version edit is committed when
	// the FORM CLOSES — and only when something actually changed, so dismissing a form unchanged is not a
	// write.
	dirty bool
	// cursor is the index into rows() of the SELECTED row (movement skips non-selectable rows).
	cursor int
	// scroll is the first drawn row, so a long scope never overflows the terminal.
	scroll int
}

// ownedRows reports whether this scope's definitions are ROWS in mcp_servers (so they can be created,
// edited, deleted, installed and given credentials) rather than inline specs written into a version.
func (s *scopeModal) ownedRows() bool { return s.kind != ownerWorkerVersion }

// ownerID is the id the MCP list/create/update calls are scoped by ("" for a worker version, which has
// no row).
func (s *scopeModal) ownerID() string {
	if s.kind == ownerProject {
		return s.projectID
	}
	return s.convID
}

// ownerName names the scope for messages ("this conversation", "this project", "this worker version").
func (s *scopeModal) ownerName() string {
	switch s.kind {
	case ownerProject:
		return "this project"
	case ownerWorkerVersion:
		return "this worker version"
	}
	return "this conversation"
}

// scopeDataMsg carries a fetched scope.
type scopeDataMsg struct {
	convID    string
	mcp       []*apiv1.MCPServer
	inherited []*apiv1.MCPServer
	err       string
}

// openScopeModal opens the modal for the OPEN conversation and starts its fetch.
//
// A conversation is required: the whole surface is per-conversation (a definition belongs to exactly one
// scope, and the skills belong to the conversation row), so with nothing open there is nothing to edit —
// and the same refusal the old commands gave is the right one.
func (m *App) openScopeModal() tea.Cmd {
	if m.chatConvID == "" {
		m.dock.SetError("no conversation open — the scope belongs to ONE conversation; /new starts one, " +
			"/project chooses the workspace")
		return nil
	}
	m.scope = &scopeModal{kind: ownerConversation, convID: m.chatConvID, loading: true}
	// The project's skill files are part of the union this modal shows, and the rail's project list is
	// where they live — a cold rail would render the inherited half as empty.
	cmd := tea.Cmd(m.loadScope())
	if len(m.railProjects) == 0 && m.clients != nil {
		cmd = tea.Batch(cmd, m.loadRailProjects())
	}
	return cmd
}

// openProjectScopeModal opens the same modal for a PROJECT — its MCP definitions and its skill files.
//
// The project is the SOURCE of what its conversations and workers inherit, so nothing is inherited here
// and the whole surface is writable.
func (m *App) openProjectScopeModal(projectID, name string) tea.Cmd {
	if projectID == "" {
		m.dock.SetError("no project selected")
		return nil
	}
	m.scope = &scopeModal{kind: ownerProject, projectID: projectID, label: name, loading: true}
	return m.loadScope()
}

// openWorkerVersionScopeModal opens the modal for a WORKER VERSION — its inline MCP specs and its skill
// files, both held in the version's own fields.
//
// IN MEMORY, because a version has no rows: a published version is immutable, so its specs live in the
// permissions JSON and are written back THROUGH the version update. The caller supplies that write as
// saveInline (the Execution screen owns the RPC, and the shell must not re-implement it).
func (m *App) openWorkerVersionScopeModal(label, permissionsJSON, skillFilesJSON string, saveInline func(permJSON, skillsJSON string) tea.Cmd) tea.Cmd {
	m.scope = &scopeModal{
		kind:     ownerWorkerVersion,
		label:    label,
		inline:   mcpforms.ParseInline(permissionsJSON),
		permBase: permissionsJSON,
		// THE VERSION'S FIELD IS A JSON ARRAY, not the newline/comma path list the form uses — decoding
		// it with the path-list parser left the raw JSON as a single "path" (caught by
		// TestWorkerVersionSkillsCommitThroughTheSave).
		skills:     decodeSkillFilesJSON(skillFilesJSON),
		saveInline: saveInline,
	}
	return nil
}

// closeScopeModal closes it. Called when the surface under it changes identity (a different
// conversation), NOT on every repaint: the modal is the shell's, and it belongs to one conversation —
// the GUI closes its Scope disclosure on a conversation switch for exactly this reason
// (ConversationScopeDisclosure: "Switching conversation closes it").
func (m *App) closeScopeModal() {
	m.scope = nil
}

// loadScope fetches the scope's owned definitions — and, for a conversation, the project's for the
// read-only half. A WORKER VERSION fetches nothing: its specs are in the version's own fields, which the
// opener already put in memory.
func (m *App) loadScope() tea.Cmd {
	if m.scope == nil {
		return nil
	}
	if m.scope.kind == ownerWorkerVersion {
		m.scope.loading = false
		return nil
	}
	convID := m.scope.convID
	ownerID := m.scope.ownerID()
	isProject := m.scope.kind == ownerProject
	cl := m.clients
	projectID := ""
	if !isProject {
		projectID = m.conversationProjectID(convID)
	}
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return scopeDataMsg{convID: convID, err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// THE OWNER COLUMN IS THE SELECTION: a definition belongs to exactly one scope, so the request
		// carries that scope's id and nothing else (the server enforces the owner XOR).
		req := &apiv1.MCPServerListRequest{}
		if isProject {
			req.ProjectId = ownerID
		} else {
			req.ConversationId = ownerID
		}
		mine, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(req))
		if err != nil {
			return scopeDataMsg{convID: convID, err: err.Error()}
		}
		out := scopeDataMsg{convID: convID, mcp: mine.Msg.GetServers()}
		// A PROJECT scope is the source: it inherits nothing, so there is no second read.
		if isProject {
			return out
		}
		// The PROJECT's definitions are a second read, and only when there is a project: a
		// conversation with no project inherits nothing, so there is nothing to ask for.
		if projectID != "" {
			inherited, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
				ProjectId: projectID,
			}))
			if err != nil {
				// The inherited half is CONTEXT, not the subject. A failure here is reported on the modal
				// but must not hide the conversation's own definitions, which are what the operator came
				// to edit.
				out.err = "the project's definitions could not be read: " + err.Error()
				return out
			}
			out.inherited = inherited.Msg.GetServers()
		}
		return out
	}
}

// onScopeData applies a fetched scope. A reply for a conversation this modal is no longer about is
// DISCARDED rather than applied: the fetch is asynchronous, and a switch in between would otherwise
// present one chat's definitions as another's.
func (m *App) onScopeData(msg scopeDataMsg) {
	if m.scope == nil || m.scope.convID != msg.convID {
		return
	}
	m.scope.loading = false
	m.scope.err = msg.err
	m.scope.mcp = msg.mcp
	m.scope.inherited = msg.inherited
	// SEAT THE CURSOR ON A REAL ROW before the first paint. A fresh modal starts at 0, which is the
	// section heading — and a cursor resting on a heading is both wrong to look at and a trap for every
	// verb (`enter`, `d`, `i` … all resolve through the selected row). The row list only becomes
	// knowable once the fetch lands, which is why this happens here rather than in openScopeModal.
	m.scope.seatCursor(m)
}

// seatCursor moves the cursor onto the first SELECTABLE row. It is idempotent: a cursor already on a
// real row stays exactly where the operator left it (a re-read must not move their selection).
func (s *scopeModal) seatCursor(m *App) {
	rows := s.rows(m)
	if len(rows) == 0 {
		return
	}
	if s.cursor >= 0 && s.cursor < len(rows) && rows[s.cursor].selectable {
		return
	}
	for i, r := range rows {
		if r.selectable {
			s.cursor = i
			return
		}
	}
}

// conversationProjectID is the project a conversation belongs to ("" when unassigned).
func (m *App) conversationProjectID(id string) string {
	for _, c := range m.conversations {
		if c.ID == id {
			return c.ProjectID
		}
	}
	return ""
}

// projectSkillFiles reads a project's skill path list from the rail — the same source the conversation
// surface uses for the inherited half, so the two cannot disagree about what a project contributes.
func (m *App) projectSkillFiles(projectID string) []string {
	for _, p := range m.railProjects {
		if p.ID == projectID {
			return p.SkillFiles
		}
	}
	return nil
}

// projectForConversation resolves the project ROW a conversation belongs to, for its name and its
// skill files (the inherited half of the modal).
func (m *App) projectForConversation(id string) (railProject, bool) {
	pid := m.conversationProjectID(id)
	if pid == "" {
		return railProject{}, false
	}
	for _, p := range m.railProjects {
		if p.ID == pid {
			return p, true
		}
	}
	return railProject{}, false
}

// --- the rows ---------------------------------------------------------------

// rows builds the modal's rows from current shell state. See scopeModal's doc for why this is derived
// per render rather than stored.
//
// IT BRANCHES ON THE OWNER, which is the whole point of the generalization: the GUI's panel renders one
// shape over three persistence targets, and so does this. The two halves that differ are named in
// scopeOwnerKind.
func (s *scopeModal) rows(m *App) []scopeRow {
	switch s.kind {
	case ownerWorkerVersion:
		return s.workerVersionRows()
	case ownerProject:
		return s.projectRows(m)
	}
	return s.conversationRows(m)
}

// conversationRows is the original surface: the conversation's own definitions, the PROJECT's read-only
// above them (a conversation consumes the union), then the conversation's skill files and the project's.
func (s *scopeModal) conversationRows(m *App) []scopeRow {
	var out []scopeRow

	// --- this conversation's MCP servers ---
	out = append(out, scopeRow{kind: scopeRowSection, text: "MCP servers — this conversation"})
	if len(s.mcp) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote,
			text: "none yet — a adds one by hand, c adds one from the catalog"})
	}
	for _, srv := range s.mcp {
		out = append(out, scopeRow{
			kind: scopeRowMCP, id: srv.GetId(), name: srv.GetName(),
			text: mcpScopeLine(srv), selectable: true,
		})
	}

	// --- the project's, read-only (the union the server actually renders) ---
	if proj, ok := m.projectForConversation(s.convID); ok {
		out = append(out, scopeRow{kind: scopeRowSection,
			text: "Inherited from project " + proj.Name + " (read-only)"})
		if len(s.inherited) == 0 {
			out = append(out, scopeRow{kind: scopeRowNote, text: "the project defines none"})
		}
		for _, srv := range s.inherited {
			out = append(out, scopeRow{
				kind: scopeRowInheritedMCP, id: srv.GetId(), name: srv.GetName(),
				text: mcpScopeLine(srv), selectable: true,
			})
		}
	}

	// --- skill files: the conversation's own, then the project's ---
	out = append(out, scopeRow{kind: scopeRowSection, text: "Skill files — this conversation"})
	mine := m.conversationSkillFiles(s.convID)
	if len(mine) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote, text: "none yet — s sets the path list"})
	}
	for _, p := range mine {
		out = append(out, scopeRow{kind: scopeRowSkill, id: p, name: p, text: p, selectable: true})
	}
	if proj, ok := m.projectForConversation(s.convID); ok && len(proj.SkillFiles) > 0 {
		out = append(out, scopeRow{kind: scopeRowSection,
			text: "From project " + proj.Name + " (read-only)"})
		for _, p := range proj.SkillFiles {
			out = append(out, scopeRow{kind: scopeRowProjectSkill, id: p, name: p, text: p, selectable: true})
		}
	}
	return out
}

// projectRows is the PROJECT surface: its own definitions and its own skill files, with nothing
// inherited — a project is the SOURCE of what its conversations and workers inherit, so there is no
// outer scope to show.
func (s *scopeModal) projectRows(m *App) []scopeRow {
	var out []scopeRow

	out = append(out, scopeRow{kind: scopeRowSection, text: "MCP servers — this project"})
	if len(s.mcp) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote,
			text: "none yet — a adds one by hand, c adds one from the catalog"})
	}
	for _, srv := range s.mcp {
		out = append(out, scopeRow{
			kind: scopeRowMCP, id: srv.GetId(), name: srv.GetName(),
			text: mcpScopeLine(srv), selectable: true,
		})
	}

	out = append(out, scopeRow{kind: scopeRowSection, text: "Skill files — this project"})
	mine := m.projectSkillFiles(s.projectID)
	if len(mine) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote, text: "none yet — s sets the path list"})
	}
	for _, p := range mine {
		out = append(out, scopeRow{kind: scopeRowSkill, id: p, name: p, text: p, selectable: true})
	}
	// WHAT CONSUMES THIS SCOPE, named — the same question the conversation surface answers in reverse
	// ("why is this server available here?"), and the reason the definitions are worth managing here.
	//
	// A PROJECT'S DEFINITIONS ARE CONSUMED BY ITS CONVERSATIONS, NOT BY WORKERS. Workers are TENANT-LEVEL
	// ("Workers are tenant level and not project level"), and a worker version's MCP specs are its OWN
	// inline list — the GUI's panel resolves a version's specs from the version, and nothing here is read
	// by a worker. Saying otherwise would send an operator to add a server "for my workers" on a project
	// page, which would do nothing.
	out = append(out, scopeRow{kind: scopeRowNote,
		text: "consumed by this project's conversations"})
	return out
}

// workerVersionRows is the WORKER-VERSION surface: the inline specs the version's permissions carry and
// its skill files, both IN MEMORY (a version has no rows to fetch, and a published one is immutable).
func (s *scopeModal) workerVersionRows() []scopeRow {
	var out []scopeRow

	out = append(out, scopeRow{kind: scopeRowSection, text: "MCP servers — inline in this version"})
	if len(s.inline) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote,
			text: "none yet — a adds one; a version's specs are written into its permissions"})
	}
	for i := range s.inline {
		out = append(out, scopeRow{
			kind: scopeRowInlineMCP, id: s.inline[i].ID, name: s.inline[i].ID,
			text: inlineScopeLine(s.inline[i]), selectable: true,
		})
	}

	out = append(out, scopeRow{kind: scopeRowSection, text: "Skill files — this version"})
	if len(s.skills) == 0 {
		out = append(out, scopeRow{kind: scopeRowNote, text: "none yet — s sets the path list"})
	}
	for _, p := range s.skills {
		out = append(out, scopeRow{kind: scopeRowSkill, id: p, name: p, text: p, selectable: true})
	}
	// WHAT THIS SCOPE IS, said plainly: the specs are this version's OWN — a worker is TENANT-LEVEL, so it
	// inherits nothing from a project, and a catalog add would have no row to create here.
	out = append(out, scopeRow{kind: scopeRowNote,
		text: "this version's own specs — a worker is tenant-level, so it inherits no project's MCP; " +
			"k points a spec's env/header at a stored secret"})
	return out
}

// selected is the row under the cursor (zero value when the modal is empty).
func (s *scopeModal) selected(m *App) (scopeRow, bool) {
	rows := s.rows(m)
	if s.cursor < 0 || s.cursor >= len(rows) {
		return scopeRow{}, false
	}
	return rows[s.cursor], true
}

// move shifts the cursor by delta over the SELECTABLE rows, wrapping — the same rule kit2.Card uses, so
// the cursor can never rest on a heading or an empty-state sentence and `enter` always has a subject.
func (s *scopeModal) move(m *App, delta int) {
	rows := s.rows(m)
	n := len(rows)
	if n == 0 {
		return
	}
	s.clampCursor(n)
	i := s.cursor
	for step := 0; step < n; step++ {
		i = (i + delta + n) % n
		if rows[i].selectable {
			s.cursor = i
			return
		}
	}
}

// clampCursor keeps the cursor inside the row list (a list can shrink under it: a definition deleted, a
// skill path removed).
func (s *scopeModal) clampCursor(n int) {
	if s.cursor >= n {
		s.cursor = n - 1
	}
	if s.cursor < 0 {
		s.cursor = 0
	}
}

// mcpScopeLine renders one definition the way the GUI's card does: transport, where it points, whether
// it is enabled, and the two states an operator has to know before using it (installed, credential
// stored).
func mcpScopeLine(s *apiv1.MCPServer) string {
	transport := "stdio"
	where := s.GetCommand() + " " + strings.Join(s.GetArgs(), " ")
	if s.GetTransport() == apiv1.MCPServerTransport_MCP_SERVER_TRANSPORT_STREAMABLE_HTTP {
		transport, where = "http", s.GetUrl()
	}
	parts := []string{s.GetName(), transport}
	if w := strings.TrimSpace(where); w != "" {
		parts = append(parts, w)
	}
	if !s.GetEnabled() {
		parts = append(parts, "disabled")
	}
	parts = append(parts, installStateWord(s.GetInstallStatus()))
	if s.GetHasSecretStored() {
		parts = append(parts, "secrets stored")
	}
	return strings.Join(parts, " · ")
}

// inlineScopeLine renders one INLINE spec (a worker version's) — the same facts mcpScopeLine shows for
// an owned row, minus the two a version cannot have (an install status and a stored credential).
func inlineScopeLine(spec mcpforms.InlineSpec) string {
	transport := "stdio"
	where := strings.Join(spec.Command, " ")
	if spec.Type == "http" || spec.URL != "" {
		transport, where = "http", spec.URL
	}
	parts := []string{spec.ID, transport}
	if w := strings.TrimSpace(where); w != "" {
		parts = append(parts, w)
	}
	if spec.Enabled != nil && !*spec.Enabled {
		parts = append(parts, "disabled")
	}
	return strings.Join(parts, " · ")
}

// installStateWord names a definition's install status — the one field the GUI renders as a badge (and
// the one an operator needs before a server silently fails to start).
//
// UNKNOWN (1) and UNSPECIFIED (0) render as NOTHING rather than as "not installed": those are the
// statuses of a definition that was never a catalog entry, so "not installed" would be a claim about a
// check that never ran (the same distinction the GUI's InstallStatusBadge keeps by defaulting to "not
// installed" only after the check).
func installStateWord(st apiv1.MCPInstallStatus) string {
	switch st {
	case apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_INSTALLED:
		return "installed"
	case apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_FAILED:
		return "install failed"
	case apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_INSTALLING:
		return "installing…"
	case apiv1.MCPInstallStatus_MCP_INSTALL_STATUS_NOT_INSTALLED:
		return "not installed"
	}
	return ""
}

// --- the keys ---------------------------------------------------------------

// scopeKey drives the modal. Like every other shell modal it OWNS every key while it is up, so a
// keystroke aimed at the list can never reach the composer behind it.
//
// THE VERBS ARE NAMES, NOT A GRAMMAR. Movement is up/down (with the project picker's ctrl+p/ctrl+n
// alternates, so the two shell modals move the same way), and each capability has its own letter with
// the hint bar spelling all of them out — which is the whole point of replacing
// "/mcp define|edit|delete|secret|install": nothing here has to be remembered in advance.
func (m *App) scopeKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.scope == nil {
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.closeScopeModal()
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "ctrl+p":
		m.scope.move(m, -1)
		return m, nil
	case "down", "ctrl+n":
		m.scope.move(m, 1)
		return m, nil
	case "r":
		return m, m.reloadScope()
	case "a":
		return m, m.scopeAddMCP()
	case "c":
		return m, m.scopeAddMCPFromCatalog()
	case "s":
		return m, m.scopeEditSkills()
	case "enter", "e":
		return m, m.scopeEditSelected()
	case "i":
		return m, m.scopeInstallSelected()
	case "k":
		return m, m.scopeCredentialForSelected()
	case "d", "x", "ctrl+x":
		return m, m.scopeDeleteSelected()
	}
	// Every other key is CONSUMED and ignored: the modal is up, and a chord landing behind it is the
	// failure mode every other shell modal takes this same precaution against.
	return m, nil
}

// scopeTarget resolves the selected row and refuses, with a reason, when the key does not apply to it.
// The refusals are named actions rather than silence: "nothing selected" and "that row is read-only"
// are different situations, and a key that appears to do nothing reads as broken.
func (m *App) scopeTarget(verb, needs string) (scopeRow, bool) {
	row, ok := m.scope.selected(m)
	if !ok {
		m.dock.SetError("nothing selected — ↑/↓ moves through the scope")
		return scopeRow{}, false
	}
	owned := m.scope.ownedRows()
	switch needs {
	case "mcp":
		switch row.kind {
		case scopeRowMCP, scopeRowInlineMCP:
			return row, true
		case scopeRowInheritedMCP:
			m.dock.SetError(row.name + " belongs to project " +
				m.projectLabelFor(m.conversationProjectID(m.scope.convID)) +
				" — edit it there; this scope only consumes it")
			return scopeRow{}, false
		}
		m.dock.SetError(verb + " applies to an MCP server — ↑/↓ moves to one (this row is " + scopeRowKindWord(row.kind) + ")")
		return scopeRow{}, false
	case "row":
		// A ROW IN mcp_servers — install needs something to hang an install STATUS on, and an INLINE
		// spec has none (the GUI's panel does not offer Install for that scope either).
		if row.kind == scopeRowMCP && owned {
			return row, true
		}
		if row.kind == scopeRowInlineMCP {
			m.dock.SetError(verb + " needs a stored definition: a worker version's specs are INLINE, so " +
				"there is no row for an install status to live on")
			return scopeRow{}, false
		}
		m.dock.SetError(verb + " applies to an MCP definition — ↑/↓ moves to one")
		return scopeRow{}, false
	case "credential":
		// A DEFINITION ROW *OR* AN INLINE SPEC, because the two attach a credential DIFFERENTLY rather
		// than one of them not at all: an owned row stores it against its ID (mcpsettings.SetSecret),
		// while a version's spec has no row and gets the ${SECRET_NAME} reference built INTO its own
		// config (mcpforms.CredentialForm). The refusal this case replaced — "there is no row to attach
		// a credential to" — was true about the row and wrong about the capability. The operator: "I
		// understand that it is inline but that doesn't mean that info can't be built on top and passed
		// to the config."
		switch row.kind {
		case scopeRowMCP:
			if owned {
				return row, true
			}
		case scopeRowInlineMCP:
			return row, true
		case scopeRowInheritedMCP:
			m.dock.SetError(row.name + " belongs to project " +
				m.projectLabelFor(m.conversationProjectID(m.scope.convID)) +
				" — edit it there; this scope only consumes it")
			return scopeRow{}, false
		}
		m.dock.SetError(verb + " applies to an MCP definition or an inline spec — ↑/↓ moves to one")
		return scopeRow{}, false
	case "writable":
		switch row.kind {
		case scopeRowMCP, scopeRowSkill, scopeRowInlineMCP:
			return row, true
		case scopeRowInheritedMCP, scopeRowProjectSkill:
			m.dock.SetError(row.name + " comes from the project — this scope cannot change it")
			return scopeRow{}, false
		}
	}
	m.dock.SetError(verb + " applies to a row in this scope")
	return scopeRow{}, false
}

// scopeRowKindWord names a row kind for a refusal message.
func scopeRowKindWord(k scopeRowKind) string {
	switch k {
	case scopeRowSection:
		return "a heading"
	case scopeRowSkill, scopeRowProjectSkill:
		return "a skill file"
	case scopeRowInlineMCP:
		return "an inline MCP spec"
	case scopeRowNote:
		return "a note"
	}
	return "another kind of row"
}

// ── the owner-aware writes ───────────────────────────────────────────────────────────────
//
// Every verb below resolves the CURRENT scope's persistence target and writes there: an OWNED ROW
// through the MCP service for a conversation or a project, or the version's own fields for an inline
// spec. That split is the whole difference between the three scopes, and it is the same split the GUI's
// panel makes.

// scopeMCPOwner is the OWNER STAMP for a create/update at this scope. A definition belongs to exactly
// one scope, so this is the field that decides where it lands.
func (s *scopeModal) scopeMCPOwner() mcpforms.Owner {
	if s.kind == ownerProject {
		return mcpforms.Owner{ProjectID: s.projectID}
	}
	return mcpforms.Owner{ConversationID: s.convID}
}

// scopeAddMCP opens the typed definition form for THIS scope — the same control the GUI's Add opens.
func (m *App) scopeAddMCP() tea.Cmd {
	if m.scope.kind == ownerWorkerVersion {
		m.openInlineSpecForm(nil, false)
		return nil
	}
	m.openScopeMCPDefine(nil)
	return nil
}

// openScopeMCPDefine builds the owned-definition form for the current scope, owner-stamped so the write
// cannot land in another scope. With prefill set it EDITS (the owner rides as an echo on the update).
func (m *App) openScopeMCPDefine(prefill *apiv1.MCPServer) {
	s := m.scope
	if s == nil || !s.ownedRows() {
		return
	}
	cl := m.clients
	owner := s.scopeMCPOwner()
	title := "Define an MCP server for " + s.ownerName()
	var seed *apiv1.MCPServerCreateRequest
	if prefill != nil {
		title = "Edit MCP server: " + prefill.GetName()
		seed = serverAsCreate(prefill)
	}
	f := mcpforms.DefineForm(title, owner, seed, func(req *apiv1.MCPServerCreateRequest) tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/scope", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if prefill != nil {
				up := &apiv1.MCPServerUpdateRequest{
					Id:          prefill.GetId(),
					Command:     strPtr(req.GetCommand()),
					ReplaceArgs: boolPtr(true),
					Args:        req.GetArgs(),
					Env:         req.GetEnv(),
					Url:         strPtr(req.GetUrl()),
					Headers:     req.GetHeaders(),
					Enabled:     boolPtr(req.GetEnabled()),
				}
				// THE OWNER IS AN ECHO ON UPDATE: the scope is immutable after create, and a differing
				// echo is rejected — which is what stops an edit silently re-scoping a row.
				up.ProjectId, up.ConversationId = owner.ProjectID, owner.ConversationID
				if _, err := cl.MCP.UpdateMCPServer(ctx, connect.NewRequest(up)); err != nil {
					return convScopeMsg{op: "/scope", err: err.Error()}
				}
				return convScopeMsg{op: "/scope", detail: "updated " + req.GetName()}
			}
			if _, err := cl.MCP.CreateMCPServer(ctx, connect.NewRequest(req)); err != nil {
				return convScopeMsg{op: "/scope", err: err.Error()}
			}
			return convScopeMsg{op: "/scope", detail: "defined " + req.GetName() + " for " + s.ownerName()}
		}
	})
	f.Width = m.modalWidth()
	m.convScopeForm = f
}

// scopeAddMCPFromCatalog opens the registry picker for this scope, which prefills the same definition
// form — the TUI's one-click add, mirroring the GUI's Registry catalog grid. A version has no row to
// create, so this is refused there rather than opening a form whose save would go nowhere.
func (m *App) scopeAddMCPFromCatalog() tea.Cmd {
	cl := m.clients
	owner := m.scope.scopeMCPOwner()
	versionScope := m.scope.kind == ownerWorkerVersion
	save := func(req *apiv1.MCPServerCreateRequest) tea.Cmd {
		// A WORKER VERSION HAS NO ROW TO CREATE, so the catalog PREFILLS ITS ADD FORM instead — the same
		// thing the GUI does (handleCatalogAdd fills the form, and the form's save writes the version's
		// array). The operator confirms before anything is written, and the spec lands inline.
		if versionScope {
			// A MESSAGE, not a direct call: this runs inside the CATALOG form's OnSubmit, and a form must
			// be opened on the live model (see versionSpecPrefillMsg). The spec is converted here because
			// the prefill's shape is the catalog's business.
			spec := mcpforms.InlineSpecFromCreateRequest(req)
			return func() tea.Msg { return versionSpecPrefillMsg{spec: spec} }
		}
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/scope", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.MCP.CreateMCPServer(ctx, connect.NewRequest(req)); err != nil {
				return convScopeMsg{op: "/scope", err: err.Error()}
			}
			return convScopeMsg{op: "/scope", detail: "added " + req.GetName() + " from the catalog"}
		}
	}
	if versionScope {
		// No owner: the catalog is a PREFILL here, and nothing is created from it, so stamping a scope
		// would be a claim about a row that never exists.
		owner = mcpforms.Owner{}
	}
	f := mcpforms.CatalogForm(owner,
		func() []*apiv1.MCPCatalogEntry {
			if cl == nil || cl.MCP == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := cl.MCP.ListMCPCatalog(ctx, connect.NewRequest(&apiv1.MCPCatalogListRequest{}))
			if err != nil {
				return nil
			}
			return res.Msg.GetEntries()
		},
		func(slug string) (*apiv1.MCPServerCreateRequest, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := cl.MCP.PrefillMCPCatalogEntry(ctx, connect.NewRequest(&apiv1.MCPCatalogPrefillRequest{Slug: slug}))
			if err != nil {
				return nil, err
			}
			return res.Msg.GetPrefill(), nil
		},
		save)
	f.Width = m.modalWidth()
	m.convScopeForm = f
	return nil
}

// versionSpecPrefillMsg asks the shell to open the INLINE add form on a spec it has just built — the
// catalog's pick, converted.
//
// IT IS A MESSAGE, NOT A DIRECT CALL, and that is this file's own recorded rule (see
// openWorkerVersionScopeModal: "the modal is opened on the MESSAGE rather than inside the cmd, because a
// cmd mutates a discarded App copy"). The pick runs inside ANOTHER FORM's OnSubmit, so a direct
// `m.openInlineSpecForm(...)` would mutate whichever App copy that callback happens to hold — and if it
// did not reach the live one, the catalog form would still be cleared (it submitted) while the form it
// was supposed to open never appeared. That is precisely the operator's report: "it just jumps back to
// the mcp screen and doesn't show that any MCP servers have been added."
type versionSpecPrefillMsg struct{ spec mcpforms.InlineSpec }

// openInlineSpecForm opens the add/edit form for ONE inline spec of a worker version, and commits it
// through the version's own save. There is no RPC per spec: the modal owns the array and writes it back
// as a whole, which is what keeps a published version's history intact (see scopeOwnerKind).
//
// `seed` PREFILLS the form. A CATALOG PICK passes a seed derived from the catalog entry
// (mcpforms.InlineSpecFromCreateRequest) and isEdit=false, so the two add paths — by hand and from the
// catalog — are the SAME control, and the operator confirms before anything is written. That is how the
// GUI works: its catalog Add fills the add form rather than creating anything.
func (m *App) openInlineSpecForm(src *mcpforms.InlineSpec, isEdit bool) {
	s := m.scope
	if s == nil || s.kind != ownerWorkerVersion {
		// REFUSE OUT LOUD. A silent return here is the worst possible outcome: the caller (another form's
		// submit, or a message that arrived after the scope changed) is cleared either way, so the
		// operator sees a modal that closes having added nothing — indistinguishable from a broken key.
		m.dock.SetError("no worker version is open — a version's MCP specs are edited from its own scope")
		return
	}
	title := "Add an inline MCP spec"
	seed := mcpforms.InlineSpec{}
	if src != nil {
		seed = *src
	}
	if isEdit {
		title = "Edit MCP spec: " + src.ID
	}
	editID := ""
	if isEdit {
		editID = src.ID
	}
	f := mcpforms.InlineForm(title, &seed, func(next mcpforms.InlineSpec) {
		if editID != "" {
			for i := range s.inline {
				if s.inline[i].ID == editID {
					s.inline[i] = next
					break
				}
			}
		} else {
			// Same id = same spec: replace rather than append a duplicate.
			replaced := false
			for i := range s.inline {
				if s.inline[i].ID == next.ID {
					s.inline[i], replaced = next, true
					break
				}
			}
			if !replaced {
				s.inline = append(s.inline, next)
			}
		}
		s.dirty = true
	})
	f.Width = m.modalWidth()
	m.convScopeForm = f
}

// scopeEditSelected acts on the focused row: an MCP definition opens its edit form, a skill path opens
// the path list. `enter` and `e` are the same act (the picker's own idiom: enter does the obvious
// thing), and neither is destructive.
func (m *App) scopeEditSelected() tea.Cmd {
	row, ok := m.scope.selected(m)
	if !ok {
		m.dock.SetError("nothing selected — ↑/↓ moves through the scope")
		return nil
	}
	switch row.kind {
	case scopeRowMCP:
		for _, s := range m.scope.mcp {
			if s.GetId() == row.id {
				m.openScopeMCPDefine(s)
				return nil
			}
		}
		m.dock.SetError(row.name + " is no longer in this scope — r re-reads it")
		return nil
	case scopeRowInlineMCP:
		for i := range m.scope.inline {
			if m.scope.inline[i].ID == row.id {
				m.openInlineSpecForm(&m.scope.inline[i], true)
				return nil
			}
		}
		m.dock.SetError(row.name + " is no longer in this version — r re-reads it")
		return nil
	case scopeRowInheritedMCP:
		m.dock.SetError(row.name + " belongs to the project and is read-only here — edit it on the project")
		return nil
	case scopeRowSkill, scopeRowProjectSkill:
		return m.scopeEditSkills()
	}
	m.dock.SetError("enter/e opens a definition or the skill-file list — ↑/↓ moves to one")
	return nil
}

// scopeEditSkills opens the typed path list, PREFILLED with what this scope holds — the TUI's control
// for the skill_files array (the GUI browses with a file tree; the TUI types a path list, the same
// deliberate asymmetry context_files already has). The form is prefilled so a removal is an edit rather
// than a retype, and an emptied field clears the list.
func (m *App) scopeEditSkills() tea.Cmd {
	s := m.scope
	if s == nil {
		return nil
	}
	save := m.scopeSkillsSaver()
	if save == nil {
		m.dock.SetError("no skill-file target for this surface")
		return nil
	}
	f := mcpforms.SkillPathsForm(strings.Join(m.scopeSkillPaths(), "\n"), save)
	f.Width = m.modalWidth()
	m.convScopeForm = f
	return nil
}

// scopeSkillPaths is what this scope currently carries, so the path form opens prefilled.
func (m *App) scopeSkillPaths() []string {
	s := m.scope
	switch s.kind {
	case ownerProject:
		return m.projectSkillFiles(s.projectID)
	case ownerWorkerVersion:
		return s.skills
	}
	return m.conversationSkillFiles(s.convID)
}

// scopeSkillsSaver is the write for this scope's skill list — the ONE place the three targets differ.
func (m *App) scopeSkillsSaver() func([]string) tea.Cmd {
	s := m.scope
	switch s.kind {
	case ownerProject:
		projectID, cl := s.projectID, m.clients
		return func(paths []string) tea.Cmd {
			return func() tea.Msg {
				if cl == nil || cl.Projects == nil {
					return convScopeMsg{op: "/scope", err: "not connected to a plane"}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				// skill_files is an `optional ContextFiles` (field-mask semantics), so an empty list is
				// sent as an EMPTY ContextFiles rather than omitted — that is what clears it.
				if _, err := cl.Projects.UpdateProject(ctx, connect.NewRequest(&apiv1.UpdateProjectRequest{
					Id:         projectID,
					SkillFiles: &apiv1.ContextFiles{Files: paths},
				})); err != nil {
					return convScopeMsg{op: "/scope", err: err.Error()}
				}
				msg := "skill files updated for this project"
				if len(paths) == 0 {
					msg = "skill files cleared for this project"
				}
				return convScopeMsg{op: "/scope", detail: msg}
			}
		}
	case ownerWorkerVersion:
		return func(paths []string) tea.Cmd {
			s.skills = paths
			return m.commitWorkerVersion()
		}
	}
	convID := s.convID
	return func(paths []string) tea.Cmd { return m.chat.SetConversationSkillFiles(convID, paths) }
}

// commitWorkerVersion writes the modal's in-memory edit back through the version's own save.
//
// IT MERGES INTO THE BASE PERMISSIONS, never replaces them: the modal OWNS the mcp_servers key of that
// blob, and the version's other keys (tools, model_providers, …) must survive the edit. The same rule
// mcpforms.MergeIntoPermissions documents for the form fields.
func (m *App) commitWorkerVersion() tea.Cmd {
	s := m.scope
	if s == nil || s.kind != ownerWorkerVersion || s.saveInline == nil {
		return nil
	}
	perm, err := mcpforms.MarshalInline(s.permBase, s.inline)
	if err != nil {
		m.dock.SetError("could not encode the version's MCP specs: " + err.Error())
		return nil
	}
	s.permBase = perm // keep the base in step, so a second edit merges from the first
	return s.saveInline(perm, skillFilesJSONForPaths(s.skills))
}

// decodeSkillFilesJSON reads a version's skill_files JSON array into a path list. A malformed or empty
// value yields nil, so a version with no skills opens on an empty list rather than on a parse error.
func decodeSkillFilesJSON(raw string) []string {
	t := strings.TrimSpace(raw)
	if t == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(t), &out); err != nil {
		return nil
	}
	return out
}

// skillFilesJSONForPaths encodes a path list as the JSON array the wire expects (the same shape
// execution.skillFilesJSON writes from the field, so the two cannot disagree).
func skillFilesJSONForPaths(paths []string) string {
	b, err := json.Marshal(paths)
	if err != nil || len(paths) == 0 {
		return "[]"
	}
	return string(b)
}

// scopeInstallSelected runs the explicit auto-install for the focused definition (the GUI's Install
// button; never implicit at session time).
func (m *App) scopeInstallSelected() tea.Cmd {
	row, ok := m.scopeTarget("install", "row")
	if !ok {
		return nil
	}
	m.convScopeForm = mcpforms.InstallForm(row.name, m.conversationInstaller(row.id, row.name))
	m.convScopeForm.Width = m.modalWidth()
	return nil
}

// scopeCredentialForSelected opens the write-only credential form for the focused definition. The
// secrets store is tenant-scoped (RLS needs a tenant) — a credential store, not an MCP scope — which is
// why the key is typed here rather than derived from the definition.
func (m *App) scopeCredentialForSelected() tea.Cmd {
	row, ok := m.scopeTarget("credential", "credential")
	if !ok {
		return nil
	}
	// AN INLINE SPEC HAS NO ROW TO STORE AGAINST, so its credential is attached to the SPEC.
	if row.kind == scopeRowInlineMCP {
		m.openInlineCredentialForm(row)
		return nil
	}
	m.convScopeForm = mcpforms.SecretForm(row.name, "", m.conversationSecretSetter(row.id, row.name))
	m.convScopeForm.Width = m.modalWidth()
	return nil
}

// ── attaching a STORED credential to an inline spec ──────────────────────────────────────
//
// The operator, looking at a worker version's spec: "the credentials should be able to be selected
// rather than typed just like it does for projects and conversations. I understand that it is inline
// but that doesn't mean that info can't be built on top and passed to the config."
//
// Both halves of that are here. SELECTED: the form's secret field is a picker over the tenant secrets
// store, so the operator chooses a name that EXISTS — which is also the only shape the reference can
// take, because an unset ${NAME} fails that server's session at resolve time
// (mcpsettings.ResolveSecretRefs). BUILT ON TOP OF THE SPEC: the pick becomes `KEY=${NAME}` in the
// spec's own env (stdio) or headers (http), and the version is saved through its own write.
//
// IT IS TWO WRITES AND THEY ARE ORDERED. When the operator asks for a NEW secret (a name the store
// does not hold, with a value), the store write goes first and the version second: a version written
// first would carry a reference nothing resolves, and the operator would only find out when the
// worker ran. The in-memory spec is therefore changed on the MESSAGE the store returns (see
// onCredentialAttached), never before it — the same "a form's submit hands a message onward" rule the
// catalog prefill follows.

// openInlineCredentialForm opens the credential form for one of THIS version's inline specs.
func (m *App) openInlineCredentialForm(row scopeRow) {
	s := m.scope
	if s == nil || s.kind != ownerWorkerVersion {
		m.dock.SetError("no worker version is open — a version's specs are edited from its own scope")
		return
	}
	idx := inlineSpecIndex(s.inline, row.id)
	if idx < 0 {
		m.dock.SetError(row.name + " is no longer in this version — r re-reads it")
		return
	}
	spec := s.inline[idx]
	f := mcpforms.CredentialForm("Attach a credential to "+spec.ID, spec, s.secrets,
		func(key, secret, value string) tea.Cmd {
			return m.attachCredentialCmd(spec.ID, key, secret, value)
		})
	if s.secretsNote != "" {
		// SAID ON THE FORM, where the missing choices are, rather than as a dock error the operator has
		// to connect to an empty picker.
		f.Note += "\n\n" + s.secretsNote
	}
	f.Width = m.modalWidth()
	m.convScopeForm = f
}

// attachCredentialCmd stores a NEW credential when one is asked for, then hands the attach onward.
//
// THE STORE WRITE IS THE FIRST LINK and a failure stops the chain with the version untouched. The
// secret NAME is not validated here: the store's own rule (secrets.ValidateName) is the source of
// truth and it answers with its own message, which reaches the operator as the store's rejection —
// the same reasoning the Control screen's secret form documents.
func (m *App) attachCredentialCmd(specID, key, secret, value string) tea.Cmd {
	cl := m.clients
	label := ""
	// The choices are SNAPSHOT here, at submit: they are what the form offered, and the store write must
	// target the row the operator was shown rather than whatever a later read would return.
	var choices []mcpforms.SecretChoice
	if m.scope != nil {
		label, choices = m.scope.label, m.scope.secrets
	}
	return func() tea.Msg {
		msg := credentialAttachedMsg{scopeLabel: label, specID: specID, key: key, secret: secret}
		if value == "" {
			// The form has already established that this name is IN the store (a name that is not,
			// without a value, never reaches here) — so there is nothing to write yet.
			return msg
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := storeTenantSecret(ctx, cl, choices, secret, value); err != nil {
			msg.err = err.Error()
			return msg
		}
		msg.stored = true
		return msg
	}
}

// credentialAttachedMsg reports a credential that is safe to reference (it was already stored, or it
// has just been). IT IS A MESSAGE, NOT A DIRECT CALL, because the store write is asynchronous and the
// in-memory edit must not happen until it has landed — and because the form's own submit is the only
// thing that can hand it onward (see the router's form-guard note).
type credentialAttachedMsg struct {
	// scopeLabel names the surface it belongs to, so a reply that lands after the operator opened a
	// DIFFERENT worker version is discarded rather than applied to the wrong spec.
	scopeLabel string
	specID     string
	key        string
	secret     string
	stored     bool
	err        string
}

// onCredentialAttached applies the attach: change the spec, save the version.
func (m *App) onCredentialAttached(msg credentialAttachedMsg) tea.Cmd {
	s := m.scope
	if s == nil || s.kind != ownerWorkerVersion || s.label != msg.scopeLabel {
		return nil
	}
	if msg.err != "" {
		// NOTHING WAS TOUCHED — the spec still holds what it held, so the version and the spec agree.
		m.dock.SetError("/scope: the credential was not stored: " + msg.err + " — the spec is unchanged")
		return nil
	}
	idx := inlineSpecIndex(s.inline, msg.specID)
	if idx < 0 {
		m.dock.SetError(msg.specID + " is no longer in this version — r re-reads it")
		return nil
	}
	mcpforms.AttachSecret(&s.inline[idx], msg.key, msg.secret)
	if msg.stored {
		m.dock.SetNotice("/scope: stored " + msg.secret + " and pointed " + msg.key + " at it — saving the version")
	} else {
		m.dock.SetNotice("/scope: " + msg.key + " now reads ${" + msg.secret + "} — saving the version")
	}
	return m.commitWorkerVersion()
}

// inlineSpecIndex finds one inline spec by id (-1 when it is gone).
func inlineSpecIndex(specs []mcpforms.InlineSpec, id string) int {
	for i := range specs {
		if specs[i].ID == id {
			return i
		}
	}
	return -1
}

// storeTenantSecret creates — or replaces — a credential in the tenant secrets store under the name
// the operator chose. The name IS the reference (${NAME}), so the two can never disagree.
func storeTenantSecret(ctx context.Context, cl *client.Clients, choices []mcpforms.SecretChoice, name, value string) error {
	if cl == nil || cl.Secrets == nil {
		return fmt.Errorf("not connected to a plane")
	}
	for _, c := range choices {
		if c.Name == name {
			if _, err := cl.Secrets.UpdateSecret(ctx, connect.NewRequest(
				&apiv1.UpdateSecretRequest{Id: c.ID, Value: &value})); err != nil {
				return err
			}
			return nil
		}
	}
	if _, err := cl.Secrets.CreateSecret(ctx, connect.NewRequest(&apiv1.CreateSecretRequest{
		Name: name, Value: value,
		Description: "MCP server credential (attached to a worker version's inline spec)",
	})); err != nil {
		return err
	}
	return nil
}

// listSecretChoices reads the tenant secrets store's NAMES (never values — GetSecret is not called)
// for the credential picker, with a note explaining an unreadable or partial list.
func listSecretChoices(ctx context.Context, cl *client.Clients) ([]mcpforms.SecretChoice, string) {
	if cl == nil || cl.Secrets == nil {
		return nil, "not connected to a plane, so nothing can be listed to pick from"
	}
	res, err := cl.Secrets.ListSecrets(ctx, connect.NewRequest(&apiv1.ListSecretsRequest{PageSize: 200}))
	if err != nil {
		return nil, "the tenant secrets store could not be read (" + err.Error() + ") — a name that is " +
			"not listed can still be given a value to store it"
	}
	out := make([]mcpforms.SecretChoice, 0, len(res.Msg.GetSecrets()))
	for _, s := range res.Msg.GetSecrets() {
		out = append(out, mcpforms.SecretChoice{ID: s.GetId(), Name: s.GetName(), Description: s.GetDescription()})
	}
	note := ""
	if res.Msg.GetNextPageToken() != "" {
		note = "the store holds more secrets than are listed here — give a name a value to store or " +
			"replace it"
	}
	return out, note
}

// scopeSecretsMsg carries the store's names back to the modal (`r`, and the open path's own read).
type scopeSecretsMsg struct {
	scopeLabel string
	secrets    []mcpforms.SecretChoice
	note       string
}

// loadScopeSecrets re-reads the tenant secrets store for the modal's credential picker.
func (m *App) loadScopeSecrets() tea.Cmd {
	label, cl := "", m.clients
	if m.scope != nil {
		label = m.scope.label
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		choices, note := listSecretChoices(ctx, cl)
		return scopeSecretsMsg{scopeLabel: label, secrets: choices, note: note}
	}
}

// onScopeSecrets applies a store read. A reply for a surface this modal is no longer about is
// DISCARDED rather than applied.
func (m *App) onScopeSecrets(msg scopeSecretsMsg) {
	if m.scope == nil || m.scope.kind != ownerWorkerVersion || m.scope.label != msg.scopeLabel {
		return
	}
	m.scope.secrets, m.scope.secretsNote = msg.secrets, msg.note
}

// reloadScope re-reads what the CURRENT scope's rows are built from.
//
// A WORKER VERSION HAS NO ROWS TO RE-READ, but its credential picker does: the tenant secrets store is
// a live thing the operator may have just written to from the Control screen, and `r` is advertised as
// the refresh — a picker that silently refuses a secret created a minute ago would read as broken.
func (m *App) reloadScope() tea.Cmd {
	if m.scope != nil && m.scope.kind == ownerWorkerVersion {
		return m.loadScopeSecrets()
	}
	return m.loadScope()
}

// scopeDeleteSelected removes the focused row's thing: an owned definition, an inline spec, or one skill
// path.
//
// ALL ARE CONFIRMED, through the shell's own confirm dialog, even though the GUI deletes a definition on
// a single button press: `d` sits one key from the movement keys in a list the operator is scanning, and
// the write is not undoable from here. The dialog NAMES what goes and what does not, which is the part a
// bare "are you sure" would leave out.
func (m *App) scopeDeleteSelected() tea.Cmd {
	row, ok := m.scopeTarget("delete", "writable")
	if !ok {
		return nil
	}
	s := m.scope
	switch row.kind {
	case scopeRowMCP:
		where := "this conversation"
		if s.kind == ownerProject {
			where = "this project"
		}
		m.openBulkConfirm("Delete this MCP definition?",
			row.name+" will be removed from "+where+"'s scope.\n"+
				"The definition belongs to "+where+" only — other scopes are untouched.",
			"delete", func() tea.Cmd { return m.scopeDeleteMCP(row.id, row.name) })
		return nil
	case scopeRowInlineMCP:
		// THE COPY SAYS WHAT ACTUALLY HAPPENS. The modal commits to the version as soon as the change is
		// made (there is no outer "Save" on a shell-hosted modal), so promising "not saved until you save
		// the version" would be a lie about a write that has already gone out.
		m.openBulkConfirm("Remove this MCP spec from the version?",
			row.name+" will be removed from this worker version's permissions and SAVED immediately.\n"+
				"The version's other permissions are untouched.",
			"remove", func() tea.Cmd {
				kept := make([]mcpforms.InlineSpec, 0, len(s.inline))
				for _, spec := range s.inline {
					if spec.ID != row.id {
						kept = append(kept, spec)
					}
				}
				s.inline = kept
				return m.commitWorkerVersion()
			})
		return nil
	case scopeRowSkill:
		remaining := withoutPath(m.scopeSkillPaths(), row.id)
		save := m.scopeSkillsSaver()
		m.openBulkConfirm("Remove this skill file?",
			row.id+" will no longer be rendered into "+s.ownerName()+"'s prompt.\n"+
				"The file itself is untouched; s shows the remaining list.",
			"remove", func() tea.Cmd { return save(remaining) })
		return nil
	}
	return nil
}

// scopeDeleteMCP deletes an OWNED definition at the modal's current scope, by ID.
func (m *App) scopeDeleteMCP(id, name string) tea.Cmd {
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return convScopeMsg{op: "/scope", err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := cl.MCP.DeleteMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerDeleteRequest{Id: id})); err != nil {
			return convScopeMsg{op: "/scope", err: err.Error()}
		}
		return convScopeMsg{op: "/scope", detail: "deleted " + name}
	}
}

// withoutPath removes one entry from a path list.
func withoutPath(paths []string, drop string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != drop {
			out = append(out, p)
		}
	}
	return out
}

// --- the view ---------------------------------------------------------------

// scopeView composes the modal over the base view, inside the same SOLID panel every other shell modal
// uses (see modalPanel: a modal must not be anything but opaque).
func (m *App) scopeView(base string, w, h int) string {
	if m.scope == nil {
		return base
	}
	return m.overlayCentered(base, m.modalPanel(m.scopeBody(), m.modalWidth()))
}

// scopeBody renders the modal's lines, windowed so a long scope never overflows the terminal.
func (m *App) scopeBody() string {
	s := m.scope
	inner := m.modalInnerWidth()
	rows := s.rows(m)
	s.clampCursor(len(rows))

	var b strings.Builder
	b.WriteString(theme.ListTitle.Render(truncateRight(m.scopeTitle(), inner)) + "\n")
	if s.loading {
		b.WriteString(theme.HintText.Render("loading…") + "\n")
	}
	if s.err != "" {
		b.WriteString(theme.ErrorText.Render(truncateRight("⚠ "+s.err, inner)) + "\n")
	}

	// Keep the CURSOR in view: the rows above it are what the operator is choosing between, so the
	// window follows the selection rather than the scroll.
	lo, hi := m.scopeWindow(rows, len(rows))
	for i := lo; i < hi; i++ {
		row := rows[i]
		switch row.kind {
		case scopeRowSection:
			// A blank row before each heading, so the sections read as blocks.
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(theme.DetailKey.Render(truncateRight(row.text, inner)) + "\n")
		case scopeRowNote:
			b.WriteString(theme.HintText.Render(truncateRight("  "+row.text, inner)) + "\n")
		default:
			// The cursor IS the marker: a selected row is drawn with the app's selection style, so a
			// second glyph would only compete with the highlight the operator already reads.
			line := truncateRight("  "+row.text, inner)
			if i == s.cursor {
				b.WriteString(theme.ListItemSelected.Render(line) + "\n")
				continue
			}
			b.WriteString(theme.ListItem.Render(line) + "\n")
		}
	}
	if hi < len(rows) {
		b.WriteString(theme.HintText.Render(truncateRight("  … more rows below", inner)) + "\n")
	}
	// THE HINT WRAPS, it is never truncated — see scopeHintItems. Its final verb (esc: close) is the one
	// an operator reaches for when the verbs above it did not do what they expected, so it is the last
	// thing that may be cut.
	for _, line := range wrapHintItems(m.scopeHintItems(), inner) {
		b.WriteString(theme.HintText.Render(line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// scopeWindow returns the [lo,hi) slice of rows to draw, following the cursor.
func (m *App) scopeWindow(rows []scopeRow, n int) (int, int) {
	if n == 0 {
		return 0, 0
	}
	// The modal is sized to its content, so the budget is the terminal minus the panel's chrome (a
	// border, the title, the hint and a couple of rows of margin).
	budget := m.height - 8
	if budget < 4 {
		budget = 4
	}
	if n <= budget {
		m.scope.scroll = 0
		return 0, n
	}
	if m.scope.cursor < m.scope.scroll {
		m.scope.scroll = m.scope.cursor
	}
	if m.scope.cursor >= m.scope.scroll+budget {
		m.scope.scroll = m.scope.cursor - budget + 1
	}
	if max := n - budget; m.scope.scroll > max {
		m.scope.scroll = max
	}
	if m.scope.scroll < 0 {
		m.scope.scroll = 0
	}
	return m.scope.scroll, m.scope.scroll + budget
}

// scopeTitle names the surface the modal is managing — one line, so the operator always knows WHICH
// scope a destructive verb would touch.
func (m *App) scopeTitle() string {
	s := m.scope
	if s == nil {
		return "Manage MCP + skills"
	}
	switch s.kind {
	case ownerProject:
		name := s.label
		if name == "" {
			name = "this project"
		}
		return "MCP + skills — project " + name
	case ownerWorkerVersion:
		name := s.label
		if name == "" {
			name = "this version"
		}
		return "MCP + skills — " + name
	}
	return "MCP + skills — this conversation"
}

// scopeHintItems is the modal's key list as ITEMS, and the items are why the hint can WRAP.
//
// The operator: "the shortcut advice is also a bit cut off". It was rendered through truncateRight,
// which cuts to ONE line — so the verbs at the end (the ones the operator needs named, because this
// line is the only place they exist) were the first thing lost. Items let it break on its own
// separators instead, so nothing is ever dropped.
// scopeHintItems is the modal's key list for the CURRENT row: the verbs that apply to it, plus the ones
// that apply everywhere.
//
// THE ROW-INDEPENDENT VERBS ARE ADDED ONCE, AT THE END, AND THAT IS A FIX. Writing each case as a
// complete list meant the row-specific cases could silently DROP a global verb — and the MCP row's case
// did exactly that, omitting `c: catalog`. The operator, looking at a definition on a project:
// "The mcp add screen does not have a shortcut helper or button to add MCPs from the catalog." The
// catalog is reachable from any row (it does not act on the selection), so it must be advertised from
// any row; deriving the list that way makes the omission impossible rather than fixing this instance.
//
// AN INLINE SPEC OFFERS NEITHER `c` NOR `i`/`k`: a worker version's specs are its own, so there is no row
// for a catalog add to create, nothing to install, and nothing to attach a credential to. That is the
// GUI's own omission for this scope, and the row's note says why.
func (m *App) scopeHintItems() []string {
	row, ok := m.scope.selected(m)

	var items []string
	switch {
	case ok && row.kind == scopeRowMCP:
		items = []string{"↑/↓ move", "enter/e: edit", "i: install", "k: credential", "d: delete"}
	case ok && row.kind == scopeRowInlineMCP:
		// `k: credential` IS OFFERED HERE, and `i: install` is not: a credential is built into the
		// spec's own env/headers (so it needs no row), while an install status can only live on one.
		items = []string{"↑/↓ move", "enter/e: edit", "k: credential", "d: remove"}
	case ok && (row.kind == scopeRowInheritedMCP || row.kind == scopeRowProjectSkill):
		items = []string{"↑/↓ move", "read-only (from the project)"}
	case ok && row.kind == scopeRowSkill:
		items = []string{"↑/↓ move", "enter/e: edit list", "d: remove"}
	}

	// The global verbs. `a` (add by hand) exists at every scope — a version adds an inline spec, the
	// others add an owned row — and `s` (skill files) likewise.
	// `c: catalog` APPLIES AT EVERY SCOPE, including a worker version: the catalog PREFILLS the add form
	// and the form's save writes whichever target the scope has (a row, or an inline spec). The verbs that
	// genuinely need a ROW — install and credential — are omitted by the row cases above, where they
	// belong, rather than by a scope-wide flag here.
	items = append(items, "a: add", "c: catalog", "s: skill files", "r: refresh", "esc: close")
	return items
}

// scopeHint is the hint on one line (the single-line consumers).
func (m *App) scopeHint() string { return strings.Join(m.scopeHintItems(), " · ") }

// wrapHintItems packs the items into lines of at most width cells, joined with the " · " the hint has
// always used. An item WIDER than the whole line is hard-split rather than truncated: a cut-off key
// label is the defect being fixed, and losing one is worse than an ugly break.
func wrapHintItems(items []string, width int) []string {
	const sep = " · "
	if len(items) == 0 {
		return nil
	}
	if width <= 0 {
		return []string{strings.Join(items, sep)}
	}
	var out []string
	cur := ""
	for _, it := range items {
		for len([]rune(it)) > width {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			out = append(out, string([]rune(it)[:width]))
			it = string([]rune(it)[width:])
		}
		switch {
		case cur == "":
			cur = it
		case len([]rune(cur))+len([]rune(sep))+len([]rune(it)) <= width:
			cur += sep + it
		default:
			out = append(out, cur)
			cur = it
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// --- the conversation-scope hooks -------------------------------------------

// scopeModalOpen reports whether the scope modal is showing (the shell reads it to decide key routing
// and to close it when the surface under it changes).
func (m *App) scopeModalOpen() bool { return m.scope != nil }

// scopeConversation is the conversation the modal is about ("" when closed).
func (m *App) scopeConversation() string {
	if m.scope == nil {
		return ""
	}
	return m.scope.convID
}

// scopeConversationChanged closes the modal when the open conversation is no longer the one it is
// about. It is the shell's guard rather than a call at each switch site, so a future switch path cannot
// forget it — the failure it prevents is a modal editing one chat's scope while another is open.
func (m *App) scopeConversationChanged() {
	if m.scope != nil && m.scope.kind == ownerConversation && m.scope.convID != m.chatConvID {
		m.closeScopeModal()
	}
}

// ── the shell surface a SCREEN opens the modal through ────────────────────────────────────
//
// The modal is the SHELL's (it is layered over every screen), so a screen reaches it through these two
// calls rather than owning a second copy of it. The GUI has the same shape: MCPServersPanel is one
// component imported by the project page, the worker page and the conversation disclosure.

// OpenProjectMCPModal opens the MCP + skills modal for a project — the surface the Work screen's
// Projects pane opens on `m`, matching the panel the GUI mounts on a project page.
func (m *App) OpenProjectMCPModal(projectID, name string) tea.Cmd {
	return m.openProjectScopeModal(projectID, name)
}

// workerVersionScopeMsg carries a worker's version data into the modal. The fetch is asynchronous, so
// the modal is opened on the MESSAGE rather than inside the cmd (a cmd mutates a discarded App copy).
type workerVersionScopeMsg struct {
	workerID    string
	workerName  string
	versionID   string
	version     int32
	permissions string
	skillFiles  string
	// secrets (and the note explaining an unreadable or partial list) ride along because THIS scope's
	// credential form needs something to pick from, and this is the one read the open path already
	// makes — a second round trip after `k` would make the picker feel broken rather than ready.
	secrets     []mcpforms.SecretChoice
	secretsNote string
	err         string
}

// OpenWorkerMCPModal opens the MCP + skills modal for a worker's version — the surface the Execution
// screen's Workers pane opens on `m`, matching the panel the GUI mounts on a worker page.
//
// IT FETCHES FIRST, because the list row carries neither the version id (which the update needs) nor the
// version's permissions and skill files (which are what the modal manages). One read of the worker, then
// the modal owns an in-memory copy until the operator saves.
func (m *App) OpenWorkerMCPModal(workerID, workerName string) tea.Cmd {
	if workerID == "" {
		m.dock.SetError("no worker selected")
		return nil
	}
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.Workers == nil {
			return workerVersionScopeMsg{err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.Workers.GetWorker(ctx, connect.NewRequest(&apiv1.GetWorkerRequest{Id: workerID}))
		if err != nil {
			return workerVersionScopeMsg{err: err.Error()}
		}
		msg := workerVersionScopeMsg{workerID: workerID, workerName: workerName}
		v := resp.Msg.GetLatestVersion()
		if v == nil {
			// NO VERSION YET: nothing to manage, and saying so is better than opening an empty modal whose
			// save would have no version to write to.
			msg.err = workerName + " has no version yet — a worker's MCP and skills live on a version, " +
				"so create one first (V on the Workers pane)"
			return msg
		}
		msg.versionID, msg.version = v.GetId(), v.GetVersion()
		msg.permissions = v.GetPermissions()
		msg.skillFiles = skillFilesJSONForPaths(v.GetSkillFiles())
		// A FAILED STORE READ IS NOT A FAILED OPEN: the version is what the operator came to edit, and
		// the credential form still works by name + value. The note is how the picker explains itself.
		msg.secrets, msg.secretsNote = listSecretChoices(ctx, cl)
		return msg
	}
}

// onWorkerVersionScope opens the modal once a worker's version has landed.
func (m *App) onWorkerVersionScope(msg workerVersionScopeMsg) tea.Cmd {
	if msg.err != "" {
		m.dock.SetError(msg.err)
		return nil
	}
	label := msg.workerName
	if label == "" {
		label = msg.workerID
	}
	label = fmt.Sprintf("%s · v%d", label, msg.version)
	cmd := m.openWorkerVersionScopeModal(label, msg.permissions, msg.skillFiles,
		m.workerVersionSaver(msg.workerID, msg.versionID, m.clients))
	if m.scope != nil && m.scope.kind == ownerWorkerVersion {
		m.scope.secrets, m.scope.secretsNote = msg.secrets, msg.secretsNote
	}
	return cmd
}

// workerVersionSaver is the write for a worker version's MCP specs and skill files, as a function of the
// worker, the version and the client set — so a test can drive the REAL request rather than a copy of it.
func (m *App) workerVersionSaver(workerID, versionID string, cl *client.Clients) func(permJSON, skillsJSON string) tea.Cmd {
	return func(permJSON, skillsJSON string) tea.Cmd {
		return func() tea.Msg {
			{
				if cl == nil || cl.Workers == nil {
					return convScopeMsg{op: "/scope", err: "not connected to a plane"}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				// PERMISSIONS AND SKILL FILES TOGETHER, in one update: they are both optional fields on
				// the request, and writing one without the other would leave the version's two halves
				// disagreeing about what the modal just showed.
				//
				// REPUBLISH IS WHAT MAKES THIS WORK ON A PUBLISHED VERSION, and leaving it unset was a
				// real bug: the server refuses a non-draft version to update ("status is \"published\",
				// must be 'draft'"), so the modal would have failed on exactly the versions an operator
				// is most likely to be editing. With it set, a PUBLISHED version takes the same
				// revert → update → republish flow the model gets from BulkUpdateWorkerModel, keeping the
				// version number and the published state. A DRAFT ignores the flag (the server only
				// consults it for a non-draft status), so it is edited in place and stays a draft.
				//
				// The operator's question is what surfaced it: "You can add models to a worker without
				// modifying a version. Why can't we add MCP servers like that as well?" The model path
				// edits the version row too (model_ref is a column there) — it just has a dedicated RPC
				// that does revert→set→republish for you, which is why it never looks like a version edit.
				if _, err := cl.Workers.UpdateWorkerVersion(ctx, connect.NewRequest(&apiv1.UpdateWorkerVersionRequest{
					WorkerId:    workerID,
					VersionId:   versionID,
					Permissions: &permJSON,
					SkillFiles:  &skillsJSON,
					Republish:   true,
				})); err != nil {
					return convScopeMsg{op: "/scope", err: err.Error()}
				}
				return convScopeMsg{op: "/scope", detail: "version saved — MCP specs and skill files updated"}
			}
		}
	}
}
