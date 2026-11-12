package askorchicon

// tool_askmcptools.go — "WHAT AM I OFFERED RIGHT NOW?", ANSWERABLE IN ONE CALL.

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// askMCPToolsView is the wire shape: the servers mounted for THIS turn, and every tool name the model can call.
type askMCPToolsView struct {
	// Conversation / Project are the scope the answer was resolved for, so a surprising result can be traced to
	// the scope it came from rather than to the tool.
	Conversation string `json:"conversation,omitempty"`
	Project      string `json:"project,omitempty"`
	// Servers is one entry per mounted server, with its provenance ("project:<id>" / "conversation:<id>") — the
	// same label the platform log prints, so a model and an operator read one vocabulary.
	Servers []askMCPToolsServer `json:"servers"`
	// Tools is EVERY callable name, including ones this mode hides.
	Tools []string `json:"tools"`
	// Mode is the mode the filter was applied with, and Hosted lists names that exist but were not offered.
	Mode   string   `json:"mode,omitempty"`
	Hosted []string `json:"hosted_by_a_refused_mode,omitempty"`
	// Note explains an empty answer, which is the case that sent the reported session down its detour.
	Note string `json:"note,omitempty"`
}

// askMCPToolsServer is one mounted server.
type askMCPToolsServer struct {
	// ID is the server's identity AND the prefix of every tool name it supplies (mcp__<id>__<tool>), which is
	// what lets a model map a tool back to the server that owns it.
	ID         string `json:"id"`
	Provenance string `json:"provenance,omitempty"`
	// Tools is how many of this server's tools were discovered — 0 means it connected and advertised nothing,
	// which is a different problem from not connecting at all.
	Tools int `json:"tools"`
}

// IT IS READ-ONLY AND NON-MUTATING, and it is deliberately allowed in EVERY mode: a mode that may
// not ACT can still tell the operator what it can see.
func toolAskMCPTools(ctx context.Context, svc *Service) (json.RawMessage, error) {
	view := askMCPToolsView{
		Conversation: askConversationFromContext(ctx),
		Project:      askConversationProjectFromContext(ctx),
		Mode:         askModeFromContext(ctx),
		Servers:      []askMCPToolsServer{},
		Tools:        []string{},
	}

	entry, err := svc.askMCPFor(ctx)
	if err != nil {
		// THE ERROR IS THE ANSWER. A resolution or connect failure is exactly what the model needs to report,
		// and swallowing it here would recreate the silence being fixed.
		view.Note = "the conversation's MCP servers could not be started: " + err.Error()
		return json.Marshal(view)
	}
	if entry == nil || entry.mgr == nil {
		view.Note = "this conversation resolves NO MCP servers (project and conversation scope are both empty, or " +
			"every definition is disabled — list_mcp_servers shows the definitions and their enabled flag). " +
			"There is nothing to call; do not attempt to drive an MCP server through a shell."
		return json.Marshal(view)
	}

	// COUNT PER SERVER from the discovered names. The names are mcp__<server>__<tool>, and ToolName owns that
	// grammar, so this counts by PREFIX rather than parsing the string itself.
	perServer := map[string]int{}
	var all []string
	for _, d := range entry.mgr.Defs() {
		all = append(all, d.Name)
		for _, s := range entry.servers {
			if hasToolPrefix(d.Name, s.Spec.ID) {
				perServer[s.Spec.ID]++
				break
			}
		}
	}
	for _, s := range entry.servers {
		view.Servers = append(view.Servers, askMCPToolsServer{
			ID:         s.Spec.ID,
			Provenance: s.FromID,
			Tools:      perServer[s.Spec.ID],
		})
	}

	// WHAT THE MODE OFFERS, using the SAME predicate the real tool surface uses, so this cannot claim a tool is
	// callable when the turn would refuse it.
	mode := view.Mode
	for _, name := range all {
		if ok, _ := modeAllowsTool(mode, name); ok {
			view.Tools = append(view.Tools, name)
		} else {
			view.Hosted = append(view.Hosted, name)
		}
	}
	sort.Strings(view.Tools)
	sort.Strings(view.Hosted)

	if len(view.Tools) == 0 {
		if len(view.Hosted) > 0 {
			view.Note = "every MCP tool this conversation mounts is withheld by the current mode — switch mode to use " +
				"them. They are real and mounted; they are not refused because they are missing."
		} else {
			view.Note = "the servers are mounted but advertise no tools. That is a server-side problem, not a missing " +
				"capability: check the server's own logs."
		}
	}
	return json.Marshal(view)
}

// hasToolPrefix reports whether name belongs to server id, using the shared naming grammar.
func hasToolPrefix(name, serverID string) bool {
	prefix := mcpclient.ToolName(serverID, "")
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}
