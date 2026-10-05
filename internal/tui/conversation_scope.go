package tui

// conversation_scope.go — the CONVERSATION scope's WRITES: the RPCs behind the scope modal
// (scope_modal.go), reachable exactly where the conversation's other per-conversation controls live:
// the slash surface.
//
// IT IS THE SAME PLACEMENT RULE AS THE GUI. There, the chat header carries mode / model / fullsend
// and hosts a disclosure beside SessionGrants; here, /mode, /models, /fullsend and /project already
// set the conversation's per-conversation state, so /scope (and the /mcp and /skills names that open
// the same modal) joins them rather than inventing a second navigation. The capability is identical
// and only the control differs (the documented asymmetry: the GUI browses skill files in a file tree,
// the TUI types a path list — the same treatment context_files already gets).
//
// THE MCP HALF USES screens/mcpforms, THE ONE TUI MCP SURFACE, with Owner{ConversationID}. A
// conversation owns its definitions exactly as a project does (mcp_servers.conversation_id); the
// OWNER COLUMN IS THE SELECTION, which is what replaced the removed tenant-level select-from-list
// field. Nothing here writes a tenant-level entry, and there is no tenant list to write one into.
//
// EVERY WRITE HERE RESOLVES ITS TARGET BY ID. The old subcommand grammar addressed a definition BY
// NAME, so each verb re-listed the conversation's definitions to turn a name into a row — a round trip
// per keystroke, and a second place for "which definition did they mean?" to be answered. The modal
// holds the rows, so it knows the id, and these take it.

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	connect "connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
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

// newConversationMCPCatalogForm lists the curated registry and, on a pick, creates the entry owned by
// THIS conversation — the TUI's one-click add, mirroring the GUI's Registry catalog grid. It is the
// conversation-scoped twin of the Work screen's project form, over the same mcpforms.CatalogForm.
func (m *App) newConversationMCPCatalogForm() *kit2.Form {
	convID := m.chatConvID
	cl := m.clients
	save := func(req *apiv1.MCPServerCreateRequest) tea.Cmd {
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
	return mcpforms.CatalogForm(mcpforms.Owner{ConversationID: convID},
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

// deleteConversationMCP deletes a definition by ID — the row the modal had selected.
func (m *App) deleteConversationMCP(id, name string) tea.Cmd {
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

// conversationSecretSetter writes a credential for a definition, by ID. The credential store is
// tenant-scoped (RLS needs a tenant); that is a credential store, not an MCP scope.
func (m *App) conversationSecretSetter(id, name string) func(key, value string) tea.Cmd {
	cl := m.clients
	return func(key, value string) tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/scope", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.MCP.SetMCPServerSecret(ctx, connect.NewRequest(&apiv1.MCPServerSetSecretRequest{
				Id: id, Name: key, Value: value,
			})); err != nil {
				return convScopeMsg{op: "/scope", err: err.Error()}
			}
			return convScopeMsg{op: "/scope", detail: "stored credential " + key + " for " + name}
		}
	}
}

// conversationInstaller installs the runtime for a definition, by ID.
func (m *App) conversationInstaller(id, name string) func() tea.Cmd {
	cl := m.clients
	return func() tea.Cmd {
		return func() tea.Msg {
			if cl == nil || cl.MCP == nil {
				return convScopeMsg{op: "/scope", err: "not connected to a plane"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.MCP.InstallMCPRuntime(ctx, connect.NewRequest(&apiv1.MCPServerInstallRequest{Id: id})); err != nil {
				return convScopeMsg{op: "/scope", err: err.Error()}
			}
			return convScopeMsg{op: "/scope", detail: "installing " + name}
		}
	}
}

// applyConversationSkills is the DIRECT-WRITE half of /skills: the operator typed the path list, so it
// is written without a modal. The control and the SAME server-side validation: the CLIENTSIDE
// deliberately validates nothing (contextfiles is the one validator, at the render boundary), so a
// rejection surfaces from the server as a plain error rather than a second, drifting copy of the rule.
//
// "clear" is the empty list, and an empty ARGUMENT list is refused rather than treated as a clear —
// clearing is destructive enough to deserve the word, and bare `/skills` opens the modal (where the
// paths and a `d` per path are visible).
func (m *App) applyConversationSkills(args []string) tea.Cmd {
	convID := m.chatConvID
	if len(args) == 1 && strings.EqualFold(args[0], "clear") {
		return m.chat.SetConversationSkillFiles(convID, nil)
	}
	files := mcpforms.ParseSkillPaths(strings.Join(args, " "))
	if len(files) == 0 {
		m.dock.SetError("usage: /skills <path>[, <path>…] | /skills clear — bare /skills opens the scope")
		return nil
	}
	return m.chat.SetConversationSkillFiles(convID, files)
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
		// AN IN-MEMORY EDIT COMMITS WHEN ITS FORM CLOSES. A worker version's specs live in its own
		// permissions, and mcpforms.InlineForm's save callback cannot return a command — so this is the
		// first point at which the modal can write the version back. Gated on `dirty`, so a form
		// dismissed without a change is not a write.
		if m.scope != nil && m.scope.kind == ownerWorkerVersion && m.scope.dirty {
			m.scope.dirty = false
			cmd = tea.Batch(cmd, m.commitWorkerVersion())
		}
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
