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
//	mcpclient.ConfigSource              — the seam: ServerList + Worker/Project selection
//	mcpsettings.NewConfigSource(pool)   — the storage-backed implementation
//	mcpclient.Resolve(ctx, src, …)      — worker → project → tenant-default → none
//	mcpclient.ServerSpec                — the neutral transport shape
//	mcpsettings.ResolveSecretRefs       — ${SECRET_NAME} → plaintext
//
// The NATIVE bridge consumes exactly that set (orchicon.SetConfigSource +
// SetMCPSecretResolver, wired in server.go). This file makes claude consume the
// same set, so the two cannot drift about WHICH servers apply.
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

// resolveMCPServers returns every MCP server this session gets:
// the built-in Orchicon sidecar, then the tenant-resolved set.
//
// A missing selection is reported the way the native bridge reports it — an
// error naming the ids — because a worker whose MCP selection silently did not
// apply would run without tools it was configured to have, which is worse than
// not starting. An absent source is simply "no operator servers": tests and a
// plane without the storage wiring keep working, and the built-in server is
// still registered.
// `builtin` is the platform's own Orchicon entry, built by the CALLER because
// only the caller knows its transport: a worker in a runtime container needs the
// daemon's bind-mounted binary and the in-container sandbox Postgres, while an
// Ask session runs this process's own executable. Resolution is shared; the
// built-in's shape is not.
func (b *Bridge) resolveMCPServers(ctx context.Context, tenantID, workerID, projectID string, builtin MCPServer) ([]MCPServer, error) {
	if b.mcpConfig == nil {
		return []MCPServer{builtin}, nil
	}
	// The source reads the tenant from the context (mcpsettings.ConfigSource),
	// so the session's tenant must be ON it — the same convention the native
	// bridge follows.
	res, err := mcpclient.Resolve(tenant.WithID(ctx, tenantID), b.mcpConfig, workerID, projectID)
	if err != nil {
		return nil, fmt.Errorf("claude: resolve MCP servers: %w", err)
	}
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("claude: MCP server(s) selected but not configured: %v — fix the worker/project MCP selection or the tenant server list", res.Missing)
	}
	if len(res.Servers) == 0 {
		return []MCPServer{builtin}, nil
	}
	if err := b.resolveMCPSpecSecrets(ctx, tenantID, res.Servers); err != nil {
		return nil, err
	}
	out := make([]MCPServer, 0, len(res.Servers)+1)
	out = append(out, builtin)
	out = append(out, MCPServersFromSpecs(res.Servers)...)
	return out, nil
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
func logMCPResolution(where string, servers []MCPServer) {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s.Name)
	}
	slog.Default().Info("claude: MCP servers registered", "transport", where, "servers", strings.Join(names, ","))
}
