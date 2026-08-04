package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListRuntimeImages(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	rows, err := db.ListRuntimeImages(ctx, ttx.Tx, db.RuntimeImageFilter{TenantID: tenantID})
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(rows)
}

func toolGetRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
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
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func toolCreateRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		AptPackages []string `json:"apt_packages"`
		Toolchains  []string `json:"toolchains"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if params.Slug == "" {
		return nil, fmt.Errorf("slug is required")
	}
	aptJSON, _ := json.Marshal(params.AptPackages)
	toolJSON, _ := json.Marshal(params.Toolchains)

	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	row, err := db.CreateRuntimeImage(ctx, ttx.Tx, db.RuntimeImageRow{
		ID:          db.NewID(),
		TenantID:    tenantID,
		Name:        params.Name,
		Slug:        params.Slug,
		Description: params.Description,
		AptPackages: aptJSON,
		Toolchains:  toolJSON,
		Env:         []byte("{}"),
		Status:      "draft",
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(row)
}

func toolDeleteRuntimeImage(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
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
	row, err := db.GetRuntimeImage(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if err := db.DeleteRuntimeImage(ctx, ttx.Tx, tenantID, params.ID); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(row)
}
