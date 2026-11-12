package orchicon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// MCPSecretResolver resolves ${SECRET_NAME} references in a resolved MCP server's env/header values
// against the tenant secrets store before the session connects.
type MCPSecretResolver func(ctx context.Context, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error)

// resolveMCPSpecSecrets rewrites every resolved spec's Env/Headers in place, replacing
// ${SECRET_NAME} references with stored plaintext via the injected resolver.
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

// ResolveExecutionMCP resolves the MCP set an execution gets.
func (b *NativeBridge) ResolveExecutionMCP(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest) (mcpclient.Resolution, error) {
	if b.mcpResolver == nil {
		return mcpclient.Resolution{}, nil
	}
	ref := mcpclient.ScopeRef{
		Kind:           mcpclient.ScopeWorker,
		ProjectID:      exec.ProjectID,
		WorkerID:       exec.WorkerID,
		Version:        manifest.WorkerVersion,
		OwnPermissions: manifest.Permissions,
	}
	res, err := b.mcpResolver.ResolveScope(tenant.WithID(ctx, exec.TenantID), ref)
	if err != nil {
		return mcpclient.Resolution{}, fmt.Errorf("orchicon bridge: resolve MCP servers: %w", err)
	}
	return res, nil
}

// mcpResolveAndStart resolves the execution's MCP definitions by SCOPE, resolves secret references,
// and starts the manager.
func (b *NativeBridge) mcpResolveAndStart(ctx context.Context, exec db.ExecutionRow, manifest scheduler.ExecutionManifest) (*mcpTools, error) {
	if b.mcpResolver == nil {
		return nil, nil
	}
	sctx := tenant.WithID(ctx, exec.TenantID)
	res, rerr := b.ResolveExecutionMCP(ctx, exec, manifest)
	if rerr != nil {
		return nil, rerr
	}
	if len(res.Missing) > 0 {
		return nil, fmt.Errorf("orchicon bridge: MCP server(s) selected but not configured (project / version permissions): %v — fix the project's or this version's MCP definitions", res.Missing)
	}
	if len(res.Servers) == 0 {
		return nil, nil
	}
	// LOG WHAT WAS RESOLVED, WITH PROVENANCE, BEFORE the secret expansion below mutates the specs'
	// Env/Headers in place.
	names := make([]string, 0, len(res.Servers))
	for _, ss := range res.Servers {
		names = append(names, ss.Spec.ID)
	}
	b.log.Info("orchicon: MCP servers registered",
		"execution", exec.ID,
		"servers", strings.Join(names, ","),
		"provenance", mcpclient.ProvenanceString(res.Servers))

	specs := make([]mcpclient.ServerSpec, 0, len(res.Servers))
	for _, ss := range res.Servers {
		specs = append(specs, ss.Spec)
	}
	if err := b.resolveMCPSpecSecrets(sctx, exec.TenantID, specs); err != nil {
		// A spec whose ${SECRET_NAME} cannot be resolved cannot run either, so the failure is
		// scope-annotated exactly like a connect failure — parity with claude's error path.
		return nil, mcpclient.DescribeFailedServer(err, res.Servers)
	}
	mgr := mcpclient.NewManager(b.log)
	_, merr := mgr.Start(sctx, specs)
	// A PARTIAL START IS A SUCCESS WITH MISSING TOOLS, and the worker has to be told which: a silently
	// absent tool reads as a platform that does not have it.
	for _, f := range mgr.Failures() {
		b.log.Warn("orchicon: an MCP server could not be started — ITS tools are absent this execution, the rest of the surface is unaffected",
			"execution", exec.ID, "server", f.ServerID, "transport", f.Transport,
			"scope", mcpclient.DescribeFailedServer(f.Err, res.Servers))
	}
	if merr != nil {
		return nil, fmt.Errorf("orchicon bridge: MCP connect: %w", mcpclient.DescribeFailedServer(merr, res.Servers))
	}
	// Start blocks until the session's terminal result; Close runs at
	// session end (kills stdio children, closes HTTP).
	return &mcpTools{mgr: mgr, close: mgr.Close}, nil
}

var errNoScopeResolver = errors.New("no MCP scope resolver")
