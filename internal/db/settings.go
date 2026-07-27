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
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

// GetTenantSettings returns the current settings for a tenant. If no row
// exists, it is created with default (zero) values and returned.
func GetTenantSettings(ctx context.Context, tx pgx.Tx, tenantID string) (TenantSettingsRow, error) {
	const q = `SELECT tenant_id, default_worker_model, default_ask_orchicon_model,
		stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
		stall_text_loop_window_seconds, stall_repetition_count,
		stall_repetition_window_seconds, created_at, updated_at
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
		stall_repetition_window_seconds, created_at, updated_at`
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
// values. Zero/nil fields are NOT updated (only non-zero values set).
// Returns the full row after update.
func UpdateTenantSettings(ctx context.Context, tx pgx.Tx, tenantID string, in TenantSettingsRow) (TenantSettingsRow, error) {
	const q = `INSERT INTO tenant_settings (
			tenant_id, default_worker_model, default_ask_orchicon_model,
			stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
			stall_text_loop_window_seconds, stall_repetition_count,
			stall_repetition_window_seconds, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (tenant_id) DO UPDATE SET
			default_worker_model = CASE WHEN $2 <> '' THEN $2 ELSE tenant_settings.default_worker_model END,
			default_ask_orchicon_model = CASE WHEN $3 <> '' THEN $3 ELSE tenant_settings.default_ask_orchicon_model END,
			stall_no_progress_window_seconds = CASE WHEN $4 <> 0 THEN $4 ELSE tenant_settings.stall_no_progress_window_seconds END,
			stall_no_file_diff_window_seconds = CASE WHEN $5 <> 0 THEN $5 ELSE tenant_settings.stall_no_file_diff_window_seconds END,
			stall_text_loop_window_seconds = CASE WHEN $6 <> 0 THEN $6 ELSE tenant_settings.stall_text_loop_window_seconds END,
			stall_repetition_count = CASE WHEN $7 <> 0 THEN $7 ELSE tenant_settings.stall_repetition_count END,
			stall_repetition_window_seconds = CASE WHEN $8 <> 0 THEN $8 ELSE tenant_settings.stall_repetition_window_seconds END,
			updated_at = now()
		RETURNING tenant_id, default_worker_model, default_ask_orchicon_model,
			stall_no_progress_window_seconds, stall_no_file_diff_window_seconds,
			stall_text_loop_window_seconds, stall_repetition_count,
			stall_repetition_window_seconds, created_at, updated_at`
	row, err := tx.Query(ctx, q,
		tenantID, in.DefaultWorkerModel, in.DefaultAskOrchiconModel,
		in.StallNoProgressWindowSeconds, in.StallNoFileDiffWindowSeconds,
		in.StallTextLoopWindowSeconds, in.StallRepetitionCount,
		in.StallRepetitionWindowSeconds,
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
		&r.StallRepetitionWindowSeconds, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return TenantSettingsRow{}, fmt.Errorf("db: scan tenant settings: %w", err)
	}
	return r, nil
}
