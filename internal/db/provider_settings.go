package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProviderSettingsRow is the data-access shape of a provider_settings row
// (ADR-0006 D1). Built-in overrides and tenant custom providers share the
// table, discriminated by IsCustom.
type ProviderSettingsRow struct {
	ID              string
	TenantID        string
	ProviderID      string
	Enabled         bool
	BaseURLOverride string
	BaseURL         string
	AuthMode        string
	NumCtxDefault   int64
	HiddenModels    []string
	ManualModels    []byte // jsonb: [{id, context, maxOutput, reasoning}]
	DisplayName     string
	IsCustom        bool
	CreatedAt       time.Time // timestamptz — must scan into time.Time, not string
	UpdatedAt       time.Time
}

const providerSettingsCols = `id, tenant_id, provider_id, enabled, base_url_override, base_url,
	auth_mode, num_ctx_default, hidden_models, manual_models, display_name, is_custom,
	created_at, updated_at`

func scanProviderSettings(row pgx.Row) (ProviderSettingsRow, error) {
	var r ProviderSettingsRow
	var hidden []byte
	err := row.Scan(&r.ID, &r.TenantID, &r.ProviderID, &r.Enabled, &r.BaseURLOverride, &r.BaseURL,
		&r.AuthMode, &r.NumCtxDefault, &hidden, &r.ManualModels, &r.DisplayName, &r.IsCustom,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	if len(hidden) > 0 {
		if err := json.Unmarshal(hidden, &r.HiddenModels); err != nil {
			return r, fmt.Errorf("db: scan provider settings: hidden_models: %w", err)
		}
	}
	return r, nil
}

// GetProviderSettings returns one row (ErrNotFound when absent).
func GetProviderSettings(ctx context.Context, tx pgx.Tx, tenantID, providerID string) (ProviderSettingsRow, error) {
	const q = `SELECT ` + providerSettingsCols + ` FROM provider_settings WHERE tenant_id=$1 AND provider_id=$2`
	r, err := scanProviderSettings(tx.QueryRow(ctx, q, tenantID, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderSettingsRow{}, ErrNotFound
	}
	if err != nil {
		return ProviderSettingsRow{}, fmt.Errorf("db: get provider settings: %w", err)
	}
	return r, nil
}

// ListProviderSettings returns every stored row for the tenant.
func ListProviderSettings(ctx context.Context, tx pgx.Tx, tenantID string) ([]ProviderSettingsRow, error) {
	const q = `SELECT ` + providerSettingsCols + ` FROM provider_settings WHERE tenant_id=$1 ORDER BY is_custom ASC, provider_id ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list provider settings: %w", err)
	}
	defer rows.Close()
	var out []ProviderSettingsRow
	for rows.Next() {
		r, err := scanProviderSettings(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list provider settings: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertProviderSettings inserts or updates one (tenant, provider) row.
func UpsertProviderSettings(ctx context.Context, tx pgx.Tx, r ProviderSettingsRow) (ProviderSettingsRow, error) {
	hidden, err := json.Marshal(orEmptyList(r.HiddenModels))
	if err != nil {
		return ProviderSettingsRow{}, fmt.Errorf("db: upsert provider settings: %w", err)
	}
	manual := r.ManualModels
	if len(manual) == 0 {
		manual = []byte("[]")
	}
	const q = `INSERT INTO provider_settings
		(id, tenant_id, provider_id, enabled, base_url_override, base_url,
		 auth_mode, num_ctx_default, hidden_models, manual_models, display_name, is_custom)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb,$11,$12)
		ON CONFLICT (tenant_id, provider_id) DO UPDATE SET
			enabled           = EXCLUDED.enabled,
			base_url_override = EXCLUDED.base_url_override,
			base_url          = EXCLUDED.base_url,
			auth_mode         = EXCLUDED.auth_mode,
			num_ctx_default   = EXCLUDED.num_ctx_default,
			hidden_models     = EXCLUDED.hidden_models,
			manual_models     = EXCLUDED.manual_models,
			display_name      = EXCLUDED.display_name,
			is_custom         = EXCLUDED.is_custom,
			updated_at        = now()
		RETURNING ` + providerSettingsCols
	row, err := scanProviderSettings(tx.QueryRow(ctx, q,
		r.ID, r.TenantID, r.ProviderID, r.Enabled, r.BaseURLOverride, r.BaseURL,
		r.AuthMode, r.NumCtxDefault, string(hidden), string(manual), r.DisplayName, r.IsCustom))
	if err != nil {
		return ProviderSettingsRow{}, fmt.Errorf("db: upsert provider settings: %w", err)
	}
	return row, nil
}

// UpdateProviderSettingsBuiltIn applies a partial update to a built-in
// override row, creating it on first change. Zero-value fields keep the
// current column (COALESCE pattern); $4 disambiguates "enabled=false" from
// "leave enabled alone" and $5 does the same for num_ctx_default.
func UpdateProviderSettingsBuiltIn(ctx context.Context, tx pgx.Tx, tenantID, providerID string, enabled *bool, baseURL *string, numCtx *int64, hidden []string, replaceHidden bool) (ProviderSettingsRow, error) {
	hiddenJSON := "null"
	if replaceHidden {
		b, err := json.Marshal(orEmptyList(hidden))
		if err != nil {
			return ProviderSettingsRow{}, fmt.Errorf("db: update provider settings: %w", err)
		}
		hiddenJSON = string(b)
	}
	const q = `INSERT INTO provider_settings
		(id, tenant_id, provider_id, enabled, base_url_override, base_url,
		 auth_mode, num_ctx_default, hidden_models, manual_models, display_name, is_custom)
		VALUES ($1,$2,$3, COALESCE($4, true), COALESCE($5, ''), '', '',
			COALESCE($6, 0), COALESCE($7::jsonb, '[]'::jsonb), '[]'::jsonb, '', false)
		ON CONFLICT (tenant_id, provider_id) DO UPDATE SET
			enabled           = COALESCE($4, provider_settings.enabled),
			base_url_override = COALESCE($5, provider_settings.base_url_override),
			num_ctx_default   = COALESCE($6, provider_settings.num_ctx_default),
			hidden_models     = COALESCE($7::jsonb, provider_settings.hidden_models),
			updated_at        = now()
		RETURNING ` + providerSettingsCols
	row, err := scanProviderSettings(tx.QueryRow(ctx, q,
		NewID(), tenantID, providerID, enabled, baseURL, numCtx, hiddenJSON))
	if err != nil {
		return ProviderSettingsRow{}, fmt.Errorf("db: update provider settings: %w", err)
	}
	return row, nil
}

// DeleteProviderSettings removes one (tenant, provider) row.
func DeleteProviderSettings(ctx context.Context, tx pgx.Tx, tenantID, providerID string) error {
	const q = `DELETE FROM provider_settings WHERE tenant_id=$1 AND provider_id=$2`
	ct, err := tx.Exec(ctx, q, tenantID, providerID)
	if err != nil {
		return fmt.Errorf("db: delete provider settings: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WorkerModelRefRow is one (worker, model_ref) pair for the deletion guard.
type WorkerModelRefRow struct {
	WorkerID   string
	WorkerName string
	ModelRef   string
}

// ListWorkerModelRefs returns every model_ref the tenant's workers carry,
// one row per worker/version pair (de-duplicated in the caller).
func ListWorkerModelRefs(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkerModelRefRow, error) {
	const q = `SELECT DISTINCT w.id, w.name, wv.model_ref
		FROM worker_versions wv JOIN workers w ON w.id = wv.worker_id AND w.tenant_id = wv.tenant_id
		WHERE wv.tenant_id = $1 AND wv.model_ref <> ''`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list worker model refs: %w", err)
	}
	defer rows.Close()
	var out []WorkerModelRefRow
	for rows.Next() {
		var r WorkerModelRefRow
		if err := rows.Scan(&r.WorkerID, &r.WorkerName, &r.ModelRef); err != nil {
			return nil, fmt.Errorf("db: list worker model refs: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetTenantDefaultModelRefs returns the tenant's default worker and
// Ask-Orchicon model refs (either may be empty).
func GetTenantDefaultModelRefs(ctx context.Context, tx pgx.Tx, tenantID string) (defaultWorker, defaultAsk string, err error) {
	const q = `SELECT COALESCE(default_worker_model, ''), COALESCE(default_ask_orchicon_model, '') FROM tenant_settings WHERE tenant_id=$1`
	err = tx.QueryRow(ctx, q, tenantID).Scan(&defaultWorker, &defaultAsk)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("db: get tenant default model refs: %w", err)
	}
	return defaultWorker, defaultAsk, nil
}

func orEmptyList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
