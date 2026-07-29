package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListExecutions(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
		TaskID    string `json:"task_id"`
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
	execs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
		Status:    params.Status,
		TaskID:    params.TaskID,
	})
	if err != nil {
		return nil, err
	}
	if execs == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(execs)
}

func toolGetExecution(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(exec)
}

func toolCancelExecution(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetExecution(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if current.Status == domain.ExecutionTerminated {
		return json.Marshal(map[string]string{"status": "already_terminated"})
	}
	now := time.Now().UTC()
	updated, err := db.UpdateExecution(ctx, ttx.Tx, tenantID, params.ID, current.Version, db.UpdateExecutionFields{
		Status:      strPtr(domain.ExecutionTerminated),
		HealthState: strPtr(domain.HealthTerminating),
		EndedAt:     &now,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"status": "cancelled",
		"id":     updated.ID,
	})
}

func strPtr(s string) *string { return &s }
