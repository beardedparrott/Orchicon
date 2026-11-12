package askorchicon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

func toolListWorkflows(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		// IncludeEphemeral opts the caller into machine-managed transient workflows (Quick Work).
		IncludeEphemeral bool `json:"include_ephemeral"`
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
	workflows, err := db.ListWorkflows(ctx, ttx.Tx, db.ListWorkflowsFilter{
		TenantID:       tenantID,
		EphemeralScope: ephemeralScopeFor(params.IncludeEphemeral),
	})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(workflows))
	for _, w := range workflows {
		out = append(out, compactWorkflow(w))
	}
	return json.Marshal(newCompactList(out, "get_workflow"))
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

// toolGetWorkflowVersion returns a workflow version's steps JSON verbatim (defaulting to the latest
// published version).
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

// toolCreateWorkflow creates a workflow AND its first draft version-1 row in one transaction via
// the shared workflow.CreateWorkflowTx core (the service path's implementation).
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
		// Ephemeral marks the workflow machine-managed and transient (Quick Work).
		Ephemeral bool `json:"ephemeral"`
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
		Ephemeral:   params.Ephemeral,
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

// toolDeleteWorkflow permanently removes a workflow AND ITS ENTIRE RUN HISTORY, matching the
// DeleteWorkflow RPC (internal/workflow/service.go:324).
func toolDeleteWorkflow(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ID                string `json:"id"`
		ConfirmDeleteRuns bool   `json:"confirm_delete_runs"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return nil, fmt.Errorf("invalid args: %w", err)
	}
	if params.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	tenantID := tenant.FromContext(ctx)
	ttx, err := pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	defer ttx.Rollback(ctx)
	current, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, params.ID)
	if err != nil {
		return nil, err
	}
	if !current.Ephemeral && !params.ConfirmDeleteRuns {
		runs, err := db.ListWorkflowRuns(ctx, ttx.Tx, db.ListWorkflowRunsFilter{
			TenantID:   tenantID,
			WorkflowID: current.ID,
			PageSize:   1,
		})
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			return nil, fmt.Errorf("workflow %q has run history: deleting it permanently removes every run and "+
				"step run it has ever produced. This is not an ephemeral (Quick Work) workflow, so pass "+
				"confirm_delete_runs=true only if destroying that history is intended", current.Name)
		}
	}
	before := audit.Snapshot(map[string]any{
		"id":         current.ID,
		"name":       current.Name,
		"status":     current.Status,
		"type":       current.Type,
		"project_id": current.ProjectID,
		"ephemeral":  current.Ephemeral,
	})
	if err := db.DeleteWorkflow(ctx, ttx.Tx, tenantID, current.ID); err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "workflow.deleted", "workflow", current.ID, before, nil); err != nil {
		return nil, err
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"id": current.ID, "deleted": true, "ephemeral": current.Ephemeral})
}

// toolPublishWorkflowVersion publishes a workflow's draft version so the workflow becomes RUNNABLE,
// mirroring Service.PublishWorkflow (internal/workflow/service.go).
func toolPublishWorkflowVersion(ctx context.Context, pool *db.Pool, args json.RawMessage) (json.RawMessage, error) {
	var params struct {
		WorkflowID string `json:"workflow_id"`
		Version    int    `json:"version"`
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

	current, err := db.GetWorkflow(ctx, ttx.Tx, tenantID, params.WorkflowID)
	if err != nil {
		return nil, err
	}
	target := params.Version
	if target <= 0 {
		latest, err := db.GetLatestWorkflowVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, false)
		if err != nil {
			return nil, err
		}
		target = latest.Version
	}
	version, err := db.GetWorkflowVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, target)
	if err != nil {
		return nil, err
	}
	if version.Status != domain.WorkflowVersionDraft {
		return nil, fmt.Errorf("workflow version v%d is not a draft (status=%s) — only a draft can be published, and "+
			"a published version is already runnable", version.Version, version.Status)
	}
	published, err := db.PublishWorkflowVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, version.Version)
	if err != nil {
		return nil, err
	}
	// Deprecate the previously-current published version so exactly one version is active, matching the RPC
	// (a workflow with two 'published' versions would leave the run path's choice arbitrary).
	if current.CurrentVersion > 0 {
		if _, err := ttx.Tx.Exec(ctx,
			`UPDATE workflow_versions SET status = 'deprecated'
			 WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND status = 'published'`,
			tenantID, params.WorkflowID, current.CurrentVersion); err != nil {
			return nil, fmt.Errorf("deprecate previous published version: %w", err)
		}
	}
	updated, err := db.UpdateWorkflowCurrentVersion(ctx, ttx.Tx, tenantID, params.WorkflowID, current.Version, version.Version)
	if err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, ttx.Tx, tenantID, "workflow.published", "workflow", updated.ID,
		audit.Snapshot(map[string]any{"id": current.ID, "status": current.Status, "current_version": current.CurrentVersion}),
		audit.Snapshot(map[string]any{"id": updated.ID, "status": updated.Status, "current_version": updated.CurrentVersion})); err != nil {
		return nil, fmt.Errorf("audit workflow.published: %w", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		return nil, err
	}
	return rowWithExtra(updated, map[string]any{
		"version":    published.Version,
		"version_id": published.ID,
	})
}
