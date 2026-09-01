package worker

import (
	"context"
	"fmt"

	"github.com/beardedparrott/orchicon/internal/audit"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/jackc/pgx/v5"
)

// CreateWorkerInput is the full input for creating a worker header plus
// its first draft version. Both the Connect service (Service.CreateWorker)
// and the AskOrchicon tool adapter (toolCreateWorker) funnel through
// ValidateCreateWorkerInput + CreateWorkerTx so there is exactly ONE
// implementation of "create a worker" — the ghost-record bug (tool path
// committed a workers row with no worker_versions row and dropped
// model_ref/runtime_ref) was two drifted implementations.
type CreateWorkerInput struct {
	TenantID     string
	Name         string
	Slug         string // optional; derived from Name when empty
	Description  string
	Purpose      string
	RoleRef      string // RBAC role binding; empty = no plane access (deny-by-default)
	VersionNote  string
	RuntimeRef   string
	ModelRef     string
	Role         string
	Skills       string
	Behavior     string
	AgentsMD     string
	SystemPrompt string // raw prompt; used only when no structured field is set

	// JSON-encoded fields (validated; empty becomes the canonical default).
	ContextSources  string // JSON array
	Permissions     string // JSON object
	GatedTools      string // JSON array
	BudgetOverrides string // JSON object
	Labels          string // JSON object

	ExecutionPolicyRef  string
	ConcurrencyLimit    int
	RecoveryWorkflowRef string
}

// validateJSONString is the string-typed variant of validateJSONField used
// by CreateWorkerInput's JSON members.
func validateJSONString(s, empty, field string) (string, error) {
	b, err := validateJSONField(s, empty, field, maxJSONFieldLen)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ValidateCreateWorkerInput validates and normalizes every field in place:
// the name is trimmed and required, the slug is derived from the name when
// empty, text fields are trimmed and bounds-checked, JSON fields must be
// valid JSON (empty becomes the canonical "[]"/"{}"), and a negative
// concurrency limit clamps to 0. Callers map the returned error to their
// surface's invalid-argument code (Connect handler) or return it as-is
// (tool). Idempotent: calling it twice is safe.
func ValidateCreateWorkerInput(in *CreateWorkerInput) error {
	name, err := validateName(in.Name)
	if err != nil {
		return err
	}
	in.Name = name
	slug, err := normalizeSlug(in.Slug, in.Name)
	if err != nil {
		return err
	}
	in.Slug = slug
	if in.Description, err = validateTextField(in.Description, maxDescLen, "description"); err != nil {
		return err
	}
	if in.Purpose, err = validateTextField(in.Purpose, maxPurposeLen, "purpose"); err != nil {
		return err
	}
	if in.RoleRef, err = validateTextField(in.RoleRef, maxNameLen, "role_ref"); err != nil {
		return err
	}
	if in.VersionNote, err = validateTextField(in.VersionNote, maxVersionNoteLen, "version_note"); err != nil {
		return err
	}
	if in.RuntimeRef, err = validateTextField(in.RuntimeRef, maxNameLen, "runtime_ref"); err != nil {
		return err
	}
	if in.ModelRef, err = validateModelRef(in.ModelRef); err != nil {
		return err
	}
	if in.SystemPrompt, err = validateTextField(in.SystemPrompt, maxPromptLen, "system_prompt"); err != nil {
		return err
	}
	if in.Role, err = validateTextField(in.Role, maxPromptLen, "role"); err != nil {
		return err
	}
	if in.Skills, err = validateTextField(in.Skills, maxPromptLen, "skills"); err != nil {
		return err
	}
	if in.Behavior, err = validateTextField(in.Behavior, maxPromptLen, "behavior"); err != nil {
		return err
	}
	if in.AgentsMD, err = validateTextField(in.AgentsMD, maxPromptLen, "agents_md"); err != nil {
		return err
	}
	if in.ContextSources, err = validateJSONString(in.ContextSources, "[]", "context_sources"); err != nil {
		return err
	}
	if in.Permissions, err = validateJSONString(in.Permissions, "{}", "permissions"); err != nil {
		return err
	}
	if in.GatedTools, err = validateJSONString(in.GatedTools, "[]", "gated_tools"); err != nil {
		return err
	}
	if in.BudgetOverrides, err = validateJSONString(in.BudgetOverrides, "{}", "budget_overrides"); err != nil {
		return err
	}
	if in.Labels, err = validateJSONString(in.Labels, "{}", "labels"); err != nil {
		return err
	}
	if in.ExecutionPolicyRef, err = validateTextField(in.ExecutionPolicyRef, maxNameLen, "execution_policy_ref"); err != nil {
		return err
	}
	if in.RecoveryWorkflowRef, err = validateTextField(in.RecoveryWorkflowRef, maxNameLen, "recovery_workflow_ref"); err != nil {
		return err
	}
	if in.ConcurrencyLimit < 0 {
		in.ConcurrencyLimit = 0
	}
	return nil
}

// CreateWorkerTx inserts the worker header and its first draft version-1
// row, enqueues the worker.created outbox event, and writes the
// worker.created audit row — all inside the caller's tenant transaction.
// It does NOT commit; the caller owns the transaction and must commit it
// so the version row, event, and audit row land atomically. Input must
// have passed ValidateCreateWorkerInput (the slug is deduped here against
// the tenant: -2, -3, ... until free).
func CreateWorkerTx(ctx context.Context, tx pgx.Tx, in CreateWorkerInput) (db.WorkerRow, db.WorkerVersionRow, error) {
	// Dedupe the slug against the tenant so clones and re-created workers
	// never hit the unique workers_tenant_slug_idx constraint.
	slug, err := uniqueSlug(ctx, tx, in.TenantID, in.Slug)
	if err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("dedupe slug: %w", err)
	}

	workerID := db.NewID()
	versionID := db.NewID()

	workerRow := db.WorkerRow{
		ID:             workerID,
		TenantID:       in.TenantID,
		Name:           in.Name,
		Slug:           slug,
		Description:    in.Description,
		Purpose:        in.Purpose,
		RoleRef:        in.RoleRef,
		Status:         domain.WorkerDraft,
		CurrentVersion: 0,
		CreatedBy:      "", // populated when auth lands (Phase 9)
	}
	created, err := db.CreateWorker(ctx, tx, workerRow)
	if err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("create worker: %w", err)
	}

	// Structured prompt fields are the source of truth for the version; the
	// composed system_prompt is derived so the DB column always matches what
	// dispatch would send. Legacy callers that only supply system_prompt keep
	// working (structured fields stay empty and the raw prompt is stored).
	versionRow := db.WorkerVersionRow{
		ID:                  versionID,
		TenantID:            in.TenantID,
		WorkerID:            workerID,
		Version:             1,
		VersionNote:         in.VersionNote,
		Status:              domain.WorkerVersionDraft,
		RuntimeRef:          in.RuntimeRef,
		ModelRef:            in.ModelRef,
		Role:                in.Role,
		Skills:              in.Skills,
		Behavior:            in.Behavior,
		AgentsMD:            in.AgentsMD,
		ContextSources:      []byte(in.ContextSources),
		Permissions:         []byte(in.Permissions),
		GatedTools:          []byte(in.GatedTools),
		BudgetOverrides:     []byte(in.BudgetOverrides),
		ExecutionPolicyRef:  in.ExecutionPolicyRef,
		ConcurrencyLimit:    in.ConcurrencyLimit,
		RecoveryWorkflowRef: in.RecoveryWorkflowRef,
		Labels:              []byte(in.Labels),
	}
	if in.Role == "" && in.Skills == "" && in.Behavior == "" && in.AgentsMD == "" {
		versionRow.SystemPrompt = in.SystemPrompt
	} else {
		versionRow.SystemPrompt = composeWorkerPrompt(versionRow)
	}
	createdVersion, err := db.CreateWorkerVersion(ctx, tx, versionRow)
	if err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("create worker version: %w", err)
	}

	if err := enqueueWorkerEvent(ctx, tx, "worker.created", created, createdVersion); err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("enqueue worker.created: %w", err)
	}
	if err := recordAudit(ctx, tx, in.TenantID, "worker.created", "worker", created.ID,
		nil, audit.Snapshot(map[string]any{
			"id": created.ID, "name": created.Name, "slug": created.Slug, "status": created.Status,
			"version": createdVersion.Version, "model_ref": createdVersion.ModelRef,
		})); err != nil {
		return db.WorkerRow{}, db.WorkerVersionRow{}, fmt.Errorf("audit worker.created: %w", err)
	}
	return created, createdVersion, nil
}
