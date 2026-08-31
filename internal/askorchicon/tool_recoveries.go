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
		PageToken string `json:"page_token"`
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
		AfterID:   params.PageToken,
		PageSize:  listCap + 1,
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(recoveries))
	for _, r := range recoveries {
		out = append(out, compactRecovery(r))
	}
	// No get_recovery tool exists — the note points at the filters and, when
	// truncated, at next_page_token for the rest.
	env := newCompactList(out, "")
	if len(recoveries) > listCap {
		env.setNextPage(recoveries[listCap-1].ID)
	}
	return json.Marshal(env)
}
