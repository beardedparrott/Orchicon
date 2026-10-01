package tui

// conversation_scope.go — the CONVERSATION scope's MCP definitions and skill files, reachable
// exactly where the conversation's other per-conversation controls live: the slash surface.
//
// IT IS THE SAME PLACEMENT RULE AS THE GUI. There, the chat header carries mode / model / fullsend
// and hosts a disclosure beside SessionGrants; here, /mode, /models, /fullsend and /project already
// set the conversation's per-conversation state, so /mcp and /skills join them rather than inventing
// a second navigation. The capability is identical and only the control differs (the documented
// asymmetry: the GUI browses skill files in a file tree, the TUI types a path list — the same
// treatment context_files already gets).
//
// THE MCP HALF USES screens/mcpforms, THE ONE TUI MCP SURFACE, with Owner{ConversationID}. A
// conversation owns its definitions exactly as a project does (mcp_servers.conversation_id); the
// OWNER COLUMN IS THE SELECTION, which is what replaced the removed tenant-level select-from-list
// field. Nothing here writes a tenant-level entry, and there is no tenant list to write one into.

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	connect "connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/mcpforms"
)

// convScopeMsg reports the outcome of a conversation-scope write so the shell can surface it. It is
// a message rather than a direct dock call because the RPC runs in a tea.Cmd, where mutating the App
// touches a copy the runtime has already discarded (see the model-picker note in models.go).
type convScopeMsg struct {
	op     string
	detail string
	err    string
}

// convScopePrefillMsg carries a fetched definition into the edit form. The fetch is asynchronous, so
// the form is opened on the message rather than inside the cmd for the reason above.
type convScopePrefillMsg struct {
	server *apiv1.MCPServer
	err    string
}

// listConversationMCP reports the OPEN conversation's owned definitions.
func (m *App) listConversationMCP() tea.Cmd {
	convID := m.chatConvID
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
			ConversationId: convID,
		}))
		if err != nil {
			return convScopeMsg{op: "/mcp", err: err.Error()}
		}
		servers := resp.Msg.GetServers()
		if len(servers) == 0 {
			return convScopeMsg{op: "/mcp", detail: "no definitions owned by this conversation — /mcp define adds one (a definition belongs to exactly one scope)"}
		}
		parts := make([]string, 0, len(servers))
		for _, s := range servers {
			loc := s.GetCommand()
			if loc == "" {
				loc = s.GetUrl()
			}
			parts = append(parts, s.GetName()+" ("+strings.ToLower(s.GetTransport().String())+" · "+loc+")")
		}
		return convScopeMsg{op: "/mcp", detail: strings.Join(parts, ", ")}
	}
}

// conversationMCPCommand dispatches the /mcp subcommands against the OPEN conversation.
func (m *App) conversationMCPCommand(args []string) tea.Cmd {
	sub := "list"
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	name := strings.TrimSpace(strings.Join(args[1:], " "))
	switch sub {
	case "list":
		return m.listConversationMCP()
	case "define", "add", "new":
		m.openConversationMCPDefine(nil)
		return nil
	case "edit":
		if name == "" {
			m.dock.SetError("usage: /mcp edit <name>")
			return nil
		}
		return m.fetchConversationMCPForEdit(name)
	case "delete", "rm":
		if name == "" {
			m.dock.SetError("usage: /mcp delete <name>")
			return nil
		}
		return m.deleteConversationMCP(name)
	case "secret":
		if name == "" {
			m.dock.SetError("usage: /mcp secret <name>")
			return nil
		}
		m.convScopeForm = mcpforms.SecretForm(name, "", m.conversationSecretSetter(name))
		m.convScopeForm.Width = m.modalWidth()
		return nil
	case "install":
		if name == "" {
			m.dock.SetError("usage: /mcp install <name>")
			return nil
		}
		m.convScopeForm = mcpforms.InstallForm(name, m.conversationInstaller(name))
		m.convScopeForm.Width = m.modalWidth()
		return nil
	default:
		m.dock.SetError("unknown /mcp subcommand " + sub + " — use define | edit <name> | delete <name> | secret <name> | install <name>")
		return nil
	}
}

// openConversationMCPDefine opens the typed DEFINITION form for the conversation. With prefill set
// it EDITS that definition (owner echo on the update); with nil it defines a new one.
//
// This is AC 9's "the TUI can define an entry": the form builds command/args/env or url/headers with
// ${SECRET_NAME} support, owner-stamped by mcpforms — never a selection from a tenant list.
func (m *App) openConversationMCPDefine(prefill *apiv1.MCPServer) {
	convID := m.chatConvID
	cl := m.clients
	owner := mcpforms.Owner{ConversationID: convID}
	title := "Define an MCP server for this conversation"
	var seed *apiv1.MCPServerCreateRequest
	if prefill != nil {
		title = "Edit MCP server: " + prefill.GetName()
		seed = serverAsCreate(prefill)
	}
	f := mcpforms.DefineForm(title, owner, seed, func(req *apiv1.MCPServerCreateRequest) tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if prefill != nil {
				// EDIT: the scope is immutable, so the owner rides along as an ECHO — a differing echo
				// is rejected by the server, which is what stops an edit silently re-scoping a row.
				up := &apiv1.MCPServerUpdateRequest{
					Id:             prefill.GetId(),
					ConversationId: convID,
					Command:        strPtr(req.GetCommand()),
					ReplaceArgs:    boolPtr(true),
					Args:           req.GetArgs(),
					Env:            req.GetEnv(),
					Url:            strPtr(req.GetUrl()),
					Headers:        req.GetHeaders(),
					Enabled:        boolPtr(req.GetEnabled()),
				}
				if _, err := cl.MCP.UpdateMCPServer(ctx, connect.NewRequest(up)); err != nil {
					return convScopeMsg{op: "/mcp", err: err.Error()}
				}
				return convScopeMsg{op: "/mcp", detail: "updated " + req.GetName()}
			}
			if _, err := cl.MCP.CreateMCPServer(ctx, connect.NewRequest(req)); err != nil {
				return convScopeMsg{op: "/mcp", err: err.Error()}
			}
			return convScopeMsg{op: "/mcp", detail: "defined " + req.GetName() + " for this conversation"}
		}
	})
	f.Width = m.modalWidth()
	m.convScopeForm = f
}

// fetchConversationMCPForEdit resolves a definition BY NAME, then opens the edit form on it.
func (m *App) fetchConversationMCPForEdit(name string) tea.Cmd {
	convID := m.chatConvID
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
			ConversationId: convID,
		}))
		if err != nil {
			return convScopeMsg{op: "/mcp", err: err.Error()}
		}
		for _, s := range resp.Msg.GetServers() {
			if s.GetName() == name {
				return convScopePrefillMsg{server: s}
			}
		}
		return convScopeMsg{op: "/mcp", err: "this conversation owns no definition named " + name}
	}
}

// deleteConversationMCP resolves a definition by name and deletes it.
func (m *App) deleteConversationMCP(name string) tea.Cmd {
	convID := m.chatConvID
	cl := m.clients
	return func() tea.Msg {
		if cl == nil || cl.MCP == nil {
			return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
			ConversationId: convID,
		}))
		if err != nil {
			return convScopeMsg{op: "/mcp", err: err.Error()}
		}
		for _, s := range resp.Msg.GetServers() {
			if s.GetName() != name {
				continue
			}
			if _, err := cl.MCP.DeleteMCPServer(ctx, connect.NewRequest(&apiv1.MCPServerDeleteRequest{Id: s.GetId()})); err != nil {
				return convScopeMsg{op: "/mcp", err: err.Error()}
			}
			return convScopeMsg{op: "/mcp", detail: "deleted " + name}
		}
		return convScopeMsg{op: "/mcp", err: "this conversation owns no definition named " + name}
	}
}

// conversationSecretSetter writes a credential for a name-resolved definition. The credential store
// is tenant-scoped (RLS needs a tenant); that is a credential store, not an MCP scope.
func (m *App) conversationSecretSetter(name string) func(key, value string) tea.Cmd {
	convID := m.chatConvID
	cl := m.clients
	return func(key, value string) tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resp, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
				ConversationId: convID,
			}))
			if err != nil {
				return convScopeMsg{op: "/mcp", err: err.Error()}
			}
			for _, s := range resp.Msg.GetServers() {
				if s.GetName() != name {
					continue
				}
				if _, err := cl.MCP.SetMCPServerSecret(ctx, connect.NewRequest(&apiv1.MCPServerSetSecretRequest{
					Id: s.GetId(), Name: key, Value: value,
				})); err != nil {
					return convScopeMsg{op: "/mcp", err: err.Error()}
				}
				return convScopeMsg{op: "/mcp", detail: "stored credential " + key + " for " + name}
			}
			return convScopeMsg{op: "/mcp", err: "this conversation owns no definition named " + name}
		}
	}
}

// conversationInstaller installs the runtime for a name-resolved definition.
func (m *App) conversationInstaller(name string) func() tea.Cmd {
	convID := m.chatConvID
	cl := m.clients
	return func() tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/mcp", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resp, err := cl.MCP.ListMCPServers(ctx, connect.NewRequest(&apiv1.MCPServerListRequest{
				ConversationId: convID,
			}))
			if err != nil {
				return convScopeMsg{op: "/mcp", err: err.Error()}
			}
			for _, s := range resp.Msg.GetServers() {
				if s.GetName() != name {
					continue
				}
				if _, err := cl.MCP.InstallMCPRuntime(ctx, connect.NewRequest(&apiv1.MCPServerInstallRequest{Id: s.GetId()})); err != nil {
					return convScopeMsg{op: "/mcp", err: err.Error()}
				}
				return convScopeMsg{op: "/mcp", detail: "installing " + name}
			}
			return convScopeMsg{op: "/mcp", err: "this conversation owns no definition named " + name}
		}
	}
}

// openConversationSkills applies /skills. The path-list control and the SAME server-side validation:
// the CLIENTSIDE deliberately validates nothing (contextfiles is the one validator, at the render
// boundary), so a rejection surfaces from the server as a plain error rather than a second, drifting
// copy of the rule.
func (m *App) openConversationSkills(args []string) tea.Cmd {
	convID := m.chatConvID
	switch {
	case len(args) == 0:
		return func() tea.Msg {
			files := m.conversationSkillFiles(m.chatConvID)
			if len(files) == 0 {
				return convScopeMsg{op: "/skills", detail: "no skill files on this conversation — /skills <paths> sets them (comma- or newline-separated)"}
			}
			return convScopeMsg{op: "/skills", detail: strings.Join(files, ", ")}
		}
	case strings.EqualFold(args[0], "clear"):
		return m.chat.SetConversationSkillFiles(convID, nil)
	default:
		files := mcpforms.ParseSkillPaths(strings.Join(args, " "))
		if len(files) == 0 {
			m.dock.SetError("usage: /skills <path>[, <path>…] | /skills clear")
			return nil
		}
		return m.chat.SetConversationSkillFiles(convID, files)
	}
}

// conversationSkillFiles reads the open conversation's skill files from the rail (the list the
// server computed), so the report is the SERVER's answer rather than a local guess.
func (m *App) conversationSkillFiles(id string) []string {
	for _, c := range m.conversations {
		if c.ID == id {
			return c.SkillFiles
		}
	}
	return nil
}

// --- the modal host -----------------------------------------------------

// convScopeKey drives the conversation-scope modal. It OWNS every key while it is up, exactly like
// the rename modal, so a save chord cannot land in the composer behind it.
func (m *App) convScopeKey(k tea.KeyMsg) (*App, tea.Cmd) {
	if m.convScopeForm == nil {
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.convScopeForm = nil
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	}
	cmd, _ := m.convScopeForm.HandleKey(k)
	if m.convScopeForm != nil && m.convScopeForm.Submitted {
		m.convScopeForm = nil
	}
	return m, cmd
}

// convScopeView composes the modal over the base view, inside a SOLID panel.
func (m *App) convScopeView(base string, w, h int) string {
	if m.convScopeForm == nil {
		return base
	}
	m.convScopeForm.Width = m.modalInnerWidth()
	return m.overlayCentered(base, m.modalPanel(m.convScopeForm.View(), m.modalWidth()))
}

// --- helpers ------------------------------------------------------------

// serverAsCreate adapts an owned row into the create request the definition form prefill expects, so
// EDITING and DEFINING share one control (the GUI panel does the same with its FormState).
func serverAsCreate(s *apiv1.MCPServer) *apiv1.MCPServerCreateRequest {
	if s == nil {
		return nil
	}
	return &apiv1.MCPServerCreateRequest{
		Name:      s.GetName(),
		Transport: s.GetTransport(),
		Command:   s.GetCommand(),
		Args:      s.GetArgs(),
		Env:       s.GetEnv(),
		Url:       s.GetUrl(),
		Headers:   s.GetHeaders(),
		Enabled:   s.GetEnabled(),
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
