package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
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

// toolCreateWorkflow creates a workflow AND its first draft version-1
// row in one transaction via the shared workflow.CreateWorkflowTx core
// (the service path's implementation), seeding steps when provided and
// writing the workflow.created audit row. The workflow is immediately
// editable and publishable from the UI.
func toolCreateWorkflow(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		VersionNote string          `json:"version_note"`
		Steps       json.RawMessage `json:"steps"`
		Inputs      json.RawMessage `json:"inputs"`
		Outputs     json.RawMessage `json:"outputs"`
		Type        string          `json:"type"`
		GitStrategy string          `json:"git_strategy"`
		ProjectID   string          `json:"project_id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(params.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	// A present-but-null JSON argument must behave like an absent one.
	rawJSON := func(raw json.RawMessage) string {
		if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
			return s
		}
		return ""
	}
	// Workflows have no description column; `description` seeds the draft
	// version-1 version_note when version_note is empty.
	versionNote := params.VersionNote
	if versionNote == "" {
		versionNote = params.Description
	}
	tenantID := tenant.FromContext(ctx)
	in := workflow.CreateWorkflowInput{
		TenantID:    tenantID,
		ProjectID:   strings.TrimSpace(params.ProjectID),
		Name:        params.Name,
		Type:        strings.ToLower(strings.TrimSpace(params.Type)),
		GitStrategy: strings.ToLower(strings.TrimSpace(params.GitStrategy)),
		VersionNote: versionNote,
		Steps:       rawJSON(params.Steps),
		Inputs:      rawJSON(params.Inputs),
		Outputs:     rawJSON(params.Outputs),
	}
	if err := workflow.ValidateCreateWorkflowInput(&in); err != nil {
		return nil, err
	}
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	created, createdVersion, err := workflow.CreateWorkflowTx(ctx, ttx.Tx, in)
	if err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return rowWithExtra(created, map[string]any{
		"version":    createdVersion.Version,
		"version_id": createdVersion.ID,
	})
}
