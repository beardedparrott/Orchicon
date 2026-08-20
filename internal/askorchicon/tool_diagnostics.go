package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolDiagnoseFailure(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ExecutionID    string `json:"execution_id"`
		WorkflowRunID  string `json:"workflow_run_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ExecutionID == "" && params.WorkflowRunID == "" {
		return nil, fmt.Errorf("execution_id or workflow_run_id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)

	diagnosis := map[string]any{
		"failure_analysis": []string{},
	}

	if params.ExecutionID != "" {
		exec, err := db.GetExecution(ctx, ttx.Tx, tenantID, params.ExecutionID)
		if err != nil {
			return nil, err
		}
		diagnosis["execution"] = map[string]any{
			"id":           exec.ID,
			"status":       exec.Status,
			"health_state": exec.HealthState,
			"error":        exec.ErrorMessage,
			"output":       truncateStr(exec.Output, 1000),
		}
		if exec.ErrorMessage != "" {
			diagnosis["failure_analysis"] = append(diagnosis["failure_analysis"].([]string),
				fmt.Sprintf("Execution %s failed with error: %s", exec.ID, exec.ErrorMessage))
		}
		if exec.Status == "failed" || exec.Status == "failed_to_start" {
			diagnosis["failure_analysis"] = append(diagnosis["failure_analysis"].([]string),
				fmt.Sprintf("Execution entered terminal status: %s", exec.Status))
		}
	}

	if params.WorkflowRunID != "" {
		runs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
			TenantID:     tenantID,
			WorkflowRunID: params.WorkflowRunID,
		})
		if err == nil {
			var failed []map[string]any
			for _, r := range runs {
				if r.Status == "failed" || r.Status == "failed_to_start" {
					failed = append(failed, map[string]any{
						"id":     r.ID,
						"status": r.Status,
						"error":  r.ErrorMessage,
					})
				}
			}
			diagnosis["workflow_executions"] = failed
			if len(failed) > 0 {
				diagnosis["failure_analysis"] = append(diagnosis["failure_analysis"].([]string),
					fmt.Sprintf("Workflow run %s has %d failed executions", params.WorkflowRunID, len(failed)))
			}
		}
	}

	return json.Marshal(diagnosis)
}

func toolGetUsage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID string `json:"project_id"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
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
	rows, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{
		TenantID:  tenantID,
		ProjectID: params.ProjectID,
	})
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(rows)
}

func toolGetSettings(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	settings, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(settings)
}

func toolUpdateSettings(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		DefaultWorkerModel           string `json:"default_worker_model"`
		DefaultAskOrchiconModel      string `json:"default_ask_orchicon_model"`
		StallNoProgressWindowSeconds int64  `json:"stall_no_progress_window_seconds"`
		StallNoFileDiffWindowSeconds int64  `json:"stall_no_file_diff_window_seconds"`
		StallTextLoopWindowSeconds   int64  `json:"stall_text_loop_window_seconds"`
		StallRepetitionCount         int32  `json:"stall_repetition_count"`
		StallRepetitionWindowSeconds int64  `json:"stall_repetition_window_seconds"`
		SessionAccessTokenTtlSeconds int64  `json:"session_access_token_ttl_seconds"`
		SessionRefreshTokenTtlSeconds int64 `json:"session_refresh_token_ttl_seconds"`
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
	settings, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenantID, db.TenantSettingsRow{
		DefaultWorkerModel:           params.DefaultWorkerModel,
		DefaultAskOrchiconModel:      params.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds: params.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds: params.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:   params.StallTextLoopWindowSeconds,
		StallRepetitionCount:         params.StallRepetitionCount,
		StallRepetitionWindowSeconds: params.StallRepetitionWindowSeconds,
		SessionAccessTokenTtlSeconds: params.SessionAccessTokenTtlSeconds,
		SessionRefreshTokenTtlSeconds: params.SessionRefreshTokenTtlSeconds,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(settings)
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}


