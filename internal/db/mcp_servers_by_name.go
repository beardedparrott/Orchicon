package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetMCPServerByNameForOwner resolves a server row by its OWNER-SCOPED name
// (the uniqueness the partial indexes mcp_servers_project_name_key /
// mcp_servers_conversation_name_key now enforce). Two projects may each own
// a `postgres`-named server; one owner may not hold two.
//
// conversationID is used for the conversation scope and projectID for the
// project scope; pass the unused one as "".
func GetMCPServerByNameForOwner(ctx context.Context, tx pgx.Tx, tenantID, projectID, conversationID, name string) (MCPServerRow, error) {
	const q = `SELECT ` + mcpServerCols + ` FROM mcp_servers
		WHERE tenant_id=$1 AND name=$2
		  AND ((project_id IS NOT NULL AND project_id=$3)
		    OR (conversation_id IS NOT NULL AND conversation_id=$4))`
	r, err := scanMCPServer(tx.QueryRow(ctx, q, tenantID, name, projectID, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return MCPServerRow{}, ErrNotFound
	}
	if err != nil {
		return MCPServerRow{}, fmt.Errorf("db: get mcp server by owner name: %w", err)
	}
	return r, nil
}
