package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TenantSettingsRow is the in-memory representation of a tenant_settings row.
type TenantSettingsRow struct {
	TenantID                     string
	DefaultWorkerModel           string
	DefaultAskOrchiconModel      string
	StallNoProgressWindowSeconds  int64
	StallNoFileDiffWindowSeconds  int64
	StallTextLoopWindowSeconds    int64
	StallRepetitionCount          int32
	StallRepetitionWindowSeconds  int64
	DefaultBudgetOverrides        []byte // jsonb: default budget JSON (tokens/cost_usd/wall_clock_seconds/tool_call_count); a worker's budget_overrides overrides these
	ExecutionReapGraceSeconds     int64 // liveness reaper: min age before an execution is reaping-eligible
	ExecutionReapConsecutiveFailures int32 // liveness reaper: consecutive not-alive probes before reaping
	BackupSchedule               string // cron expression; empty = disabled
	BackupRetentionDays           int32  // 0 = keep all
	BackupDirectory              string // empty = default
	LogDirectory                 string // serve log dir; empty = env/code default
	LogMaxSizeMB                 int64  // max log file size before rotation (MB); 0 = default
	LogRollIntervalHours         int64  // time-based roll interval (hours); 0 = default
	LogRetentionDays             int32  // days rotated logs are kept; 0 = default
	LogMaxFiles                  int32  // max rotated log files kept; 0 = default
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

// GetTenantSettings returns the current settings for a tenant. If no row
// exists, it is created with default (zero) values and returned.
func GetTenantSettings(ctx context.Context, tx pgx.Tx, tenantID string) (TenantSettingsRow, error) {
	const q = `SELECT tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, default_budget_overrides,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
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
		stall_repetition_window_seconds, default_budget_overrides,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
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
	const q = `INSERT INTO tenant_settings (
		tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, default_budget_overrides,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		backup_schedule, backup_retention_days, backup_directory,
		log_directory, log_max_size_mb, log_roll_interval_hours,
		log_retention_days, log_max_files,
		updated_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, now())
	ON CONFLICT (tenant_id) DO UPDATE SET
		default_worker_model = CASE WHEN $2 <> '' THEN $2 ELSE tenant_settings.default_worker_model END,
		default_ask_orchicon_model = CASE WHEN $3 <> '' THEN $3 ELSE tenant_settings.default_ask_orchicon_model END,
		stall_no_progress_window_seconds = CASE WHEN $4 <> 0 THEN $4 ELSE tenant_settings.stall_no_progress_window_seconds END,
		stall_no_file_diff_window_seconds = CASE WHEN $5 <> 0 THEN $5 ELSE tenant_settings.stall_no_file_diff_window_seconds END,
		stall_text_loop_window_seconds = CASE WHEN $6 <> 0 THEN $6 ELSE tenant_settings.stall_text_loop_window_seconds END,
		stall_repetition_count = CASE WHEN $7 <> 0 THEN $7 ELSE tenant_settings.stall_repetition_count END,
		stall_repetition_window_seconds = CASE WHEN $8 <> 0 THEN $8 ELSE tenant_settings.stall_repetition_window_seconds END,
		default_budget_overrides = CASE WHEN $9 <> '{}'::jsonb THEN $9 ELSE tenant_settings.default_budget_overrides END,
		execution_reap_grace_seconds = CASE WHEN $10 <> 0 THEN $10 ELSE tenant_settings.execution_reap_grace_seconds END,
		execution_reap_consecutive_failures = CASE WHEN $11 <> 0 THEN $11 ELSE tenant_settings.execution_reap_consecutive_failures END,
		backup_schedule = CASE WHEN $12 <> '' THEN $12 ELSE tenant_settings.backup_schedule END,
		backup_retention_days = CASE WHEN $13 <> 0 THEN $13 ELSE tenant_settings.backup_retention_days END,
		backup_directory = CASE WHEN $14 <> '' THEN $14 ELSE tenant_settings.backup_directory END,
		log_directory = CASE WHEN $15 <> '' THEN $15 ELSE tenant_settings.log_directory END,
		log_max_size_mb = CASE WHEN $16 <> 0 THEN $16 ELSE tenant_settings.log_max_size_mb END,
		log_roll_interval_hours = CASE WHEN $17 <> 0 THEN $17 ELSE tenant_settings.log_roll_interval_hours END,
		log_retention_days = CASE WHEN $18 <> 0 THEN $18 ELSE tenant_settings.log_retention_days END,
		log_max_files = CASE WHEN $19 <> 0 THEN $19 ELSE tenant_settings.log_max_files END,
		updated_at = now()
	RETURNING tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, default_budget_overrides,
		execution_reap_grace_seconds, execution_reap_consecutive_failures,
		COALESCE(backup_schedule, ''), COALESCE(backup_retention_days, 0), COALESCE(backup_directory, ''),
		COALESCE(log_directory, ''), COALESCE(log_max_size_mb, 0), COALESCE(log_roll_interval_hours, 0),
		COALESCE(log_retention_days, 0), COALESCE(log_max_files, 0),
		created_at, updated_at`
	row, err := tx.Query(ctx, q,
		tenantID, in.DefaultWorkerModel, in.DefaultAskOrchiconModel,
		in.StallNoProgressWindowSeconds, in.StallNoFileDiffWindowSeconds,
		in.StallTextLoopWindowSeconds, in.StallRepetitionCount,
		in.StallRepetitionWindowSeconds, in.DefaultBudgetOverrides,
		in.ExecutionReapGraceSeconds, in.ExecutionReapConsecutiveFailures,
		in.BackupSchedule, in.BackupRetentionDays, in.BackupDirectory,
		in.LogDirectory, in.LogMaxSizeMB, in.LogRollIntervalHours,
		in.LogRetentionDays, in.LogMaxFiles,
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
		&r.StallRepetitionWindowSeconds, &r.DefaultBudgetOverrides,
		&r.ExecutionReapGraceSeconds, &r.ExecutionReapConsecutiveFailures,
		&r.BackupSchedule, &r.BackupRetentionDays, &r.BackupDirectory,
		&r.LogDirectory, &r.LogMaxSizeMB, &r.LogRollIntervalHours,
		&r.LogRetentionDays, &r.LogMaxFiles,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: scan tenant settings: %w", err)
	}
	return r, nil
}
