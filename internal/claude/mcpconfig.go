package claude

// mcpconfig.go — claude's MCP wiring: the built-in Orchicon tools, and the
// mechanism for any other MCP server.
//
// # Why this was missing, and why it mattered
//
// Every other transport has an MCP path:
//
//   - the NATIVE adapter gets the tools in-process AND reads the tenant's MCP
//     settings (server.go: nativeBridge.SetConfigSource(mcpsettings…));
//   - OPENCODE gets a built-in Orchicon sidecar plus the operator's own opencode
//     MCP config (internal/opencode/config.go: orchiconMCPServer / readMCPServers).
//
// claude had NEITHER, so an Ask session could not call a single `orchicon_*`
// tool. The operator's Opus 5 said so in its own words: "I tried to read it with
// orchicon_get_current_conversation and that tool isn't reachable from this
// session — the orchicon_* MCP surface isn't mounted right now". It could not
// create work items or drive runs either.
//
// # The mechanism, verified against the real CLI rather than inferred
//
// claude takes `--mcp-config <json-or-file>` (repeatable) and merges it with its
// other MCP sources; `--strict-mcp-config` is what would SUPPRESS the others, so
// we deliberately do not pass it — which is also why an operator's own
// `claude mcp add` servers keep working alongside ours.
//
// Verified by running the real binary with the config this file builds:
//
//	mcp_servers: [{"name":"orchicon","status":"connected","source":"dynamic"}]
//	tools: 111, of which 87 are mcp__orchicon__*  (including get_current_conversation)
//
// The tool naming is `mcp__<server>__<toolName>` — which is load-bearing for the
// MODE GATE: a policy that names `create_work_item` does not match
// `mcp__orchicon__create_work_item`, so the mapping in modegate.go handles the
// prefix or the boundary silently does nothing.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
)

// MCPBinaryContainerPath is where the runtime daemon bind-mounts its own
// executable inside EVERY runtime container (internal/runtime/daemon.go — the
// daemon hard-fails if it cannot). It is the ONLY orchicon path guaranteed to
// exist in a container: the plane's own os.Executable() is a host path. Same
// constant the worker hook uses, and the same convention opencode's
// runtimeContainerBinaryPath follows.
const MCPBinaryContainerPath = HookBinaryContainerPath

// MCPTenantEnv / MCPWorkflowRunEnv / the conversation pair are read by the
// `orchicon mcp` sidecar (internal/mcp/server.go): the tenant its DB channel is
// scoped to, the workflow run whose run_context it injects into create calls,
// and — for an Ask session — the conversation whose tools it is serving. Each
// is restored onto the tool call's context by the sidecar, because a child
// process inherits an ENVIRONMENT, never a Go context.
const (
	MCPTenantEnv      = "ORCHICON_MCP_TENANT_ID"
	MCPWorkflowRunEnv = "ORCHICON_MCP_WORKFLOW_RUN_ID"

	// MCPConversationEnv / MCPConversationProjectEnv carry an Ask session's
	// conversation scope across the stdio boundary.
	//
	// THEY MUST BE SET ONLY BY A TRANSPORT WHOSE MCP CHILD SERVES ONE CONVERSATION.
	// claude's Ask child qualifies: it is spawned per conversation and its
	// `--mcp-config` is fixed at spawn, so the scope cannot go stale under it. A
	// SHARED serve must NOT set them — one process serving several conversations
	// would stamp whichever conversation happened to launch it onto every other
	// conversation's tool calls, which is worse than the empty scope it replaces.
	MCPConversationEnv        = "ORCHICON_MCP_CONVERSATION_ID"
	MCPConversationProjectEnv = "ORCHICON_MCP_CONVERSATION_PROJECT_ID"
)

// OrchiconMCPConversationEnv builds the extra environment that carries an Ask
// conversation's scope into the Orchicon sidecar.
//
// WHY THE SIDECAR NEEDS THIS AT ALL. The turn's conversation and project are a
// CONTEXT value (askmode.ConversationScope), stamped in-process by the transport
// before it dispatches the turn. That reaches a tool fine when the tools run
// IN-PROCESS — the native Ask path — and cannot cross into a stdio child, so
// every Orchicon tool in a claude Ask session saw an unstamped context and
// get_current_conversation correctly refused to name a model it could not
// identify: "no conversation is stamped on this turn". The scope rides the
// environment for the same reason the tenant does.
//
// An empty conversation id yields nil (no extra env): a worker or plane sidecar
// has no conversation, and inventing one would be the confidently-wrong answer
// that tool exists to refuse. A conversation with no project omits only the
// project var — the sidecar then stamps the scope's own meaning for "", which is
// "this conversation is assigned to no project", not "unset".
func OrchiconMCPConversationEnv(conversationID, projectID string) map[string]string {
	if strings.TrimSpace(conversationID) == "" {
		return nil
	}
	env := map[string]string{MCPConversationEnv: conversationID}
	if strings.TrimSpace(projectID) != "" {
		env[MCPConversationProjectEnv] = projectID
	}
	return env
}

// MCPServer is one entry in claude's `mcpServers` map.
//
// Command/Args/Env describe a stdio server (what the Orchicon sidecar is);
// URL/Headers describe a remote one. The zero combination is dropped rather than
// emitted, so a half-built entry cannot become a server claude tries and fails
// to start — a failed MCP server is not a warning in claude, it is a startup it
// waits on.
type MCPServer struct {
	Name string

	// stdio transport.
	Command string
	Args    []string
	Env     map[string]string

	// remote transport.
	URL     string
	Headers map[string]string
}

// usable reports whether the entry has enough to be startable.
func (s MCPServer) usable() bool {
	if strings.TrimSpace(s.Name) == "" {
		return false
	}
	if strings.TrimSpace(s.Command) != "" {
		return true
	}
	return strings.TrimSpace(s.URL) != ""
}

// toJSON renders the entry in claude's schema.
func (s MCPServer) toJSON() map[string]any {
	if strings.TrimSpace(s.URL) != "" {
		out := map[string]any{"type": "http", "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
		return out
	}
	out := map[string]any{"command": s.Command}
	if len(s.Args) > 0 {
		out["args"] = s.Args
	}
	if len(s.Env) > 0 {
		out["env"] = s.Env
	}
	return out
}

// MCPConfigJSON renders servers as the document `--mcp-config` accepts:
// {"mcpServers": {name: {…}}}. Unusable entries are skipped, and an empty set
// returns "" so a caller can simply omit the flag (rather than pass an empty
// server map, which claude would parse and find nothing in).
//
// NAME COLLISIONS RESOLVE TO THE LATER ENTRY, deliberately. The built-in Orchicon
// server is emitted FIRST and the operator's resolved set after it, so an
// operator who configures their own entry named `orchicon` gets THEIRS — the
// same rule opencode applies ("unless the user already defines one named
// `orchicon` — respect their explicit choice"). A tenant that configures their
// own connection should not have it silently overridden by ours, and a
// duplicate entry would otherwise start two servers under one name.
func MCPConfigJSON(servers []MCPServer) string {
	entries := map[string]any{}
	for _, s := range servers {
		if !s.usable() {
			continue
		}
		entries[strings.TrimSpace(s.Name)] = s.toJSON()
	}
	if len(entries) == 0 {
		return ""
	}
	b, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return ""
	}
	return string(b)
}

// OrchiconMCPServer builds the built-in Orchicon entry: this binary's `mcp`
// subcommand over stdio, scoped to a tenant.
//
// `binary` is the orchicon executable to run, and it DIFFERS BY TRANSPORT: on
// the host it is this process's own executable (HookBinaryPath), while inside a
// runtime container it must be the daemon's bind mount
// (MCPBinaryContainerPath) — a host path there is a server that cannot start.
//
// `extraEnv` carries the transport's own additions (a sandbox DSN in a runtime
// container, a workflow-run id). The tenant is always set: without it the
// sidecar falls back to the dev tenant and a worker would read the wrong
// tenant's work items.
func OrchiconMCPServer(binary, tenantID string, extraEnv map[string]string) MCPServer {
	env := map[string]string{}
	for k, v := range extraEnv {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			env[k] = v
		}
	}
	if t := strings.TrimSpace(tenantID); t != "" {
		env[MCPTenantEnv] = t
	}
	return MCPServer{
		Name:    orchiconMCPServerName,
		Command: binary,
		Args:    []string{"mcp"},
		Env:     env,
	}
}

// MCPSecretResolver resolves ${SECRET_NAME} references in a spec's env/headers to
// plaintext. Declared here so the bridge does not import the storage layer that
// implements it — the same reason the native bridge declares its own.
type MCPSecretResolver func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error)

// MCPServersFromSpecs renders the NEUTRAL server specs into claude's entries.
//
// THIS IS THE ADAPTER-SPECIFIC HALF, AND IT IS THE ONLY ONE. The resolution —
// which servers an execution gets (the project-owned ∪ the scope's own
// definitions) and the ${SECRET_NAME} → plaintext expansion — is shared
// and lives in internal/mcpsettings + internal/mcpclient. An adapter supplies
// only this: a function from []mcpclient.ServerSpec to its own config format.
// Adding codex means adding a renderer beside this one, not another resolution
// stack, which is what keeps the surface standard as adapters multiply.
//
// A spec's Command is an ARGV SLICE; claude wants `command` plus `args`, so the
// head and tail are split. A URL spec becomes claude's typed http entry.
func MCPServersFromSpecs(specs []mcpclient.ServerSpec) []MCPServer {
	out := make([]MCPServer, 0, len(specs))
	for _, sp := range specs {
		if strings.TrimSpace(sp.ID) == "" {
			continue
		}
		m := MCPServer{Name: sp.ID}
		if u := strings.TrimSpace(sp.URL); u != "" {
			m.URL = u
			m.Headers = sp.Headers
		} else if len(sp.Command) > 0 {
			m.Command = sp.Command[0]
			m.Args = append([]string(nil), sp.Command[1:]...)
			m.Env = sp.Env
		}
		if m.usable() {
			out = append(out, m)
		}
	}
	return out
}

// MCPArgs returns the `--mcp-config` argv fragment for a server set, or nil when
// there is nothing usable to register.
func MCPArgs(servers []MCPServer) []string {
	cfg := MCPConfigJSON(servers)
	if cfg == "" {
		return nil
	}
	return []string{"--mcp-config", cfg}
}
