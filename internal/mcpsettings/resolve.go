package mcpsettings

import (
	"context"
	"errors"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/jackc/pgx/v5"
)

// Resolution contract (worker → project → tenant default → none), defined
// here as the storage-side helper and consumed by the sibling MCP-client
// task (internal/mcpclient.Resolve) at session time. Only enabled servers
// resolve; disabled entries are skipped.

// ResolveResult is the outcome of resolving a scope against the tenant
// server list.
type ResolveResult struct {
	// Servers are the resolved entries in selection order (worker →
	// project → tenant default), enabled only.
	Servers []Entry
	// SelectedIDs are the raw ids the resolution drew from.
	SelectedIDs []string
	// Missing are selected ids with no matching configured server.
	Missing []string
	// Disabled are selected ids whose entries are disabled (skipped).
	Disabled []string
}

// ResolveForScope resolves the effective MCP server set for a scope:
// worker selection wins when non-empty, else the project selection, else
// the tenant default. Empty everywhere → no servers (not an error).
func ResolveForScope(ctx context.Context, pool *db.Pool, tenantID, workerID string, projectID string) (ResolveResult, error) {
	var r ResolveResult
	tx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return r, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	servers, err := db.ListMCPServers(ctx, tx.Tx, tenantID)
	if err != nil {
		return r, err
	}
	byID := make(map[string]db.MCPServerRow, len(servers))
	for _, s := range servers {
		byID[s.ID] = s
	}

	// Worker selection: from the worker's latest published (or active)
	// version permissions jsonb.
	var sel []string
	if workerID != "" {
		perms, err := workerPermissions(ctx, tx.Tx, tenantID, workerID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return r, err
		}
		sel = db.MCPServerRefsFromPermissions(perms)
	}
	if len(sel) == 0 && projectID != "" {
		sel, err = db.ListProjectMCPServerIDs(ctx, tx.Tx, tenantID, projectID)
		if err != nil {
			return r, err
		}
	}
	if len(sel) == 0 {
		sel, err = db.GetTenantDefaultMCPServers(ctx, tx.Tx, tenantID)
		if err != nil {
			return r, err
		}
	}
	r.SelectedIDs = sel
	if len(sel) == 0 {
		return r, nil
	}

	seen := map[string]bool{}
	for _, id := range sel {
		if seen[id] {
			continue
		}
		seen[id] = true
		row, ok := byID[id]
		if !ok {
			r.Missing = append(r.Missing, id)
			continue
		}
		if !row.Enabled {
			r.Disabled = append(r.Disabled, id)
			continue
		}
		r.Servers = append(r.Servers, entryFromRow(row))
	}
	return r, nil
}

// workerPermissions returns the latest version's permissions jsonb for a
// worker (the latest version row is the one an execution would use).
func workerPermissions(ctx context.Context, tx pgx.Tx, tenantID, workerID string) ([]byte, error) {
	var perms []byte
	err := tx.QueryRow(ctx,
		`SELECT permissions FROM worker_versions wv
		 WHERE wv.tenant_id=$1 AND wv.worker_id=$2
		 ORDER BY wv.version DESC LIMIT 1`, tenantID, workerID).Scan(&perms)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: worker permissions: %w", err)
	}
	return perms, nil
}
