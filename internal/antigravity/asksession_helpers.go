package antigravity

// asksession_helpers.go — the pieces of an Ask conversation's ATTACH step that need the platform's own
// resolution stack (MCP servers, the system-prompt suffix, the extra readable roots) and agy's model
// catalogue.
//
// THEY LIVE BESIDE ask.go RATHER THAN IN IT because they are the parts that talk to the rest of the plane,
// and ask.go is the lifetime state machine. Splitting them keeps the registry, the turn loop and the
// re-attach decision readable in one file.

import (
	"context"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/askmode"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// askMCPBuiltin builds the platform's own sidecar entry for a CONVERSATION scope.
//
// IT IS THE SAME SIDECAR claude's Ask path registers, with the same env — including
// ORCHICON_MCP_CONVERSATION_ID, which is what scopes `orchicon_ask_user` to THIS conversation. Without that
// variable the sidecar's ask tool has no conversation to record an answer against, and the option card would
// render with nowhere to put the answer.
func (b *Bridge) askMCPBuiltin(tenantID, convID, projectID string) acp.MCPServer {
	extra := claude.OrchiconMCPConversationEnv(convID, projectID)
	builtin := claude.OrchiconMCPServer(claude.HookBinaryFor(false), tenantID, extra)
	return acpMCPServer(builtin)
}

// askMCPServers resolves and renders this conversation's MCP union, and returns its fingerprint.
//
// THE SCOPE IS ScopeConversation, the same ref claude's Ask path builds: the project's servers ∪ the
// conversation's own, never a worker's version-scoped inline set.
//
// THE OFFERED HALF OF THE MODE RULE is applied here (withholdOpaqueACPMCP): a mode that may not act is not
// shown the operator's servers at all, so the model cannot call a tool whose result the permission handler
// would have to refuse. The handler's gate is the ENFORCED half; this keeps the two agreeing.
func (s *askSession) askMCPServers(ctx context.Context, tenantID, projectID, mode string) ([]acp.MCPServer, string, error) {
	builtin := s.b.askMCPBuiltin(tenantID, s.convID, projectID)
	res, err := s.b.resolveMCP(ctx, tenantID, mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeConversation,
		ProjectID:      projectID,
		ConversationID: s.convID,
	})
	servers := []acp.MCPServer{builtin}
	provenance := mcpclient.ProvenanceString(res.Servers)
	if err != nil {
		// A RESOLUTION FAILURE IS NOT FATAL: the conversation still gets the platform's own sidecar, which
		// is what makes orchicon_ask_user work at all. Logged, because an MCP surface that quietly came up
		// empty is otherwise invisible — the model just reports it cannot call a tool.
		s.b.askLog().Warn("antigravity ask: MCP resolution failed — only the built-in Orchicon server is registered",
			"conversation", s.convID, "error", err)
		return servers, askMCPFingerprint(askmode.MayAct(mode), keysOfMCPServers(servers)), nil
	}
	rendered, rerr := s.b.renderMCP(ctx, tenantID, res, builtin)
	if rerr != nil {
		s.b.askLog().Warn("antigravity ask: MCP render failed — only the built-in Orchicon server is registered",
			"conversation", s.convID, "error", rerr)
		return servers, askMCPFingerprint(askmode.MayAct(mode), keysOfMCPServers(servers)), nil
	}
	servers = rendered
	mayAct := askmode.MayAct(mode)
	if !mayAct {
		before := len(servers)
		servers = withholdOpaqueACPWire(servers)
		if len(servers) < before {
			s.b.askLog().Info("antigravity ask: the mode may not act — the operator's MCP servers are not offered this turn",
				"conversation", s.convID, "mode", mode, "withheld", provenance)
		}
	}
	logMCPResolution("ask", servers, provenance)
	return servers, askMCPFingerprint(mayAct, keysOfMCPServers(servers)), nil
}

// withholdOpaqueACPWire applies the withhold rule to the WIRE entries, keeping the platform's own sidecar.
//
// IT WRAPS withholdOpaqueACPMCP so the rule itself is stated exactly once (askmodegate.go): a second copy of
// "which server is the platform's own" is a second place for the two to disagree, and the disagreement would
// silently withhold orchicon_ask_user from a planning conversation.
func withholdOpaqueACPWire(servers []acp.MCPServer) []acp.MCPServer {
	keep := make(map[string]bool, 1)
	for _, k := range withholdOpaqueACPMCP(keysOfMCPServers(servers)) {
		keep[k.Name] = true
	}
	out := make([]acp.MCPServer, 0, len(keep))
	for _, s := range servers {
		if keep[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// keysOfMCPServers projects the wire entries onto the comparable view the fingerprint and the withhold rule
// read.
func keysOfMCPServers(servers []acp.MCPServer) []acpMCPServerKey {
	out := make([]acpMCPServerKey, 0, len(servers))
	for _, s := range servers {
		k := acpMCPServerKey{
			Name: s.Name,
			URL:  s.URL,
		}
		if s.Command != "" {
			k.Command = s.Command
		}
		k.Args = append([]string(nil), s.Args...)
		// Env/Headers are the ACP {name,value} JSON list; the fingerprint only needs their TEXT, which is
		// stable because EnvList sorts by name. Comparing the JSON verbatim therefore compares the map.
		if len(s.Env) > 0 {
			k.Env = []string{string(s.Env)}
		}
		if len(s.Headers) > 0 {
			k.Headers = []string{string(s.Headers)}
		}
		out = append(out, k)
	}
	return out
}

// mcpDrifted reports whether the live child was launched with a different MCP set or mode authority.
//
// THE CHILD'S mcpServers ARE FIXED AT FORK/EXEC, so the only way to apply a new server, a rotated secret or a
// mode change that flips whether the operator's servers are offered at all is to start a NEW child and
// re-attach the SAME agy session id.
func (s *askSession) mcpDrifted(current, tenantID, projectID, mode string) bool {
	_ = current
	_, want := s.peekMCPFingerprint(tenantID, projectID, mode)
	s.mu.Lock()
	have := s.mcpFingerprint
	s.mu.Unlock()
	if strings.TrimSpace(have) == "" {
		return false
	}
	return have != want
}

// peekMCPFingerprint computes the fingerprint the desired set WOULD have, without launching anything. It
// intentionally never fails: a resolution error means "the sidecar only", which is still a comparable set.
func (s *askSession) peekMCPFingerprint(tenantID, projectID, mode string) ([]acp.MCPServer, string) {
	builtin := s.b.askMCPBuiltin(tenantID, s.convID, projectID)
	servers := []acp.MCPServer{builtin}
	res, err := s.b.resolveMCP(context.Background(), tenantID, mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeConversation,
		ProjectID:      projectID,
		ConversationID: s.convID,
	})
	if err == nil {
		if rendered, rerr := s.b.renderMCP(context.Background(), tenantID, res, builtin); rerr == nil {
			servers = rendered
		}
	}
	mayAct := askmode.MayAct(mode)
	if !mayAct {
		servers = withholdOpaqueACPWire(servers)
	}
	return servers, askMCPFingerprint(mayAct, keysOfMCPServers(servers))
}

// askSystemAppend builds the `_meta.antigravity.systemPrompt.append` text for an attach.
//
// IT CARRIES TWO THINGS and never the turn's own text: the platform's system seed (which a worker keeps in
// its manifest and a conversation only receives on its first turn), and the FIXED Ask suffix. The suffix is
// what tells the model two things it cannot learn from the tools themselves:
//
//  1. which mode it is in (agy has no per-mode tool policy to read), and
//  2. NEVER CALL `ask_question` — because agy's own multiple-choice tool is an ungated tool call with no ACP
//     answer channel (docs §3.5), so an answer to it can never reach the model. `orchicon_ask_user` is the
//     card that works.
func askSystemAppend(system string) string {
	var b strings.Builder
	if seed := strings.TrimSpace(system); seed != "" {
		b.WriteString(seed)
		b.WriteString("\n\n")
	}
	b.WriteString(claudeAskSuffix)
	return b.String()
}

// claudeAskSuffix is the fixed Ask instruction appended to every attach. It is deliberately FRAMED AS A
// RULE, not as advice: the first line is the one that stops a dead-end tool call.
const claudeAskSuffix = "=== ORCHICON ASK RULES ===\n" +
	"Never call `ask_question`. Its answer cannot be delivered back to you on this transport, so it ends " +
	"the turn with no reply. To ask the operator anything, call `orchicon_ask_user` with a question and its " +
	"options — that raises a card they answer, and the answer comes back to you in this same turn.\n" +
	"Reads are free. `write_to_file`, `replace_file_content`, `multi_replace_file_content` and " +
	"`run_command` raise a permission card: state what you are about to change, then make the call and wait " +
	"for the answer.\n" +
	"The conversation's working directory is where your files go. The conversation's project, when it has " +
	"one, is an additional root you may read and edit."

// askAdditionalDirs is the extra readable roots list for the attach: the conversation's project.
//
// THE ASK DIRECTORY IS THE CWD, so it is not repeated here; agy refuses a duplicate and the list is what the
// UI reports as "this conversation can see these roots".
func (s *askSession) askAdditionalDirs(projectDir, askDir string) []string {
	p := strings.TrimSpace(projectDir)
	if p == "" {
		return nil
	}
	if strings.TrimSpace(askDir) == p {
		return nil
	}
	return []string{p}
}

// checkModelInCatalogue pre-validates the model against agy's advertised catalogue, and refuses loudly.
//
// AN UNKNOWN MODEL NEVER FALLS BACK: the operator chose it, and silently running a different one is a false
// claim about the conversation.
func checkModelInCatalogue(state *acp.SessionState, id string) error {
	if state == nil || len(state.Models) == 0 {
		// A build that advertises no catalogue is trusted to refuse the model itself on SetModel.
		return nil
	}
	for _, m := range state.Models {
		if strings.EqualFold(m.ModelID, id) {
			return nil
		}
	}
	return fmt.Errorf("antigravity ask: the model %q is not offered by agy for this account (available: %s)",
		id, availableModels(state))
}

// isSessionNotFound reports whether an attach failure means "agy no longer knows this session id", which is
// the ONE failure that should ask the platform to recreate the conversation rather than fail the turn.
func isSessionNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"session not found", "unknown session", "no such session", "not found"} {
		if strings.Contains(msg, want) {
			return true
		}
	}
	return false
}
