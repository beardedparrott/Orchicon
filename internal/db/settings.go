package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantSettingsRow is the in-memory representation of a tenant_settings row.
type TenantSettingsRow struct {
	TenantID                         string
	DefaultWorkerModel               string
	DefaultAskOrchiconModel          string
	StallNoProgressWindowSeconds     int64
	StallNoFileDiffWindowSeconds     int64
	StallTextLoopWindowSeconds       int64
	StallRepetitionCount             int32
	StallRepetitionWindowSeconds     int64
	StallNudgeMax                    int32
	StallNudgeReplyWindowSeconds     int64
	StallNudgeCooldownSeconds        int64
	DefaultBudgetOverrides           []byte       // jsonb: default budget JSON transport (see BudgetLadder for the typed source of truth); a worker's budget_overrides overrides these
	Budget                           BudgetLadder // typed budget ladder + gates (authoritative; the jsonb BudgetLadder... is the wire transport)
	ExecutionReapGraceSeconds        int64        // liveness reaper: min age before an execution is reaping-eligible
	ExecutionReapConsecutiveFailures int32        // liveness reaper: consecutive not-alive probes before reaping
	BackupSchedule                   string       // cron expression; empty = disabled
	BackupRetentionDays              int32        // 0 = keep all
	BackupDirectory                  string       // empty = default
	LogDirectory                     string       // serve log dir; empty = env/code default
	LogMaxSizeMB                     int64        // max log file size before rotation (MB); 0 = default
	LogRollIntervalHours             int64        // time-based roll interval (hours); 0 = default
	LogRetentionDays                 int32        // days rotated logs are kept; 0 = default
	LogMaxFiles                      int32        // max rotated log files kept; 0 = default
	MaxConcurrentRuns                int          // tenant-wide cap on concurrently running executions; 0 = no cap
	MaxConcurrentRunsSet             bool         // true when the update explicitly sets max_concurrent_runs (0 is meaningful)
	SessionAccessTokenTtlSeconds     int64        // access-token TTL in seconds; 0 = leave unchanged on update
	SessionRefreshTokenTtlSeconds    int64        // refresh-token TTL in seconds; 0 = leave unchanged on update
	CreatedAt                        time.Time
	UpdatedAt                        time.Time
}

// GetTenantSettings returns the current settings for a tenant. If no row
// exists, it is created with default (zero) values and returned.
func GetTenantSettings(ctx context.Context, tx pgx.Tx, tenantID string) (TenantSettingsRow, error) {
	const q = `SELECT tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, stall_nudge_max,
		stall_nudge_reply_window_seconds, stall_nudge_cooldown_seconds,
		default_budget_overrides,
		budget_tokens, budget_cost_usd, budget_tool_call_count,
		budget_wall_clock_seconds, budget_compact_max_turns,
		budget_warn_frac_tokens, budget_escalate_frac_tokens, budget_final_frac_tokens,
		budget_warn_frac_cost_usd, budget_escalate_frac_cost_usd, budget_final_frac_cost_usd,
		budget_warn_frac_tool_call_count, budget_escalate_frac_tool_call_count, budget_final_frac_tool_call_count,
		budget_warn_frac_wall_clock_seconds, budget_escalate_frac_wall_clock_seconds, budget_final_frac_wall_clock_seconds,
		budget_warn_msg_tokens, budget_escalate_msg_tokens, budget_final_msg_tokens,
		budget_warn_msg_cost_usd, budget_escalate_msg_cost_usd, budget_final_msg_cost_usd,
		budget_warn_msg_tool_call_count, budget_escalate_msg_tool_call_count, budget_final_msg_tool_call_count,
		budget_warn_msg_wall_clock_seconds, budget_escalate_msg_wall_clock_seconds, budget_final_msg_wall_clock_seconds,
		budget_compact_warn_tier, budget_compact_escalate_tier, budget_compact_final_tier,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
		max_concurrent_runs,
		session_access_token_ttl_seconds, session_refresh_token_ttl_seconds,
		created_at, updated_at
		FROM tenant_settings WHERE tenant_id = $1`
	row, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: get tenant settings: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanTenantSettings(row)
	}
	// No row exists — create defaults.
	const insertQ = `INSERT INTO tenant_settings (tenant_id) VALUES ($1)
		ON CONFLICT (tenant_id) DO NOTHING RETURNING
		tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, stall_nudge_max,
		stall_nudge_reply_window_seconds, stall_nudge_cooldown_seconds,
		default_budget_overrides,
		budget_tokens, budget_cost_usd, budget_tool_call_count,
		budget_wall_clock_seconds, budget_compact_max_turns,
		budget_warn_frac_tokens, budget_escalate_frac_tokens, budget_final_frac_tokens,
		budget_warn_frac_cost_usd, budget_escalate_frac_cost_usd, budget_final_frac_cost_usd,
		budget_warn_frac_tool_call_count, budget_escalate_frac_tool_call_count, budget_final_frac_tool_call_count,
		budget_warn_frac_wall_clock_seconds, budget_escalate_frac_wall_clock_seconds, budget_final_frac_wall_clock_seconds,
		budget_warn_msg_tokens, budget_escalate_msg_tokens, budget_final_msg_tokens,
		budget_warn_msg_cost_usd, budget_escalate_msg_cost_usd, budget_final_msg_cost_usd,
		budget_warn_msg_tool_call_count, budget_escalate_msg_tool_call_count, budget_final_msg_tool_call_count,
		budget_warn_msg_wall_clock_seconds, budget_escalate_msg_wall_clock_seconds, budget_final_msg_wall_clock_seconds,
		budget_compact_warn_tier, budget_compact_escalate_tier, budget_compact_final_tier,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
		max_concurrent_runs,
		session_access_token_ttl_seconds, session_refresh_token_ttl_seconds,
		created_at, updated_at`
	ins, err := tx.Query(ctx, insertQ, tenantID)
	if err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: create tenant settings: %w", err)
	}
	defer ins.Close()
	if ins.Next() {
		return scanTenantSettings(ins)
	}
	// Race: another goroutine created the row. Re-read.
	return GetTenantSettings(ctx, tx, tenantID)
}

// UpdateTenantSettings upserts the tenant settings row with the provided
// values. Empty/zero fields are NOT updated (only non-zero, non-empty values set).
// Returns the full row after update.
func UpdateTenantSettings(ctx context.Context, tx pgx.Tx, tenantID string, in TenantSettingsRow) (TenantSettingsRow, error) {
	// The INSERT branch supplies a value for default_budget_overrides, which
	// is jsonb NOT NULL. A caller that leaves the field unset (nil/empty)
	// must still yield a valid '{}' — passing literal NULL here makes the
	// INSERT violate the NOT NULL constraint BEFORE the ON CONFLICT arbiter
	// engages, so the upsert errors (or returns no row) instead of updating.
	if len(in.DefaultBudgetOverrides) == 0 {
		in.DefaultBudgetOverrides = []byte("{}")
	}
	// The typed budget columns are written unconditionally on the ON CONFLICT
	// DO UPDATE branch below, unlike every other field which is CASE-guarded
	// to preserve the existing value. Guard against a caller that did NOT
	// intend to change the budget (zero-value Budget + empty '{}' payload):
	// overlay the current ladder/gates so a partial settings update (e.g. only
	// editing log management) can never clobber a healthy budget with zeros.
	if budgetIsZero(in.Budget) {
		if cur, err := GetTenantSettings(ctx, tx, tenantID); err == nil {
			in.Budget = cur.Budget
		}
	}
	const q = `INSERT INTO tenant_settings (
		tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, stall_nudge_max,
		stall_nudge_reply_window_seconds, stall_nudge_cooldown_seconds,
		default_budget_overrides,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		backup_schedule, backup_retention_days, backup_directory,
		log_directory, log_max_size_mb, log_roll_interval_hours,
		log_retention_days, log_max_files, max_concurrent_runs,
		session_access_token_ttl_seconds, session_refresh_token_ttl_seconds,
		budget_tokens, budget_cost_usd, budget_tool_call_count,
		budget_wall_clock_seconds, budget_compact_max_turns,
		budget_warn_frac_tokens, budget_escalate_frac_tokens, budget_final_frac_tokens,
		budget_warn_frac_cost_usd, budget_escalate_frac_cost_usd, budget_final_frac_cost_usd,
		budget_warn_frac_tool_call_count, budget_escalate_frac_tool_call_count, budget_final_frac_tool_call_count,
		budget_warn_frac_wall_clock_seconds, budget_escalate_frac_wall_clock_seconds, budget_final_frac_wall_clock_seconds,
		budget_warn_msg_tokens, budget_escalate_msg_tokens, budget_final_msg_tokens,
		budget_warn_msg_cost_usd, budget_escalate_msg_cost_usd, budget_final_msg_cost_usd,
		budget_warn_msg_tool_call_count, budget_escalate_msg_tool_call_count, budget_final_msg_tool_call_count,
		budget_warn_msg_wall_clock_seconds, budget_escalate_msg_wall_clock_seconds, budget_final_msg_wall_clock_seconds,
		budget_compact_warn_tier, budget_compact_escalate_tier, budget_compact_final_tier,
		updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, now())
	ON CONFLICT (tenant_id) DO UPDATE SET
		default_worker_model = CASE WHEN $2 <> '' THEN $2 ELSE tenant_settings.default_worker_model END,
		default_ask_orchicon_model = CASE WHEN $3 <> '' THEN $3 ELSE tenant_settings.default_ask_orchicon_model END,
		stall_no_progress_window_seconds = CASE WHEN $4 <> 0 THEN $4 ELSE tenant_settings.stall_no_progress_window_seconds END,
		stall_no_file_diff_window_seconds = CASE WHEN $5 <> 0 THEN $5 ELSE tenant_settings.stall_no_file_diff_window_seconds END,
		stall_text_loop_window_seconds = CASE WHEN $6 <> 0 THEN $6 ELSE tenant_settings.stall_text_loop_window_seconds END,
		stall_repetition_count = CASE WHEN $7 <> 0 THEN $7 ELSE tenant_settings.stall_repetition_count END,
		stall_repetition_window_seconds = CASE WHEN $8 <> 0 THEN $8 ELSE tenant_settings.stall_repetition_window_seconds END,
		stall_nudge_max = CASE WHEN $9 <> 0 THEN $9 ELSE tenant_settings.stall_nudge_max END,
		stall_nudge_reply_window_seconds = CASE WHEN $10 <> 0 THEN $10 ELSE tenant_settings.stall_nudge_reply_window_seconds END,
		stall_nudge_cooldown_seconds = CASE WHEN $11 <> 0 THEN $11 ELSE tenant_settings.stall_nudge_cooldown_seconds END,
		default_budget_overrides = CASE WHEN $12 <> '{}'::jsonb THEN $12 ELSE tenant_settings.default_budget_overrides END,
		execution_reap_grace_seconds = CASE WHEN $13 <> 0 THEN $13 ELSE tenant_settings.execution_reap_grace_seconds END,
		execution_reap_consecutive_failures = CASE WHEN $14 <> 0 THEN $14 ELSE tenant_settings.execution_reap_consecutive_failures END,
		backup_schedule = CASE WHEN $15 <> '' THEN $15 ELSE tenant_settings.backup_schedule END,
		backup_retention_days = CASE WHEN $16 <> 0 THEN $16 ELSE tenant_settings.backup_retention_days END,
		backup_directory = CASE WHEN $17 <> '' THEN $17 ELSE tenant_settings.backup_directory END,
		log_directory = CASE WHEN $18 <> '' THEN $18 ELSE tenant_settings.log_directory END,
		log_max_size_mb = CASE WHEN $19 <> 0 THEN $19 ELSE tenant_settings.log_max_size_mb END,
		log_roll_interval_hours = CASE WHEN $20 <> 0 THEN $20 ELSE tenant_settings.log_roll_interval_hours END,
		log_retention_days = CASE WHEN $21 <> 0 THEN $21 ELSE tenant_settings.log_retention_days END,
		log_max_files = CASE WHEN $22 <> 0 THEN $22 ELSE tenant_settings.log_max_files END,
		max_concurrent_runs = CASE WHEN $24 THEN $23 ELSE tenant_settings.max_concurrent_runs END,
		session_access_token_ttl_seconds = CASE WHEN $25 <> 0 THEN $25 ELSE tenant_settings.session_access_token_ttl_seconds END,
		session_refresh_token_ttl_seconds = CASE WHEN $26 <> 0 THEN $26 ELSE tenant_settings.session_refresh_token_ttl_seconds END,
		budget_tokens = $27,
		budget_cost_usd = $28,
		budget_tool_call_count = $29,
		budget_wall_clock_seconds = $30,
		budget_compact_max_turns = $31,
		budget_warn_frac_tokens = $32,
		budget_escalate_frac_tokens = $33,
		budget_final_frac_tokens = $34,
		budget_warn_frac_cost_usd = $35,
		budget_escalate_frac_cost_usd = $36,
		budget_final_frac_cost_usd = $37,
		budget_warn_frac_tool_call_count = $38,
		budget_escalate_frac_tool_call_count = $39,
		budget_final_frac_tool_call_count = $40,
		budget_warn_frac_wall_clock_seconds = $41,
		budget_escalate_frac_wall_clock_seconds = $42,
		budget_final_frac_wall_clock_seconds = $43,
		budget_warn_msg_tokens = $44,
		budget_escalate_msg_tokens = $45,
		budget_final_msg_tokens = $46,
		budget_warn_msg_cost_usd = $47,
		budget_escalate_msg_cost_usd = $48,
		budget_final_msg_cost_usd = $49,
		budget_warn_msg_tool_call_count = $50,
		budget_escalate_msg_tool_call_count = $51,
		budget_final_msg_tool_call_count = $52,
		budget_warn_msg_wall_clock_seconds = $53,
		budget_escalate_msg_wall_clock_seconds = $54,
		budget_final_msg_wall_clock_seconds = $55,
		budget_compact_warn_tier = $56,
		budget_compact_escalate_tier = $57,
		budget_compact_final_tier = $58,
		updated_at = now()
	RETURNING tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, stall_nudge_max,
		stall_nudge_reply_window_seconds, stall_nudge_cooldown_seconds,
		default_budget_overrides,
		budget_tokens, budget_cost_usd, budget_tool_call_count,
		budget_wall_clock_seconds, budget_compact_max_turns,
		budget_warn_frac_tokens, budget_escalate_frac_tokens, budget_final_frac_tokens,
		budget_warn_frac_cost_usd, budget_escalate_frac_cost_usd, budget_final_frac_cost_usd,
		budget_warn_frac_tool_call_count, budget_escalate_frac_tool_call_count, budget_final_frac_tool_call_count,
		budget_warn_frac_wall_clock_seconds, budget_escalate_frac_wall_clock_seconds, budget_final_frac_wall_clock_seconds,
		budget_warn_msg_tokens, budget_escalate_msg_tokens, budget_final_msg_tokens,
		budget_warn_msg_cost_usd, budget_escalate_msg_cost_usd, budget_final_msg_cost_usd,
		budget_warn_msg_tool_call_count, budget_escalate_msg_tool_call_count, budget_final_msg_tool_call_count,
		budget_warn_msg_wall_clock_seconds, budget_escalate_msg_wall_clock_seconds, budget_final_msg_wall_clock_seconds,
		budget_compact_warn_tier, budget_compact_escalate_tier, budget_compact_final_tier,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
		max_concurrent_runs,
		session_access_token_ttl_seconds, session_refresh_token_ttl_seconds,
		created_at, updated_at`
	row, err := tx.Query(ctx, q,
		tenantID, in.DefaultWorkerModel, in.DefaultAskOrchiconModel,
		in.StallNoProgressWindowSeconds, in.StallNoFileDiffWindowSeconds,
		in.StallTextLoopWindowSeconds, in.StallRepetitionCount,
		in.StallRepetitionWindowSeconds, in.StallNudgeMax,
		in.StallNudgeReplyWindowSeconds, in.StallNudgeCooldownSeconds,
		in.DefaultBudgetOverrides,
		in.ExecutionReapGraceSeconds, in.ExecutionReapConsecutiveFailures,
		in.BackupSchedule, in.BackupRetentionDays, in.BackupDirectory,
		in.LogDirectory, in.LogMaxSizeMB, in.LogRollIntervalHours,
		in.LogRetentionDays, in.LogMaxFiles, in.MaxConcurrentRuns,
		in.MaxConcurrentRunsSet,
		in.SessionAccessTokenTtlSeconds, in.SessionRefreshTokenTtlSeconds,
		in.Budget.Tokens, in.Budget.CostUSD, in.Budget.ToolCallCount,
		in.Budget.WallClockSecs, in.Budget.CompactMaxTurns,
		in.Budget.WarnFracTokens, in.Budget.EscFracTokens, in.Budget.FinalFracTokens,
		in.Budget.WarnFracCostUSD, in.Budget.EscFracCostUSD, in.Budget.FinalFracCostUSD,
		in.Budget.WarnFracTools, in.Budget.EscFracTools, in.Budget.FinalFracTools,
		in.Budget.WarnFracTime, in.Budget.EscFracTime, in.Budget.FinalFracTime,
		in.Budget.WarnMsgTokens, in.Budget.EscMsgTokens, in.Budget.FinalMsgTokens,
		in.Budget.WarnMsgCostUSD, in.Budget.EscMsgCostUSD, in.Budget.FinalMsgCostUSD,
		in.Budget.WarnMsgTools, in.Budget.EscMsgTools, in.Budget.FinalMsgTools,
		in.Budget.WarnMsgTime, in.Budget.EscMsgTime, in.Budget.FinalMsgTime,
		in.Budget.CompactWarnTier, in.Budget.CompactEscalTier, in.Budget.CompactFinalTier,
	)
	if err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: update tenant settings: %w", err)
	}
	defer row.Close()
	if row.Next() {
		return scanTenantSettings(row)
	}
	return TenantSettingsRow{}, fmt.Errorf("db: update tenant settings: no row returned")
}

func scanTenantSettings(row pgx.Rows) (TenantSettingsRow, error) {
	var r TenantSettingsRow
	if err := row.Scan(
		&r.TenantID, &r.DefaultWorkerModel, &r.DefaultAskOrchiconModel,
		&r.StallNoProgressWindowSeconds, &r.StallNoFileDiffWindowSeconds,
		&r.StallTextLoopWindowSeconds, &r.StallRepetitionCount,
		&r.StallRepetitionWindowSeconds, &r.StallNudgeMax,
		&r.StallNudgeReplyWindowSeconds, &r.StallNudgeCooldownSeconds,
		&r.DefaultBudgetOverrides,
		&r.Budget.Tokens, &r.Budget.CostUSD, &r.Budget.ToolCallCount,
		&r.Budget.WallClockSecs, &r.Budget.CompactMaxTurns,
		&r.Budget.WarnFracTokens, &r.Budget.EscFracTokens, &r.Budget.FinalFracTokens,
		&r.Budget.WarnFracCostUSD, &r.Budget.EscFracCostUSD, &r.Budget.FinalFracCostUSD,
		&r.Budget.WarnFracTools, &r.Budget.EscFracTools, &r.Budget.FinalFracTools,
		&r.Budget.WarnFracTime, &r.Budget.EscFracTime, &r.Budget.FinalFracTime,
		&r.Budget.WarnMsgTokens, &r.Budget.EscMsgTokens, &r.Budget.FinalMsgTokens,
		&r.Budget.WarnMsgCostUSD, &r.Budget.EscMsgCostUSD, &r.Budget.FinalMsgCostUSD,
		&r.Budget.WarnMsgTools, &r.Budget.EscMsgTools, &r.Budget.FinalMsgTools,
		&r.Budget.WarnMsgTime, &r.Budget.EscMsgTime, &r.Budget.FinalMsgTime,
		&r.Budget.CompactWarnTier, &r.Budget.CompactEscalTier, &r.Budget.CompactFinalTier,
		&r.ExecutionReapGraceSeconds, &r.ExecutionReapConsecutiveFailures,
		&r.BackupSchedule, &r.BackupRetentionDays, &r.BackupDirectory,
		&r.LogDirectory, &r.LogMaxSizeMB, &r.LogRollIntervalHours,
		&r.LogRetentionDays, &r.LogMaxFiles,
		&r.MaxConcurrentRuns,
		&r.SessionAccessTokenTtlSeconds, &r.SessionRefreshTokenTtlSeconds,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: scan tenant settings: %w", err)
	}
	return r, nil
}
