package claude

// mcpresolve.go — the ONE place this adapter decides which MCP servers an
// execution or conversation gets.
//
// # The agnostic shape, and why it is written this way
//
// Resolving MCP servers is NOT adapter work, and duplicating it per adapter is
// how two adapters end up disagreeing about which servers apply. The platform
// already owns the whole resolution:
//
//	mcpclient.ScopeResolver             — the seam: ResolveScope(ScopeRef)
//	mcpsettings.NewResolver(pool)       — the storage-backed implementation
//	mcpclient.Resolution/ScopedServer   — the union + per-server provenance
//	mcpclient.ServerSpec                — the neutral transport shape
//	mcpsettings.ResolveSecretRefs       — ${SECRET_NAME} → plaintext
//
// The NATIVE bridge consumes exactly that set (orchicon.SetScopeResolver +
// SetMCPSecretResolver, wired in server.go). This file makes claude consume the
// same set, so the two cannot drift about WHICH servers apply.
//
// The scope resolved here is the WORKER scope for a worker execution — the
// union of the project's owned definitions and the executing version's inline
// specs (ScopeRef.OwnPermissions, from ExecutionManifest.Permissions). Ask
// resolves the PROJECT scope (child 6 owns the Ask surface).
//
// WHAT IS ADAPTER-SPECIFIC IS ONE FUNCTION: rendering the neutral []ServerSpec
// into claude's config format (MCPServersFromSpecs in mcpconfig.go). A codex
// adapter needs its own renderer and nothing else — no second resolution stack,
// no second selection rule, no second secret path. That is the standardisation
// the operator asked for, and it is deliberately a SEAM rather than a framework:
// three small types, all already in the tree.
//
// # The built-in Orchicon server is added ON TOP, never instead
//
// The resolved set is the OPERATOR'S servers. The Orchicon sidecar is the
// platform's own and is appended unconditionally: a tenant that has configured
// no MCP servers must still get `orchicon_*` tools, which is the state this
// adapter was in until the operator's Opus 5 reported it.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// resolveMCP is THE resolution step: exactly one scope, the RAW result WITH
// its provenance. Nothing here renders, expands secrets, or adds the built-in —
// a resolution is an observation, and the log line and the parity test both
// read THIS value (never a post-expansion copy, which would carry plaintext
// credentials).
func (b *Bridge) resolveMCP(ctx context.Context, tenantID string, ref mcpclient.ScopeRef) (mcpclient.Resolution, error) {
	if b.mcpResolver == nil {
		return mcpclient.Resolution{}, nil
	}
	// The resolver reads the tenant from the context, so the session's tenant
	// must be ON it — the same convention the native bridge follows.
	res, err := b.mcpResolver.ResolveScope(tenant.WithID(ctx, tenantID), ref)
	if err != nil {
		return mcpclient.Resolution{}, fmt.Errorf("claude: resolve MCP servers: %w", err)
	}
	return res, nil
}

// renderMCP renders a resolution into claude's config list: the built-in
// Orchicon sidecar FIRST, then the resolved servers (rendered by
// MCPServersFromSpecs), with ${SECRET_NAME} refs expanded. The built-in is added
// ON TOP, never instead: a tenant with no configured servers must still get the
// orchicon_* surface.
//
// A missing selection is the native bridge's documented failure and is kept
// here — a worker whose MCP selection silently did not apply looks healthy and
// cannot do its job.
func (b *Bridge) renderMCP(ctx context.Context, tenantID string, res mcpclient.Resolution, builtin MCPServer) ([]MCPServer, error) {
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("claude: MCP server(s) selected but not configured (project / version permissions): %v — fix the project's or this version's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		return []MCPServer{builtin}, nil
	}
	specs := make([]mcpclient.ServerSpec, 0, len(res.Servers))
	for _, ss := range res.Servers {
		specs = append(specs, ss.Spec)
	}
	if err := b.resolveMCPSpecSecrets(ctx, tenantID, specs); err != nil {
		return nil, err
	}
	out := make([]MCPServer, 0, len(specs)+1)
	out = append(out, builtin)
	out = append(out, MCPServersFromSpecs(specs)...)
	return out, nil
}

// resolveMCPServers returns every MCP server an ASK conversation gets:
// the built-in Orchicon sidecar, then the tenant-resolved PROJECT set.
//
// It is the project-scope wrapper kept for Ask's caller (ask.go); a worker
// execution goes through resolveMCP + renderMCP with the WORKER scope so the
// version's inline specs are in the union. Callers that must LOG provenance
// call resolveMCP/renderMCP directly and read mcpclient.ProvenanceString off
// the RAW resolution.
//
// `builtin` is the platform's own Orchicon entry, built by the CALLER because
// only the caller knows its transport: a worker in a runtime container needs the
// daemon's bind-mounted binary and the in-container sandbox Postgres, while an
// Ask session runs this process's own executable. Resolution is shared; the
// built-in's shape is not.
func (b *Bridge) resolveMCPServers(ctx context.Context, tenantID, workerID, projectID string, builtin MCPServer) ([]MCPServer, error) {
	res, err := b.resolveMCP(ctx, tenantID, mcpclient.ScopeRef{
		Kind:      mcpclient.ScopeProject,
		ProjectID: projectID,
	})
	if err != nil {
		return nil, err
	}
	return b.renderMCP(ctx, tenantID, res, builtin)
}

// resolveMCPSpecSecrets expands ${SECRET_NAME} references in place, mirroring
// the native bridge's resolver semantics exactly (nil resolver = pass through).
func (b *Bridge) resolveMCPSpecSecrets(ctx context.Context, tenantID string, specs []mcpclient.ServerSpec) error {
	if b.mcpSecretResolver == nil {
		return nil
	}
	for i := range specs {
		env, headers, err := b.mcpSecretResolver(ctx, tenantID, specs[i].Env, specs[i].Headers)
		if err != nil {
			return fmt.Errorf("claude: MCP server %q: %w", specs[i].ID, err)
		}
		specs[i].Env = env
		specs[i].Headers = headers
	}
	return nil
}

// logMCPResolution records what the session actually got, because an MCP surface
// that is absent is otherwise invisible: the model simply reports it cannot call
// a tool, and nothing in the log says why.
//
// `provenance` names WHICH SCOPE supplied each server ("gh=inline:w1@3"), because
// "the server is missing" and "the server resolved from the WRONG scope" are
// different problems with different fixes. It is built from the RAW resolution
// (before ${SECRET} expansion), so it can never carry a credential.
func logMCPResolution(where string, servers []MCPServer, provenance string) {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	slog.Default().Info("claude: MCP servers registered", "transport", where, "servers", strings.Join(names, ","), "provenance", provenance)
}
