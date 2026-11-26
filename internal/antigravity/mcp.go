package antigravity

// mcp.go — THE OWNER-SCOPED MCP UNION, RENDERED AS ACP `mcpServers`.
//
// THE RESOLUTION IS NOT WRITTEN HERE. It is the platform's one resolution stack, the same one the native
// and claude bridges consume:
//
//	mcpclient.ScopeResolver            — the seam (ResolveScope)
//	mcpsettings.NewResolver(pool)      — the storage-backed implementation
//	mcpclient.Resolution               — the union + per-server provenance
//	mcpsettings.ResolveSecretRefs      — ${SECRET_NAME} → plaintext
//
// Only the RENDER differs per adapter, and that is deliberate: a second resolution stack would let the
// three bridges disagree about which servers an execution gets, which is precisely the drift
// internal/claude/mcpresolve.go exists to prevent.
//
// THE SERVERS GO ON THE WIRE, NEVER ON DISK. claude takes its servers as a `--mcp-config` JSON argument;
// ACP takes them as `mcpServers` in `session/new` / `session/resume`. What matters is what this file does
// NOT do: it never writes `~/.gemini/config/mcp_config.json` or a `.agents/mcp_config.json` in the
// worktree. Writing either would put an operator-scoped credential into a file on disk that a later run
// (or a screenshot of the workspace) can read, and the antigravity no-token-handling guarantee in
// notokens_test.go exists for the same reason.
//
// THE PLATFORM'S OWN SERVER IS ADDED ON TOP, NEVER INSTEAD: a tenant with no MCP servers configured must
// still get `orchicon_*`, otherwise a worker cannot call orchicon_ask_user or report a work item.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/antigravity/acp"
	"github.com/beardedparrott/orchicon/internal/claude"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// renderACPMCPServers resolves the WORKER-scoped MCP union for one execution and renders it for ACP.
//
// A resolution FAILURE IS NOT FATAL to the launch: the worker still gets the platform's own server, which
// is the same degradation claude's mcpArgs makes. What it must never do is silently produce an EMPTY set,
// because then the worker has no `orchicon_*` tools at all and its reports go nowhere.
func (b *Bridge) renderACPMCPServers(ctx context.Context, tenantID string, manifest scheduler.ExecutionManifest, inContainer bool) ([]acp.MCPServer, error) {
	builtin := b.orchiconACPServer(tenantID, manifest, inContainer)

	res, err := b.resolveMCP(ctx, tenantID, mcpclient.ScopeRef{
		// THE WORKER SCOPE: the project's owned definitions ∪ the executing VERSION's inline specs — the
		// same ref claude builds (session.go mcpArgs).
		Kind:           mcpclient.ScopeWorker,
		ProjectID:      manifest.ProjectID,
		WorkerID:       manifest.WorkerID,
		Version:        manifest.WorkerVersion,
		OwnPermissions: manifest.Permissions,
	})
	if err != nil {
		return []acp.MCPServer{builtin}, err
	}
	servers, rerr := b.renderMCP(ctx, tenantID, res, builtin)
	if rerr != nil {
		return []acp.MCPServer{builtin}, rerr
	}
	logMCPResolution("worker", servers, mcpclient.ProvenanceString(res.Servers))
	return servers, nil
}

// orchiconACPServer builds the platform's own sidecar entry, rendered for ACP.
//
// IT REUSES claude.OrchiconMCPServer rather than re-implementing it: the sidecar is the same binary with
// the same `mcp` subcommand and the same tenant env, and a second construction is a second place for the
// tenant id to be forgotten.
func (b *Bridge) orchiconACPServer(tenantID string, manifest scheduler.ExecutionManifest, inContainer bool) acp.MCPServer {
	extra := map[string]string{}
	if inContainer {
		// NOT inherited: the plane's ORCHICON_POSTGRES_DSN names the HOST's Postgres, which inside the
		// container is nothing. claude's workerMCPExtraEnv applies exactly this rule.
		extra["ORCHICON_POSTGRES_DSN"] = runtime.SandboxPostgresDSN
	}
	if runID := strings.TrimSpace(manifest.RuntimeWorkflowID); runID != "" {
		extra[claude.MCPWorkflowRunEnv] = runID
	}
	builtin := claude.OrchiconMCPServer(claude.HookBinaryFor(inContainer), tenantID, extra)
	return acpMCPServer(builtin)
}

// resolveMCP is THE resolution step: exactly one scope, the RAW result WITH its provenance.
func (b *Bridge) resolveMCP(ctx context.Context, tenantID string, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	if b.mcpResolver == nil {
		return mcpclient.Resolution{}, nil
	}
	// The resolver reads the tenant from the context, so the session's tenant must be ON it.
	res, err := b.mcpResolver.ResolveScope(tenant.WithID(ctx, tenantID), ref)
	if err != nil {
		return mcpclient.Resolution{}, fmt.Errorf("antigravity: resolve MCP servers: %w", err)
	}
	return res, nil
}

// renderMCP renders a resolution into ACP's server list: the built-in Orchicon sidecar FIRST, then the
// resolved servers, with ${SECRET_NAME} refs expanded.
//
// A SELECTED-BUT-UNCONFIGURED SERVER IS A LAUNCH FAILURE, not a silent omission: it means the project's or
// the version's MCP definitions disagree with what is stored, and running the worker without a server its
// configuration names would leave the model reporting it cannot call a tool.
func (b *Bridge) renderMCP(ctx context.Context, tenantID string, res mcpclient.Resolution, builtin acp.MCPServer) ([]acp.MCPServer, error) {
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("antigravity: MCP server(s) selected but not configured (project / version "+
			"permissions): %v — fix the project's or this version's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		return []acp.MCPServer{builtin}, nil
	}
	specs := make([]mcpclient.ServerSpec, 0, len(res.Servers))
	for _, ss := range res.Servers {
		specs = append(specs, ss.Spec)
	}
	if err := b.resolveMCPSpecSecrets(ctx, tenantID, specs); err != nil {
		return nil, err
	}
	out := make([]acp.MCPServer, 0, len(specs)+1)
	out = append(out, builtin)
	for _, sp := range specs {
		if s, ok := acpMCPServerFromSpec(sp); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// resolveMCPSpecSecrets expands ${SECRET_NAME} references in place. A nil resolver passes them through
// (tests / an unwired plane), which is the same degrade the native bridge applies.
func (b *Bridge) resolveMCPSpecSecrets(ctx context.Context, tenantID string, specs []mcpclient.ServerSpec) error {
	if b.mcpSecretResolver == nil {
		return nil
	}
	for i := range specs {
		env, headers, err := b.mcpSecretResolver(ctx, tenantID, specs[i].Env, specs[i].Headers)
		if err != nil {
			return fmt.Errorf("antigravity: MCP server %q: %w", specs[i].ID, err)
		}
		specs[i].Env = env
		specs[i].Headers = headers
	}
	return nil
}

// acpMCPServer converts claude's neutral server entry into ACP's declaration.
func acpMCPServer(s claude.MCPServer) acp.MCPServer {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		name = "orchicon"
	}
	out := acp.MCPServer{Name: name}
	if u := strings.TrimSpace(s.URL); u != "" {
		out.URL = u
		out.Type = "http"
		out.Headers = acp.EnvList(s.Headers)
		return out
	}
	out.Command = s.Command
	out.Args = s.Args
	// ACP declares a stdio server's environment as a {name,value} LIST, not a map.
	out.Env = acp.EnvList(s.Env)
	return out
}

// acpMCPServerFromSpec renders one neutral spec directly, reporting false for an entry with no transport.
func acpMCPServerFromSpec(sp mcpclient.ServerSpec) (acp.MCPServer, bool) {
	id := strings.TrimSpace(sp.ID)
	if id == "" {
		return acp.MCPServer{}, false
	}
	if u := strings.TrimSpace(sp.URL); u != "" {
		// A REMOTE SERVER IS RENDERED BUT ITS SUPPORT IS UNVERIFIED against agy 1.5.13 (the recording only
		// exercised a stdio server), so it is flagged rather than silently claimed to work.
		slog.Default().Warn("antigravity: registering a REMOTE MCP server — agy 1.5.13's support for a remote "+
			"server was not recorded, so the tool list may not appear", "server", id, "url", u)
		return acp.MCPServer{Name: id, URL: u, Type: "http", Headers: acp.EnvList(sp.Headers)}, true
	}
	if len(sp.Command) == 0 {
		return acp.MCPServer{}, false
	}
	return acp.MCPServer{
		Name:    id,
		Command: sp.Command[0],
		Args:    append([]string(nil), sp.Command[1:]...),
		Env:     acp.EnvList(sp.Env),
	}, true
}

// logMCPResolution records what the session actually got, because an MCP surface that is absent is
// otherwise invisible: the model simply reports it cannot call a tool.
func logMCPResolution(where string, servers []acp.MCPServer, provenance string) {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	slog.Default().Info("antigravity: MCP servers registered",
		"transport", where, "servers", strings.Join(names, ","), "provenance", provenance)
}
