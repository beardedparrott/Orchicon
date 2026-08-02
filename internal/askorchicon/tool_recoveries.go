package askorchicon

import (
	"context"
	"encoding/json"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListRecoveries(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	if len(args) > 0 && string(args) != "null" {
		json.Unmarshal(args, &params)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	recoveries, err := db.ListRecoveries(ctx, ttx.Tx, db.ListRecoveriesFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
		Status:    params.Status,
	})
	if err != nil {
		return nil, err
	}
	if recoveries == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(recoveries)
}
