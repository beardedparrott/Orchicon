package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetMCPServerByName resolves a server row by its tenant-unique name.
func GetMCPServerByName(ctx context.Context, tx pgx.Tx, tenantID, name string) (MCPServerRow, error) {
	const q = `SELECT ` + mcpServerCols + ` FROM mcp_servers WHERE tenant_id=$1 AND name=$2`
	r, err := scanMCPServer(tx.QueryRow(ctx, q, tenantID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPServerRow{}, ErrNotFound
	}
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: get mcp server by name: %w", err)
	}
	return r, nil
}
