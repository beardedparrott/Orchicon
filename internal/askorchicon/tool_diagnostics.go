package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/beardedparrott/orchicon/internal/antigravity/authmethod"
	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolDiagnoseFailure(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ExecutionID   string `json:"execution_id"`
		WorkflowRunID string `json:"workflow_run_id"`
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
		// THE RUN ITSELF IS A DIAGNOSIS SOURCE, and leaving it out is why this tool returned an empty
		// analysis for a run that had plainly failed.
		if run, rerr := db.GetWorkflowRun(ctx, ttx.Tx, tenantID, params.WorkflowRunID); rerr == nil {
			diagnosis["run"] = map[string]any{
				"id":            run.ID,
				"status":        run.Status,
				"current_step":  run.CurrentStep,
				"error":         run.ErrorMessage,
				"error_kind":    run.ErrorKind,
				"worktree":      run.WorktreeStatus,
				"worktree_path": run.WorktreePath,
				"branch":        run.WorktreeBranch,
			}
			if run.ErrorMessage != "" {
				label := run.ErrorKind
				if label == "" {
					label = "platform"
				}
				diagnosis["failure_analysis"] = append(diagnosis["failure_analysis"].([]string),
					fmt.Sprintf("Workflow run %s failed (%s): %s", run.ID, label, run.ErrorMessage))
			} else if run.Status == "failed" {
				// A failed run with NO recorded reason is itself a finding, and saying so is better than an
				// empty list that reads as "nothing wrong".
				diagnosis["failure_analysis"] = append(diagnosis["failure_analysis"].([]string),
					fmt.Sprintf("Workflow run %s is %s but recorded no error — the failure happened outside any "+
						"step (preflight or dispatch) before errors were recorded for that path", run.ID, run.Status))
			}
		}

		runs, err := db.ListExecutions(ctx, ttx.Tx, db.ListExecutionsFilter{
			TenantID:      tenantID,
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

// toolGetUsage reads token/cost usage.
//
// IT COULD NOT BE SCOPED TO THE THING A CALLER ACTUALLY ASKS ABOUT. The db filter has carried
// ExecutionID / TaskID / SessionID / time bounds all along, but this tool exposed only
// project/provider/model — so "how many tokens did execution X use, and what did it cost?" had no
// answer from here, and the honest-looking reply was a page of the tenant's most recent usage rows
// that happened to be a DIFFERENT execution's. That reads as "the tool reports 0 / wrong numbers"
// from the outside, because the number it returns is not about the thing that was asked about.
//
// A SUMMARY is returned alongside the page so a scoped call needs no client-side arithmetic: the
// per-execution figure the executions page shows is SUM(total_tokens) over exactly these rows, and
// the whole point of this tool is to agree with it.
func toolGetUsage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ProjectID   string `json:"project_id"`
		TaskID      string `json:"task_id"`
		ExecutionID string `json:"execution_id"`
		SessionID   string `json:"session_id"`
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		StartTime   string `json:"start_time"`
		EndTime     string `json:"end_time"`
		PageToken   string `json:"page_token"`
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

	parseTime := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		return time.Time{}
	}

	filter := db.ListUsageRecordsFilter{
		TenantID:    tenantID,
		ProjectID:   params.ProjectID,
		TaskID:      params.TaskID,
		ExecutionID: params.ExecutionID,
		SessionID:   params.SessionID,
		Provider:    params.Provider,
		Model:       params.Model,
		StartTime:   parseTime(params.StartTime),
		EndTime:     parseTime(params.EndTime),
		AfterID:     params.PageToken,
		PageSize:    int32(listCap + 1),
	}
	rows, err := db.ListUsageRecords(ctx, ttx.Tx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, compactUsage(r))
	}
	env := newCompactList(out, "")
	if len(rows) > listCap {
		env.setNextPage(rows[listCap-1].ID)
	}

	// The SUMMARY is computed over the WHOLE filter, not the page: paging must not change the total,
	// or a long execution's cost would appear to shrink as you page through it.
	summary := map[string]any{}
	scoped := filter
	scoped.AfterID = ""
	scoped.PageSize = 1000
	var tokens int64
	var cost float64
	var records int
	for {
		page, perr := db.ListUsageRecords(ctx, ttx.Tx, scoped)
		if perr != nil {
			return nil, perr
		}
		for _, r := range page {
			tokens += r.TotalTokens
			cost += r.CostUSD
			records++
		}
		if len(page) < 1000 {
			break
		}
		scoped.AfterID = page[len(page)-1].ID
	}
	summary["total_tokens"] = tokens
	summary["cost_usd"] = cost
	summary["record_count"] = records
	// State the scope back explicitly: an empty result is then unambiguous ("no rows for THIS
	// execution") instead of being mistaken for "the filter was ignored".
	summary["scope"] = map[string]string{
		"project_id": params.ProjectID, "task_id": params.TaskID,
		"execution_id": params.ExecutionID, "session_id": params.SessionID,
		"provider": params.Provider, "model": params.Model,
	}
	b, err := json.Marshal(struct {
		compactList
		Summary map[string]any `json:"summary"`
	}{compactList: env, Summary: summary})
	if err != nil {
		return nil, err
	}
	return b, nil
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
	// The row is marshalled directly (its field tags are the API keys), and the ToS notice is
	// ADDED here: it is a read-only constant, not a column, and an agent deciding whether to
	// select oauth-personal must be able to read the same notice an operator sees.
	b, err := json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	out["antigravity_tos_notice"] = authmethod.ToSNotice
	return json.Marshal(out)
}

func toolUpdateSettings(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		DefaultWorkerModel               string  `json:"default_worker_model"`
		DefaultAskOrchiconModel          string  `json:"default_ask_orchicon_model"`
		StallNoProgressWindowSeconds     *int64  `json:"stall_no_progress_window_seconds"`
		StallNoFileDiffWindowSeconds     *int64  `json:"stall_no_file_diff_window_seconds"`
		StallTextLoopWindowSeconds       *int64  `json:"stall_text_loop_window_seconds"`
		StallRepetitionCount             *int32  `json:"stall_repetition_count"`
		StallRepetitionWindowSeconds     *int64  `json:"stall_repetition_window_seconds"`
		StallNudgeMax                    *int32  `json:"stall_nudge_max"`
		StallNudgeReplyWindowSeconds     *int64  `json:"stall_nudge_reply_window_seconds"`
		StallNudgeCooldownSeconds        *int64  `json:"stall_nudge_cooldown_seconds"`
		StallToolHangSeconds             *int64  `json:"stall_tool_hang_seconds"`
		MaxToolOutputBytes               *int64  `json:"max_tool_output_bytes"`
		DefaultBudgetOverrides           *string `json:"default_budget_overrides"`
		ExecutionReapGraceSeconds        int64   `json:"execution_reap_grace_seconds"`
		ExecutionReapConsecutiveFailures int32   `json:"execution_reap_consecutive_failures"`
		SessionAccessTokenTtlSeconds     int64   `json:"session_access_token_ttl_seconds"`
		SessionRefreshTokenTtlSeconds    int64   `json:"session_refresh_token_ttl_seconds"`
		// Pointer, not value: "the key was absent" and "the operator sent false" are different
		// answers, and the telemetry column is written unconditionally.
		AntigravityAuthMethod     *string `json:"antigravity_auth_method"`
		AntigravityShareTelemetry *bool   `json:"antigravity_share_telemetry"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	// Validate the default model refs against the shared adapter/provider/model grammar BEFORE touching
	// the DB (parity with the SettingsService RPC).
	if err := toolValidateModelRef(params.DefaultAskOrchiconModel); err != nil {
		return nil, fmt.Errorf("default_ask_orchicon_model: %w", err)
	}
	if err := toolValidateModelRef(params.DefaultWorkerModel); err != nil {
		return nil, fmt.Errorf("default_worker_model: %w", err)
	}
	// Parity with SettingsService.UpdateSettings: reject an unknown method BEFORE the DB, naming
	// the allowed values instead of surfacing a CHECK-constraint error.
	if params.AntigravityAuthMethod != nil {
		if err := authmethod.Validate(*params.AntigravityAuthMethod); err != nil {
			return nil, err
		}
	}
	var budget []byte
	if params.DefaultBudgetOverrides != nil {
		budget = []byte(*params.DefaultBudgetOverrides)
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)

	// The typed budget columns are written unconditionally on upsert, so
	// preserve the current ladder/gates and overlay the client's budget JSON.
	cur, err := db.GetTenantSettings(ctx, ttx.Tx, tenantID)
	if err != nil {
		return nil, err
	}
	inRow := db.TenantSettingsRow{
		DefaultWorkerModel:               params.DefaultWorkerModel,
		DefaultAskOrchiconModel:          params.DefaultAskOrchiconModel,
		StallNoProgressWindowSeconds:     params.StallNoProgressWindowSeconds,
		StallNoFileDiffWindowSeconds:     params.StallNoFileDiffWindowSeconds,
		StallTextLoopWindowSeconds:       params.StallTextLoopWindowSeconds,
		StallRepetitionCount:             params.StallRepetitionCount,
		StallRepetitionWindowSeconds:     params.StallRepetitionWindowSeconds,
		StallNudgeMax:                    params.StallNudgeMax,
		StallNudgeReplyWindowSeconds:     params.StallNudgeReplyWindowSeconds,
		StallNudgeCooldownSeconds:        params.StallNudgeCooldownSeconds,
		StallToolHangSeconds:             params.StallToolHangSeconds,
		MaxToolOutputBytes:               params.MaxToolOutputBytes,
		DefaultBudgetOverrides:           budget,
		ExecutionReapGraceSeconds:        params.ExecutionReapGraceSeconds,
		ExecutionReapConsecutiveFailures: params.ExecutionReapConsecutiveFailures,
		SessionAccessTokenTtlSeconds:     params.SessionAccessTokenTtlSeconds,
		SessionRefreshTokenTtlSeconds:    params.SessionRefreshTokenTtlSeconds,
	}
	// Both Antigravity columns are written unconditionally on upsert, so start from the stored
	// values and overlay only what the caller actually sent.
	inRow.AntigravityAuthMethod = cur.AntigravityAuthMethod
	inRow.AntigravityShareTelemetry = cur.AntigravityShareTelemetry
	if params.AntigravityAuthMethod != nil {
		inRow.AntigravityAuthMethod = *params.AntigravityAuthMethod
	}
	if params.AntigravityShareTelemetry != nil {
		inRow.AntigravityShareTelemetry = *params.AntigravityShareTelemetry
	}
	inRow.Budget = cur.Budget
	// Preserve the current compaction/memory policy columns when the client's budget JSON omits the
	// context_compaction/memory keys (D4 partial-update semantics.
	inRow.ContextCompactionEnabled = cur.ContextCompactionEnabled
	inRow.ContextCompactionPressureFrac = cur.ContextCompactionPressureFrac
	inRow.ContextRecentTurns = cur.ContextRecentTurns
	inRow.MemoryEnabled = cur.MemoryEnabled
	inRow.MemoryDigestEntries = cur.MemoryDigestEntries
	// STALL THRESHOLDS ARE DIFFERENT, and this is the one place the difference can bite.
	if params.StallNoProgressWindowSeconds == nil {
		inRow.StallNoProgressWindowSeconds = cur.StallNoProgressWindowSeconds
	}
	if params.StallNoFileDiffWindowSeconds == nil {
		inRow.StallNoFileDiffWindowSeconds = cur.StallNoFileDiffWindowSeconds
	}
	if params.StallTextLoopWindowSeconds == nil {
		inRow.StallTextLoopWindowSeconds = cur.StallTextLoopWindowSeconds
	}
	if params.StallRepetitionCount == nil {
		inRow.StallRepetitionCount = cur.StallRepetitionCount
	}
	if params.StallRepetitionWindowSeconds == nil {
		inRow.StallRepetitionWindowSeconds = cur.StallRepetitionWindowSeconds
	}
	if params.StallNudgeMax == nil {
		inRow.StallNudgeMax = cur.StallNudgeMax
	}
	if params.StallNudgeReplyWindowSeconds == nil {
		inRow.StallNudgeReplyWindowSeconds = cur.StallNudgeReplyWindowSeconds
	}
	if params.StallNudgeCooldownSeconds == nil {
		inRow.StallNudgeCooldownSeconds = cur.StallNudgeCooldownSeconds
	}
	if params.StallToolHangSeconds == nil {
		inRow.StallToolHangSeconds = cur.StallToolHangSeconds
	}
	if params.MaxToolOutputBytes == nil {
		inRow.MaxToolOutputBytes = cur.MaxToolOutputBytes
	}
	if err := inRow.ApplyBudgetJSON(budget); err != nil {
		return nil, fmt.Errorf("invalid default_budget_overrides: %w", err)
	}

	settings, err := db.UpdateTenantSettings(ctx, ttx.Tx, tenantID, inRow)
	if err != nil {
		return nil, err
	}

	// THIS WRITE IS AUDITED, AND IT WAS NOT.
	if err := recordAudit(ctx, ttx.Tx, tenantID, "settings.updated", "settings", tenantID,
		nil, audit.Snapshot(settings.AuditFields())); err != nil {
		return nil, fmt.Errorf("audit settings.updated: %w", err)
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
