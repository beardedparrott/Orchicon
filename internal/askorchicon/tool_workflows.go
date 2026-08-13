package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func toolListWorkflows(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	workflows, err := db.ListWorkflows(ctx, ttx.Tx, db.ListWorkflowsFilter{
		TenantID: tenantID,
	})
	if err != nil {
		return nil, err
	}
	if workflows == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(workflows)
}

func toolGetWorkflow(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
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
	workflow, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(workflow)
}

// toolGetWorkflowVersion returns a workflow version's steps JSON verbatim
// (defaulting to the latest published version). Used to adopt a
// UI-built workflow configuration as the seed template: dump the steps,
// bake them into internal/db/seed_workflows.go, and rebuild.
func toolGetWorkflowVersion(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkflowID string `json:"workflow_id"`
		Version    *int   `json:"version"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.WorkflowID == "" {
		return nil, fmt.Errorf("workflow_id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	var ver db.WorkflowVersionRow
	if params.Version != nil {
		ver, err = db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, *params.Version)
	} else {
		ver, err = db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, true)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"workflow_id":  ver.WorkflowID,
		"version":      ver.Version,
		"version_note": ver.VersionNote,
		"status":       ver.Status,
		"steps":        json.RawMessage(ver.Steps),
	})
}

func toolCreateWorkflow(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	workflow, err := db.CreateWorkflow(ctx, ttx.Tx, db.WorkflowRow{
		ID:             db.NewID(),
		TenantID:       tenantID,
		Name:           params.Name,
		Status:         "draft",
		Type:           "one_shot",
		CurrentVersion: 0,
	})
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(workflow)
}
