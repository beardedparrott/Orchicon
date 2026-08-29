package workflow

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// CreateWorkflowInput is the full input for creating a workflow header
// plus its first draft version. Both the Connect service
// (Service.CreateWorkflow) and the AskOrchicon tool adapter
// (toolCreateWorkflow) funnel through ValidateCreateWorkflowInput +
// CreateWorkflowTx so there is exactly ONE implementation of "create a
// workflow" — the ghost-record bug (tool path committed a workflows row
// with no workflow_versions row and silently dropped steps) was two
// drifted implementations.
type CreateWorkflowInput struct {
	TenantID  string
	ProjectID string // optional; empty for tenant-level templates
	Name      string
	Type      string // "", domain.WorkflowTypeOneShot, or domain.WorkflowTypeTemplate; "" derives
	// GitStrategy is "", "local", "pr", or "none" (nil/inherit when empty).
	GitStrategy string
	VersionNote string
	Steps       string // JSON array string; "" becomes "[]"
	Inputs      string // JSON object string; "" becomes "{}"
	Outputs     string // JSON object string; "" becomes "{}"
}

// validateJSONString is the string-typed variant of validateJSONField used
// by CreateWorkflowInput's JSON members.
func validateJSONString(s, empty, field string) (string, error) {
	b, err := validateJSONField(s, empty, field, maxJSONFieldLen)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// validateStepsString is the string-typed variant of validateStepsField.
func validateStepsString(s string) (string, error) {
	b, err := validateStepsField(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ValidateCreateWorkflowInput validates and normalizes every field in
// place: the name is trimmed and required, the version note is
// bounds-checked, steps must be a JSON array (empty becomes "[]"),
// inputs/outputs must be JSON objects (empty becomes "{}"), type must be
// one_shot/template (or empty to derive), and git_strategy must be
// local/pr/none (or empty for inherit). Callers map the returned error to
// their surface's invalid-argument code (Connect handler) or return it
// as-is (tool). Idempotent: calling it twice is safe.
func ValidateCreateWorkflowInput(in *CreateWorkflowInput) error {
	name, err := validateName(in.Name)
	if err != nil {
		return err
	}
	in.Name = name
	if in.VersionNote, err = validateTextField(in.VersionNote, maxVersionNoteLen, "version_note"); err != nil {
		return err
	}
	if in.Steps, err = validateStepsString(in.Steps); err != nil {
		return err
	}
	if in.Inputs, err = validateJSONString(in.Inputs, "{}", "inputs"); err != nil {
		return err
	}
	if in.Outputs, err = validateJSONString(in.Outputs, "{}", "outputs"); err != nil {
		return err
	}
	switch in.Type {
	case "", domain.WorkflowTypeOneShot, domain.WorkflowTypeTemplate:
	default:
		return fmt.Errorf("type must be %q or %q", domain.WorkflowTypeOneShot, domain.WorkflowTypeTemplate)
	}
	switch in.GitStrategy {
	case "", "local", "pr", "none":
	default:
		return fmt.Errorf("git_strategy must be one of: local, pr, none")
	}
	return nil
}

// CreateWorkflowTx inserts the workflow header and its first draft
// version-1 row, enqueues the workflow.created outbox event, and writes
// the workflow.created audit row — all inside the caller's tenant
// transaction. It does NOT commit; the caller owns the transaction and
// must commit it so the version row, event, and audit row land atomically.
// Input must have passed ValidateCreateWorkflowInput. When ProjectID is
// set, the project must exist and be active.
func CreateWorkflowTx(ctx context.Context, tx pgx.Tx, in CreateWorkflowInput) (db.WorkflowRow, db.WorkflowVersionRow, error) {
	// Only active projects may host workflows (docs/02 §2.1).
	if in.ProjectID != "" {
		if err := db.RequireProjectActive(ctx, tx, in.TenantID, in.ProjectID); err != nil {
			return db.WorkflowRow{}, db.WorkflowVersionRow{}, fmt.Errorf("project not active: %w", err)
		}
	}

	workflowType := in.Type
	if workflowType == "" {
		if in.ProjectID == "" {
			workflowType = domain.WorkflowTypeTemplate
		} else {
			workflowType = domain.WorkflowTypeOneShot
		}
	}
	var gitStrategy *string
	if in.GitStrategy != "" {
		gitStrategy = &in.GitStrategy // validated in ValidateCreateWorkflowInput
	}

	workflowID := db.NewID()
	versionID := db.NewID()

	workflowRow := db.WorkflowRow{
		ID:             workflowID,
		TenantID:       in.TenantID,
		ProjectID:      in.ProjectID,
		Name:           in.Name,
		Type:           workflowType,
		Status:         domain.WorkflowDraft,
		CurrentVersion: 0,
		GitStrategy:    gitStrategy,
	}
	created, err := db.CreateWorkflow(ctx, tx, workflowRow)
	if err != nil {
		return db.WorkflowRow{}, db.WorkflowVersionRow{}, fmt.Errorf("create workflow: %w", err)
	}

	// First version is always version 1, in draft state (docs/02 §2.4).
	versionRow := db.WorkflowVersionRow{
		ID:          versionID,
		TenantID:    in.TenantID,
		WorkflowID:  workflowID,
		Version:     1,
		VersionNote: in.VersionNote,
		Status:      domain.WorkflowVersionDraft,
		Steps:       []byte(in.Steps),
		Inputs:      []byte(in.Inputs),
		Outputs:     []byte(in.Outputs),
	}
	createdVersion, err := db.CreateWorkflowVersion(ctx, tx, versionRow)
	if err != nil {
		return db.WorkflowRow{}, db.WorkflowVersionRow{}, fmt.Errorf("create workflow version: %w", err)
	}

	if err := enqueueWorkflowEvent(ctx, tx, "workflow.created", created, createdVersion, db.WorkflowRunRow{}, ""); err != nil {
		return db.WorkflowRow{}, db.WorkflowVersionRow{}, fmt.Errorf("enqueue workflow.created: %w", err)
	}
	if err := recordAudit(ctx, tx, in.TenantID, "workflow.created", "workflow", created.ID,
		nil, audit.Snapshot(workflowVersionAuditSnapshot(createdVersion))); err != nil {
		return db.WorkflowRow{}, db.WorkflowVersionRow{}, fmt.Errorf("audit workflow.created: %w", err)
	}
	return created, createdVersion, nil
}
