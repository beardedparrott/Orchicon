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
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// scopeRowKind is what a modal row IS, which decides both how it draws and what the keys do to it.
type scopeRowKind int

const (
	scopeRowSection      scopeRowKind = iota // a section heading (not selectable)
	scopeRowMCP                              // a definition THIS conversation owns (selectable, actionable)
	scopeRowInheritedMCP                     // a definition the PROJECT owns (selectable to read the note, not writable)
	scopeRowSkill                            // a skill path THIS conversation carries (selectable, removable)
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
type scopeModal struct {
	// convID is the conversation this modal is ABOUT. The fetch result is discarded when it does not
	// match, so a reply that lands after a conversation switch cannot scope the wrong chat.
	convID string
	// mcp and inherited are this conversation's owned definitions and the project's (read-only).
	mcp       []*apiv1.MCPServer
	inherited []*apiv1.MCPServer
	// loading is true until the first fetch lands, and err carries a failed one.
	loading bool
	err     string
	// cursor is the index into rows() of the SELECTED row (movement skips non-selectable rows).
	cursor int
	// scroll is the first drawn row, so a long scope never overflows the terminal.
	scroll int
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
	m.scope = &scopeModal{convID: m.chatConvID, loading: true}
	// The project's skill files are part of the union this modal shows, and the rail's project list is
	// where they live — a cold rail would render the inherited half as empty.
	cmd := tea.Cmd(m.loadScope())
	if len(m.railProjects) == 0 && m.clients != nil {
		cmd = tea.Batch(cmd, m.loadRailProjects())
	}
	return cmd
}

// closeScopeModal closes it. Called when the surface under it changes identity (a different
// conversation), NOT on every repaint: the modal is the shell's, and it belongs to one conversation —
// the GUI closes its Scope disclosure on a conversation switch for exactly this reason
// (ConversationScopeDisclosure: "Switching conversation closes it").
func (m *App) closeScopeModal() {
	m.scope = nil
}

// loadScope fetches the conversation's owned definitions and the project's, for the read-only half.
func (m *App) loadScope() tea.Cmd {
	if m.scope == nil {
		return nil
	}
	convID := m.scope.convID
	cl := m.clients
	projectID := m.conversationProjectID(convID)
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return scopeDataMsg{convID: convID, err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		mine, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
			ConversationId: convID,
		}))
		if err != nil {
			return scopeDataMsg{convID: convID, err: err.Error()}
		}
		out := scopeDataMsg{convID: convID, mcp: mine.Msg.GetServers()}
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
func (s *scopeModal) rows(m *App) []scopeRow {
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
		return m, m.loadScope()
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
	switch needs {
	case "mcp":
		switch row.kind {
		case scopeRowMCP:
			return row, true
		case scopeRowInheritedMCP:
			m.dock.SetError(row.name + " belongs to project " +
				m.projectLabelFor(m.conversationProjectID(m.scope.convID)) +
				" — edit it there; this scope only consumes it")
			return scopeRow{}, false
		}
		m.dock.SetError(verb + " applies to an MCP server — ↑/↓ moves to one (this row is " + scopeRowKindWord(row.kind) + ")")
		return scopeRow{}, false
	case "writable":
		switch row.kind {
		case scopeRowMCP, scopeRowSkill:
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
	case scopeRowNote:
		return "a note"
	}
	return "another kind of row"
}

// scopeAddMCP opens the typed definition form for this conversation (mcpforms.DefineForm, owner-stamped
// with the conversation) — the same control the GUI's Add opens.
func (m *App) scopeAddMCP() tea.Cmd {
	m.openConversationMCPDefine(nil)
	return nil
}

// scopeAddMCPFromCatalog opens the registry picker, which prefills the same definition form — the TUI's
// one-click add, mirroring the GUI's Registry catalog grid.
func (m *App) scopeAddMCPFromCatalog() tea.Cmd {
	m.convScopeForm = m.newConversationMCPCatalogForm()
	m.convScopeForm.Width = m.modalWidth()
	return nil
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
				m.openConversationMCPDefine(s)
				return nil
			}
		}
		m.dock.SetError(row.name + " is no longer in this scope — r re-reads it")
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

// scopeEditSkills opens the typed path list, PREFILLED with what the conversation holds — the TUI's
// control for the skill_files array (the GUI browses with a file tree; the TUI types a path list, the
// same deliberate asymmetry context_files already has). The form is prefilled so a removal is an edit
// rather than a retype, and an emptied field clears the list (which is what /skills clear does).
func (m *App) scopeEditSkills() tea.Cmd {
	convID := m.chatConvID
	if m.scope != nil {
		convID = m.scope.convID
	}
	if convID == "" {
		m.dock.SetError("no conversation open — /skills applies to one conversation")
		return nil
	}
	current := m.conversationSkillFiles(convID)
	f := mcpforms.SkillPathsForm(strings.Join(current, "\n"), func(paths []string) tea.Cmd {
		return m.chat.SetConversationSkillFiles(convID, paths)
	})
	f.Width = m.modalWidth()
	m.convScopeForm = f
	return nil
}

// scopeInstallSelected runs the explicit auto-install for the focused definition (the GUI's Install
// button; never implicit at session time).
func (m *App) scopeInstallSelected() tea.Cmd {
	row, ok := m.scopeTarget("install", "mcp")
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
	row, ok := m.scopeTarget("credential", "mcp")
	if !ok {
		return nil
	}
	m.convScopeForm = mcpforms.SecretForm(row.name, "", m.conversationSecretSetter(row.id, row.name))
	m.convScopeForm.Width = m.modalWidth()
	return nil
}

// scopeDeleteSelected removes the focused row's thing: an owned definition, or one skill path.
//
// BOTH ARE CONFIRMED, through the shell's own confirm dialog, even though the GUI deletes a definition
// on a single button press: `d` sits one key from the movement keys in a list the operator is scanning,
// and the write is not undoable from here. The dialog NAMES what goes and what does not, which is the
// part a bare "are you sure" would leave out.
func (m *App) scopeDeleteSelected() tea.Cmd {
	row, ok := m.scopeTarget("delete", "writable")
	if !ok {
		return nil
	}
	switch row.kind {
	case scopeRowMCP:
		m.openBulkConfirm("Delete this MCP definition?",
			row.name+" will be removed from this conversation's scope.\n"+
				"The definition belongs to the conversation only — the project's and any worker's are untouched.",
			"delete", func() tea.Cmd { return m.deleteConversationMCP(row.id, row.name) })
		return nil
	case scopeRowSkill:
		remaining := withoutPath(m.conversationSkillFiles(m.scope.convID), row.id)
		m.openBulkConfirm("Remove this skill file?",
			row.id+" will no longer be rendered into this conversation's prompt.\n"+
				"The file itself is untouched; s shows the remaining list.",
			"remove", func() tea.Cmd {
				return m.chat.SetConversationSkillFiles(m.scope.convID, remaining)
			})
		return nil
	}
	return nil
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
	b.WriteString(theme.ListTitle.Render(truncateRight("Scope — this conversation", inner)) + "\n")
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
	b.WriteString(theme.HintText.Render(truncateRight(m.scopeHint(), inner)))
	return b.String()
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

// scopeHint is the modal's key line. EVERY verb is named — that is the whole repair: the old surface
// made the operator learn /mcp's five subcommands from a usage string that did not fit on screen.
func (m *App) scopeHint() string {
	row, ok := m.scope.selected(m)
	verbs := "a: add · c: catalog · s: skill files · r: refresh · esc: close"
	switch {
	case ok && (row.kind == scopeRowMCP):
		verbs = "enter/e: edit · i: install · k: credential · d: delete · a: add · s: skill files · esc: close"
	case ok && (row.kind == scopeRowInheritedMCP || row.kind == scopeRowProjectSkill):
		verbs = "read-only (from the project) · a: add · s: skill files · esc: close"
	case ok && row.kind == scopeRowSkill:
		verbs = "enter/e: edit list · d: remove · s: skill files · a: add MCP · esc: close"
	}
	return "↑/↓ move · " + verbs
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
	if m.scope != nil && m.scope.convID != m.chatConvID {
		m.closeScopeModal()
	}
}
