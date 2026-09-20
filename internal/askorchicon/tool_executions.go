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
	execs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
		Status:    params.Status,
		TaskID:    params.TaskID,
		AfterID:   params.PageToken,
		PageSize:  listCap + 1,
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(execs))
	for _, e := range execs {
		out = append(out, compactExecution(e))
	}
	env := newCompactList(out, "get_execution")
	if len(execs) > listCap {
		env.setNextPage(execs[listCap-1].ID)
	}
	return json.Marshal(env)
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
	// The worker_executions row columns are write-never (always zero) —
	// fill the totals from the usage-records sum (best-effort) so agents
	// stop reporting 0 tokens for executions with real spend.
	if tokens, cost, uerr := db.SumUsageForExecution(ctx, ttx.Tx, tenantID, exec.ID); uerr == nil {
		exec.TokenUsage = tokens
		exec.CostUSD = cost
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
