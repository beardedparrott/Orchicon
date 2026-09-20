package mcpsettings

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/mcpclient"
	"github.com/beardedparrott/orchicon/internal/secretcrypto"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// ConfigSource implements mcpclient.ConfigSource over the tenant's stored
// MCP server entries + selections (ADR-0008). It is injected into the
// session bridge (NativeBridge.SetConfigSource) so the MCP-client task
// resolves the execution's server set from storage at session time.
//
// The tenant is read from the context (tenant.FromContext); the bridge
// scopes the session context with the execution's tenant before
// resolving. ServerList returns enabled entries only (disabled servers
// never connect — they remain in the pickers for re-enable).
// ProjectSelection falls back to the tenant default when the project has
// no selection, implementing the full worker → project → tenant-default
// → none resolution order inside the ConfigSource contract (mcpclient
// only knows worker → project → none).
//
// Secret references (${SECRET_NAME}) in env/header values are NOT
// resolved here — they are resolved at session time by the bridge's
// secret resolver so only actually-selected servers trigger a secret
// lookup (a missing secret is a clear per-server error, never a session
// kill for an unrelated unselected entry).
type ConfigSource struct {
	pool *db.Pool
}

// NewConfigSource constructs the storage-backed ConfigSource.
func NewConfigSource(pool *db.Pool) *ConfigSource {
	return &ConfigSource{pool: pool}
}

// ServerList implements mcpclient.ConfigSource: the tenant's enabled MCP
// server entries as specs.
func (c *ConfigSource) ServerList(ctx context.Context) ([]mcpclient.ServerSpec, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return nil, fmt.Errorf("mcpsettings config source: no tenant in context")
	}
	tx, err := c.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := db.ListMCPServers(ctx, tx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	specs := make([]mcpclient.ServerSpec, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		spec := mcpclient.ServerSpec{
			ID:      r.ID,
			Command: append([]string{r.Command}, r.Args...),
			URL:     r.URL,
			Headers: r.Headers,
			Env:     r.Env,
		}
		switch r.Transport {
		case TransportStreamable, "http":
			spec.Type = mcpclient.TypeHTTP
		default:
			spec.Type = mcpclient.TypeStdio
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// WorkerSelection implements mcpclient.ConfigSource: the worker's
// permissions.mcp_servers ids.
func (c *ConfigSource) WorkerSelection(ctx context.Context, workerID string) ([]string, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return nil, fmt.Errorf("mcpsettings config source: no tenant in context")
	}
	if workerID == "" {
		return nil, nil
	}
	tx, err := c.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	perms, err := workerPermissions(ctx, tx.Tx, tenantID, workerID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return db.MCPServerRefsFromPermissions(perms), nil
}

// ProjectSelection implements mcpclient.ConfigSource: the project's
// selection, falling back to the tenant default when the project has
// none (worker → project → tenant-default → none).
func (c *ConfigSource) ProjectSelection(ctx context.Context, projectID string) ([]string, error) {
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return nil, fmt.Errorf("mcpsettings config source: no tenant in context")
	}
	tx, err := c.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var sel []string
	if projectID != "" {
		sel, err = db.ListProjectMCPServerIDs(ctx, tx.Tx, tenantID, projectID)
		if err != nil {
			return nil, err
		}
	}
	if len(sel) == 0 {
		sel, err = db.GetTenantDefaultMCPServers(ctx, tx.Tx, tenantID)
		if err != nil {
			return nil, err
		}
	}
	return sel, nil
}

var _ mcpclient.ConfigSource = (*ConfigSource)(nil)

// ResolveSecretRefs replaces ${SECRET_NAME} references in a server's
// env/header values with the stored tenant-secret plaintext (never baked,
// never stored inline). A reference to an unset secret is a clear error
// naming the secret — callers surface it as a per-server session error.
// Values without a reference pass through unchanged.
func ResolveSecretRefs(ctx context.Context, pool *db.Pool, kek []byte, tenantID string, env, headers map[string]string) (map[string]string, map[string]string, error) {
	resolve := func(m map[string]string) (map[string]string, error) {
		if len(m) == 0 {
			return m, nil
		}
		// Collect unique referenced secret names.
		names := map[string]bool{}
		for _, v := range m {
			if name, ok := secretRefName(v); ok {
				names[name] = true
			}
		}
		if len(names) == 0 {
			return m, nil
		}
		var values map[string]string
		if len(names) > 0 {
			values = map[string]string{}
			tx, err := pool.BeginTenantTx(ctx, tenantID)
			if err != nil {
				return nil, err
			}
			for name := range names {
				row, err := db.GetSecretByName(ctx, tx.Tx, tenantID, name)
				if errors.Is(err, db.ErrNotFound) {
					_ = tx.Rollback(ctx)
					return nil, fmt.Errorf("secret %q referenced by the server is not stored", name)
				}
				if err != nil {
					_ = tx.Rollback(ctx)
					return nil, err
				}
				pt, err := secretcrypto.Decrypt(row.Ciphertext, kek)
				if err != nil {
					_ = tx.Rollback(ctx)
					return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
				}
				values[name] = string(pt)
			}
			_ = tx.Rollback(ctx) // read-only
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			if name, ok := secretRefName(v); ok {
				out[k] = values[name]
			} else {
				out[k] = v
			}
		}
		return out, nil
	}
	envOut, err := resolve(env)
	if err != nil {
		return nil, nil, err
	}
	headOut, err := resolve(headers)
	if err != nil {
		return nil, nil, err
	}
	return envOut, headOut, nil
}
