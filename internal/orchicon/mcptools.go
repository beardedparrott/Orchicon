package orchicon

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// MCPSecretResolver resolves ${SECRET_NAME} references in a resolved MCP
// server's env/header values against the tenant secrets store before the
// session connects. It is injected into the bridge by the server so the
// orchicon package never imports mcpsettings (avoids a cycle; the resolver
// is a plain func).
type MCPSecretResolver func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error)

// resolveMCPSpecSecrets rewrites every resolved spec's Env/Headers in
// place, replacing ${SECRET_NAME} references with stored plaintext via the
// injected resolver. A missing referenced secret fails the session
// actionably, naming the secret (ADR-0008: missing secret → clear
// per-server error, never silent).
func (b *NativeBridge) resolveMCPSpecSecrets(ctx context.Context, tenantID string, specs []mcpclient.ServerSpec) error {
	if b.mcpSecretResolver == nil {
		return nil // no resolver injected → pass through as-is (tests / degrade)
	}
	for i := range specs {
		env, headers, err := b.mcpSecretResolver(ctx, tenantID, specs[i].Env, specs[i].Headers)
		if err != nil {
			return fmt.Errorf("orchicon bridge: MCP server %q: %w", specs[i].ID, err)
		}
		specs[i].Env = env
		specs[i].Headers = headers
	}
	return nil
}

// mcpResolveSecretsAndStart resolves the execution's MCP server set
// (worker → project → tenant-default → none over the tenant server list),
// resolves secret references, and starts the manager. It returns the tool
// registry (or nil when no servers resolve — no MCP tools, never an
// error).
func (b *NativeBridge) mcpResolveAndStart(ctx context.Context, exec db.ExecutionRow) (*mcpTools, error) {
	if b.mcpConfig == nil {
		return nil, nil
	}
	sctx := tenant.WithID(ctx, exec.TenantID)
	res, rerr := mcpclient.Resolve(sctx, b.mcpConfig, exec.WorkerID, exec.ProjectID)
	if rerr != nil {
		return nil, fmt.Errorf("orchicon bridge: resolve MCP servers: %w", rerr)
	}
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("orchicon bridge: MCP server(s) selected but not configured: %v — fix the worker/project MCP selection or the tenant server list", res.Missing)
	}
	if len(res.Servers) == 0 {
		return nil, nil
	}
	if err := b.resolveMCPSpecSecrets(sctx, exec.TenantID, res.Servers); err != nil {
		return nil, err
	}
	mgr := mcpclient.NewManager(b.log)
	if _, merr := mgr.Start(sctx, res.Servers); merr != nil {
		return nil, fmt.Errorf("orchicon bridge: MCP connect: %w", merr)
	}
	// Start blocks until the session's terminal result; Close runs at
	// session end (kills stdio children, closes HTTP).
	return &mcpTools{mgr: mgr, close: mgr.Close}, nil
}

var errNoConfigSource = errors.New("no MCP config source")
