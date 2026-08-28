package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListCategories(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { TargetType string `json:"target_type"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if err := db.ValidateTargetType(in.TargetType); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	cats, err := db.ListCategories(ctx, tx.Tx, tenantID, in.TargetType); if err != nil { return nil, err }
	assigns, err := db.ListAssignments(ctx, tx.Tx, tenantID, in.TargetType); if err != nil { return nil, err }
	_ = tx.Commit(ctx)
	return json.Marshal(map[string]any{"categories": cats, "assignments": assigns})
}

func toolCreateCategory(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { TargetType string `json:"target_type"`; Name string `json:"name"`; Description string `json:"description"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if err := db.ValidateTargetType(in.TargetType); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	cat, err := db.CreateCategory(ctx, tx.Tx, tenantID, in.TargetType, in.Name, in.Description); if err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(cat)
}

func toolUpdateCategory(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { ID string `json:"id"`; Name string `json:"name"`; Description string `json:"description"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if in.ID == "" { return nil, fmt.Errorf("id required") }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	var np *string; if in.Name != "" { np = &in.Name }
	var dp *string; if in.Description != "" { dp = &in.Description }
	if np == nil && dp == nil { return nil, fmt.Errorf("name or description required") }
	cat, err := db.UpdateCategory(ctx, tx.Tx, tenantID, in.ID, np, dp); if err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(cat)
}

func toolDeleteCategory(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { ID string `json:"id"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	if err := db.DeleteCategory(ctx, tx.Tx, tenantID, in.ID); err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(map[string]string{"deleted": in.ID})
}

func toolAssignToCategory(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { CategoryID string `json:"category_id"`; EntityID string `json:"entity_id"`; TargetType string `json:"target_type"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if err := db.ValidateTargetType(in.TargetType); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	if err := db.AssignToCategory(ctx, tx.Tx, tenantID, in.TargetType, in.EntityID, in.CategoryID); err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(map[string]string{"assigned": in.EntityID})
}

func toolUnassignFromCategory(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { EntityID string `json:"entity_id"`; TargetType string `json:"target_type"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if err := db.ValidateTargetType(in.TargetType); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	if err := db.UnassignFromCategory(ctx, tx.Tx, tenantID, in.TargetType, in.EntityID); err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(map[string]string{"unassigned": in.EntityID})
}

func toolReorderCategories(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var in struct { TargetType string `json:"target_type"`; OrderedIDs []string `json:"ordered_ids"` }
	if err := json.Unmarshal(args, &in); err != nil { return nil, err }
	if err := db.ValidateTargetType(in.TargetType); err != nil { return nil, err }
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" { return nil, fmt.Errorf("no tenant in context") }
	tx, err := pool.BeginTenantTx(ctx, tenantID); if err != nil { return nil, err }
	defer tx.Rollback(ctx)
	if err := db.ReorderCategories(ctx, tx.Tx, tenantID, in.TargetType, in.OrderedIDs); err != nil { return nil, err }
	if err := tx.Commit(ctx); err != nil { return nil, err }
	return json.Marshal(map[string]any{"reordered": in.OrderedIDs})
}
