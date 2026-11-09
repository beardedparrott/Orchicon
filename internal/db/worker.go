package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkerRow is the data-access shape of a workers table row — the immutable header (docs/05 §3.1,
// docs/09 §3.3).
type WorkerRow struct {
	ID             string
	TenantID       string
	Name           string
	Slug           string
	Description    string
	Purpose        string
	RoleRef        string
	Status         string
	CurrentVersion int
	CreatedBy      string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Ephemeral marks a machine-managed transient worker (Ask Orchicon Quick Work): created for one
	// job, pinned to the Quick Work agent's own model_ref, hidden from the Workers view.
	Ephemeral bool
}

// WorkerVersionRow is the data-access shape of a worker_versions table row - the mutable snapshot
// of a Worker at a specific version.
type WorkerVersionRow struct {
	ID          string
	TenantID    string
	WorkerID    string
	Version     int
	VersionNote string
	Status      string
	ModelRef    string
	// SystemPrompt is the LEGACY RAW PROMPT: one pre-composed prompt, used only when all four
	// structured fields below are blank.
	SystemPrompt   string
	Role           string
	Skills         string
	Behavior       string
	AgentsMD       string
	ContextSources []byte // jsonb
	// SkillFiles is the jsonb array of absolute skill file/directory paths for THIS VERSION (never the
	// worker header: published versions are immutable by version and the version is what every.
	SkillFiles          []byte // jsonb
	Permissions         []byte // jsonb
	GatedTools          []byte // jsonb
	BudgetOverrides     []byte // jsonb
	ExecutionPolicyRef  string
	ConcurrencyLimit    int
	RecoveryWorkflowRef string
	Labels              []byte // jsonb
	PublishedAt         *time.Time
	CreatedAt           time.Time
}

// WorkerSlugExists reports whether a worker with the given slug already exists in the tenant.
func WorkerSlugExists(ctx context.Context, tx pgx.Tx, tenantID, slug string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM workers WHERE tenant_id = $1 AND slug = $2)`,
		tenantID, slug).Scan(&exists)
	return exists, err
}

// CreateWorker inserts a new worker header row within the given tenant transaction.
func CreateWorker(ctx context.Context, tx pgx.Tx, w WorkerRow) (WorkerRow, error) {
	const q = `INSERT INTO workers
		(id, tenant_id, name, slug, description, purpose, role_ref, status, current_version, created_by, ephemeral)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, name, slug, description, purpose, role_ref, status,
			current_version, created_by, version, created_at, updated_at, ephemeral`
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.Name, w.Slug, w.Description, w.Purpose,
		w.RoleRef, w.Status, w.CurrentVersion, w.CreatedBy, w.Ephemeral,
	).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.Slug, &row.Description,
		&row.Purpose, &row.RoleRef, &row.Status, &row.CurrentVersion, &row.CreatedBy,
		&row.Version, &row.CreatedAt, &row.UpdatedAt, &row.Ephemeral,
	)
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: create worker: %w", err)
	}
	return row, nil
}

// GetWorker fetches a single worker by id within the tenant scope.
func GetWorker(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkerRow, error) {
	const q = `SELECT id, tenant_id, name, slug, description, purpose, role_ref, status,
		current_version, created_by, version, created_at, updated_at, ephemeral
		FROM workers WHERE id = $1 AND tenant_id = $2`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose, &w.RoleRef,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt, &w.Ephemeral,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: get worker: %w", err)
	}
	return w, nil
}

// ListWorkersFilter scopes a list query to a tenant, optionally
// filtered by status, search text, and sort.
type ListWorkersFilter struct {
	TenantID  string
	Status    string // empty = all statuses
	Search    string
	SortBy    string
	SortOrder string
	PageSize  int
	AfterID   string
	// EphemeralScope scopes the ephemeral split (Ask Orchicon Quick Work): ""/"exclude" (the default) =
	// ONLY ordinary workers — the Workers view; "only" = only ephemeral workers; "include" = both.
	EphemeralScope string
}

// Page-size bounds, EXPORTED so a caller can tell whether a page came back FULL.
const (
	DefaultListPageSize = 100
	MaxListPageSize     = 1000
)

// WorkerListRow is the enriched list row — the Worker header plus the active version's model_ref
// and status.
type WorkerListRow struct {
	WorkerRow
	ActiveModelRef      string
	ActiveVersionStatus string
}

// ListWorkersWithActiveVersion returns a page of workers with the active version's model_ref and
// publish state in one round-trip.
func ListWorkersWithActiveVersion(ctx context.Context, tx pgx.Tx, f ListWorkersFilter) ([]WorkerListRow, error) {
	if f.PageSize <= 0 || f.PageSize > MaxListPageSize {
		f.PageSize = DefaultListPageSize
	}
	args := []any{f.TenantID}
	where := `w.tenant_id = $1`
	// Qualified: this query joins worker_versions, and `ephemeral` is now a
	// column on more than one table, so the bare form would be ambiguous.
	where += ephemeralPredicateOn("w", f.EphemeralScope)
	idx := 2
	if f.Search != "" {
		where += fmt.Sprintf(` AND (w.name ILIKE $%d OR w.slug ILIKE $%d OR w.purpose ILIKE $%d)`, idx, idx, idx)
		args = append(args, "%"+f.Search+"%")
		idx++
	}
	if f.Status != "" {
		where += fmt.Sprintf(` AND w.status = $%d`, idx)
		args = append(args, f.Status)
		idx++
	}
	sortBy := "w.created_at"
	sortCol := "created_at"
	if f.SortBy == "name" {
		sortBy, sortCol = "w.name", "name"
	} else if f.SortBy == "status" {
		sortBy, sortCol = "w.status", "status"
	}
	sortOrder := "ASC"
	if f.SortOrder == "desc" {
		sortOrder = "DESC"
	}
	// THE CURSOR MUST AGREE WITH THE ORDER — the SAME correction ListWorkers already carries, applied
	// here because THIS is the function the Workers pane calls.
	if f.AfterID != "" {
		cmp := ">"
		if sortOrder == "DESC" {
			cmp = "<"
		}
		where += fmt.Sprintf(` AND (%s, w.id) %s (
			SELECT w2.%s, w2.id FROM workers w2
			WHERE w2.tenant_id = $1 AND w2.id = $%d)`, sortBy, cmp, sortCol, idx)
		args = append(args, f.AfterID)
		idx++
	}
	q := fmt.Sprintf(`SELECT w.id, w.tenant_id, w.name, w.slug, w.description, w.purpose, w.role_ref, w.status,
		w.current_version, w.created_by, w.version, w.created_at, w.updated_at,
		w.ephemeral,
		COALESCE(v.model_ref, ''), COALESCE(v.status, '')
		FROM workers w
		LEFT JOIN LATERAL (
			SELECT model_ref, status, version FROM worker_versions
			WHERE tenant_id = w.tenant_id AND worker_id = w.id
				AND ((w.current_version > 0 AND version = w.current_version) OR w.current_version = 0)
			ORDER BY version DESC LIMIT 1
		) v ON true
		WHERE %s
		ORDER BY %s %s, w.id %s LIMIT $%d`, where, sortBy, sortOrder, sortOrder, idx)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list workers with active version: %w", err)
	}
	defer rows.Close()
	var out []WorkerListRow
	for rows.Next() {
		var r WorkerListRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Slug, &r.Description,
			&r.Purpose, &r.RoleRef, &r.Status, &r.CurrentVersion, &r.CreatedBy, &r.Version,
			&r.CreatedAt, &r.UpdatedAt, &r.Ephemeral, &r.ActiveModelRef, &r.ActiveVersionStatus); err != nil {
			return nil, fmt.Errorf("db: scan worker list row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListWorkers returns a page of workers for the tenant with cursor-based
// pagination, optional search/filter, and configurable sort.
func ListWorkers(ctx context.Context, tx pgx.Tx, f ListWorkersFilter) ([]WorkerRow, error) {
	if f.PageSize <= 0 || f.PageSize > MaxListPageSize {
		f.PageSize = DefaultListPageSize
	}
	args := []any{f.TenantID}
	where := `tenant_id = $1`
	where += ephemeralPredicateOn("", f.EphemeralScope)
	idx := 2
	if f.Search != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR slug ILIKE $%d OR purpose ILIKE $%d)`, idx, idx, idx)
		args = append(args, "%"+f.Search+"%")
		idx++
	}
	if f.Status != "" {
		where += fmt.Sprintf(` AND status = $%d`, idx)
		args = append(args, f.Status)
		idx++
	}
	sortBy := "created_at"
	if f.SortBy == "name" || f.SortBy == "status" {
		sortBy = f.SortBy
	}
	sortOrder := "ASC"
	if f.SortOrder == "desc" {
		sortOrder = "DESC"
	}
	// THE CURSOR MUST AGREE WITH THE ORDER — the same correction as ListWorkItems and ListProjects.
	if f.AfterID != "" {
		cmp := ">"
		if sortOrder == "DESC" {
			cmp = "<"
		}
		where += fmt.Sprintf(` AND (%s, id) %s (
			SELECT w2.%s, w2.id FROM workers w2
			WHERE w2.tenant_id = $1 AND w2.id = $%d)`, sortBy, cmp, sortBy, idx)
		args = append(args, f.AfterID)
		idx++
	}
	q := fmt.Sprintf(`SELECT id, tenant_id, name, slug, description, purpose, role_ref, status,
		current_version, created_by, version, created_at, updated_at, ephemeral
		FROM workers
		WHERE %s
		ORDER BY %s %s, id %s LIMIT $%d`, where, sortBy, sortOrder, sortOrder, idx)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list workers: %w", err)
	}
	defer rows.Close()
	var out []WorkerRow
	for rows.Next() {
		var w WorkerRow
		if err := rows.Scan(&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description,
			&w.Purpose, &w.RoleRef, &w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
			&w.CreatedAt, &w.UpdatedAt, &w.Ephemeral); err != nil {
			return nil, fmt.Errorf("db: scan worker: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkerStatus transitions a worker's status with optimistic concurrency.
func UpdateWorkerStatus(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, status string) (WorkerRow, error) {
	const q = `UPDATE workers
		SET status = $4, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, description, purpose, role_ref, status,
			current_version, created_by, version, created_at, updated_at, ephemeral`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose, &w.RoleRef,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt, &w.Ephemeral,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: update worker status: %w", err)
	}
	return w, nil
}

// UpdateWorkerFields is a partial update for worker header fields.
type UpdateWorkerFields struct {
	Name        *string
	Description *string
	Purpose     *string
	RoleRef     *string
}

// UpdateWorker applies a partial update to worker header fields with optimistic concurrency. WHY
// HEADER TEXT IS NO LONGER DRAFT-ONLY.
func UpdateWorker(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateWorkerFields) (WorkerRow, error) {
	q := `UPDATE workers SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Name != nil {
		q += fmt.Sprintf(`, name = $%d`, setIdx)
		args = append(args, *f.Name)
		setIdx++
	}
	if f.Description != nil {
		q += fmt.Sprintf(`, description = $%d`, setIdx)
		args = append(args, *f.Description)
		setIdx++
	}
	if f.Purpose != nil {
		q += fmt.Sprintf(`, purpose = $%d`, setIdx)
		args = append(args, *f.Purpose)
		setIdx++
	}
	if f.RoleRef != nil {
		q += fmt.Sprintf(`, role_ref = $%d`, setIdx)
		args = append(args, *f.RoleRef)
		setIdx++
	}
	onlyRole := f.Name == nil && f.Description == nil && f.Purpose == nil
	q += fmt.Sprintf(` WHERE tenant_id = $1 AND id = $2 AND version = $3 AND (status <> 'retired' OR $%d = true)`, setIdx)
	args = append(args, onlyRole)
	q += ` RETURNING id, tenant_id, name, slug, description, purpose, role_ref, status,
		current_version, created_by, version, created_at, updated_at, ephemeral`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose, &w.RoleRef,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt, &w.Ephemeral,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: update worker: %w", err)
	}
	return w, nil
}

// UpdateWorkerCurrentVersion bumps the current_version pointer to the
// newly published version. Uses optimistic concurrency on the header row.
func UpdateWorkerCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion, newVersion int) (WorkerRow, error) {
	const q = `UPDATE workers
		SET current_version = $4, status = 'published', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, description, purpose, role_ref, status,
			current_version, created_by, version, created_at, updated_at, ephemeral`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, newVersion).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose, &w.RoleRef,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt, &w.Ephemeral,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: update worker current_version: %w", err)
	}
	return w, nil
}

// jsonbOrDefault returns a non-NULL jsonb literal for a nil field, so an
// unset jsonb column takes the schema default instead of violating NOT NULL.
func jsonbOrDefault(v []byte, def string) []byte {
	if v == nil {
		return []byte(def)
	}
	return v
}

// CreateWorkerVersion inserts a new worker version snapshot row within the given tenant
// transaction.
func CreateWorkerVersion(ctx context.Context, tx pgx.Tx, v WorkerVersionRow) (WorkerVersionRow, error) {
	const q = `INSERT INTO worker_versions
		(id, tenant_id, worker_id, version, version_note, status,
		 model_ref, role, skills, behavior, agents_md,
		 context_sources, skill_files, permissions,
		 gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		 recovery_workflow_ref, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
		 $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			model_ref, role, skills, behavior, agents_md,
			context_sources, skill_files, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	// Every jsonb column on worker_versions is NOT NULL with a schema default.
	if len(v.ContextSources) == 0 {
		v.ContextSources = []byte("[]")
	}
	if len(v.SkillFiles) == 0 {
		v.SkillFiles = []byte("[]")
	}
	if len(v.Permissions) == 0 {
		v.Permissions = []byte("{}")
	}
	if len(v.GatedTools) == 0 {
		v.GatedTools = []byte("[]")
	}
	if len(v.BudgetOverrides) == 0 {
		v.BudgetOverrides = []byte("{}")
	}
	if len(v.Labels) == 0 {
		v.Labels = []byte("{}")
	}
	row := v
	// The jsonb columns are NOT NULL with schema defaults ([] / {}), but a caller that leaves a field
	// nil binds SQL NULL and trips the constraint instead of taking the default.
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID, v.WorkerID, v.Version, v.VersionNote, v.Status,
		v.ModelRef, v.Role, v.Skills, v.Behavior, v.AgentsMD,
		jsonbOrDefault(v.ContextSources, "[]"), jsonbOrDefault(v.SkillFiles, "[]"), jsonbOrDefault(v.Permissions, "{}"),
		jsonbOrDefault(v.GatedTools, "[]"), jsonbOrDefault(v.BudgetOverrides, "{}"),
		v.ExecutionPolicyRef, v.ConcurrencyLimit,
		v.RecoveryWorkflowRef, jsonbOrDefault(v.Labels, "{}"),
	).Scan(
		&row.ID, &row.TenantID, &row.WorkerID, &row.Version, &row.VersionNote, &row.Status,
		&row.ModelRef, &row.Role, &row.Skills, &row.Behavior, &row.AgentsMD, &row.ContextSources, &row.SkillFiles, &row.Permissions,
		&row.GatedTools, &row.BudgetOverrides, &row.ExecutionPolicyRef, &row.ConcurrencyLimit,
		&row.RecoveryWorkflowRef, &row.Labels, &row.PublishedAt, &row.CreatedAt,
	)
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: create worker version: %w", err)
	}
	return row, nil
}

// PublishWorkerVersion transitions a draft version to published, setting published_at.
func PublishWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, version int) (WorkerVersionRow, error) {
	const q = `UPDATE worker_versions
		SET status = 'published', published_at = now()
		WHERE tenant_id = $1 AND worker_id = $2 AND version = $3 AND status = 'draft'
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workerID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
		&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
		&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: publish worker version: %w", err)
	}
	return v, nil
}

// GetLatestWorkerVersion returns the latest version (by version number) for a worker.
func GetLatestWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, publishedOnly bool) (WorkerVersionRow, error) {
	q := `SELECT id, tenant_id, worker_id, version, version_note, status,
		model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
		gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		recovery_workflow_ref, labels, published_at, created_at
		FROM worker_versions
		WHERE tenant_id = $1 AND worker_id = $2`
	args := []any{tenantID, workerID}
	if publishedOnly {
		q += ` AND status = 'published'`
	}
	q += ` ORDER BY version DESC LIMIT 1`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
		&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
		&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: get latest worker version: %w", err)
	}
	return v, nil
}

// ListWorkerVersions returns all versions of a worker, newest first.
func ListWorkerVersions(ctx context.Context, tx pgx.Tx, tenantID, workerID string) ([]WorkerVersionRow, error) {
	const q = `SELECT id, tenant_id, worker_id, version, version_note, status,
		model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
		gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		recovery_workflow_ref, labels, published_at, created_at
		FROM worker_versions
		WHERE tenant_id = $1 AND worker_id = $2
		ORDER BY version DESC`
	rows, err := tx.Query(ctx, q, tenantID, workerID)
	if err != nil {
		return nil, fmt.Errorf("db: list worker versions: %w", err)
	}
	defer rows.Close()
	var out []WorkerVersionRow
	for rows.Next() {
		var v WorkerVersionRow
		if err := rows.Scan(
			&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
			&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
			&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
			&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan worker version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeprecateWorkerVersion marks the latest published version of a worker
// as deprecated. Returns ErrNotFound if no published version exists.
func DeprecateWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, version int) (WorkerVersionRow, error) {
	const q = `UPDATE worker_versions
		SET status = 'deprecated'
		WHERE tenant_id = $1 AND worker_id = $2 AND version = $3 AND status = 'published'
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workerID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
		&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
		&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: deprecate worker version: %w", err)
	}
	return v, nil
}

// DeleteWorker hard-deletes a worker and cascades to all owned entities (worker versions, edit
// locks).
func DeleteWorker(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM worker_versions WHERE tenant_id = $1 AND worker_id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete worker versions: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM edit_locks WHERE resource_id = $2 AND resource_type = 'worker' AND tenant_id = $1`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete worker edit locks: %w", err)
	}
	ct, err := tx.Exec(ctx, `DELETE FROM workers WHERE id = $2 AND tenant_id = $1`, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete worker: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWorkerVersion hard-deletes a single worker version (any status).
func DeleteWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID, versionID string) error {
	const q = `DELETE FROM worker_versions WHERE id = $1 AND tenant_id = $2 AND worker_id = $3`
	tag, err := tx.Exec(ctx, q, versionID, tenantID, workerID)
	if err != nil {
		return fmt.Errorf("db: delete worker version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActiveWorkerVersion sets a published version as the worker's current
// version. If versionNum <= 0 the operation is a no-op.
func SetActiveWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, expectedWorkerVersion int, versionNum int) (WorkerRow, error) {
	// First verify the version exists and is published.
	const verifyQ = `SELECT 1 FROM worker_versions WHERE worker_id = $1 AND tenant_id = $2 AND version = $3 AND status = 'published'`
	var ok int
	if err := tx.QueryRow(ctx, verifyQ, workerID, tenantID, versionNum).Scan(&ok); err != nil {
		return WorkerRow{}, ErrNotFound
	}
	// Update the worker's current_version.
	updated, err := UpdateWorkerCurrentVersion(ctx, tx, tenantID, workerID, expectedWorkerVersion, versionNum)
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: set active worker version: %w", err)
	}
	return updated, nil
}

// RevertWorkerVersionToDraft sets a published version back to draft status.
func RevertWorkerVersionToDraft(ctx context.Context, tx pgx.Tx, tenantID, versionID string) error {
	const q = `UPDATE worker_versions SET status = 'draft' WHERE id = $1 AND tenant_id = $2 AND status = 'published'`
	tag, err := tx.Exec(ctx, q, versionID, tenantID)
	if err != nil {
		return fmt.Errorf("db: revert worker version to draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetWorkerIDForVersion resolves the owning worker_id for a worker version within the tenant scope
// (used to target audit rows for version-only requests like RevertWorkerVersionToDraft).
func GetWorkerIDForVersion(ctx context.Context, tx pgx.Tx, tenantID, versionID string) (string, error) {
	var workerID string
	err := tx.QueryRow(ctx,
		`SELECT worker_id FROM worker_versions WHERE id = $1 AND tenant_id = $2`,
		versionID, tenantID).Scan(&workerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: get worker id for version: %w", err)
	}
	return workerID, nil
}

// GetWorkerVersionByID fetches a single worker version by its ID within
// the given tenant scope.
func GetWorkerVersionByID(ctx context.Context, tx pgx.Tx, tenantID, workerID, versionID string) (WorkerVersionRow, error) {
	const q = `SELECT id, tenant_id, worker_id, version, version_note, status,
		model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
		gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		recovery_workflow_ref, labels, published_at, created_at
		FROM worker_versions
		WHERE id = $1 AND worker_id = $2 AND tenant_id = $3`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, versionID, workerID, tenantID).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
		&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
		&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: get worker version by id: %w", err)
	}
	return v, nil
}

// GetWorkerVersionByNumber resolves a worker version by its VERSION NUMBER —
// the ordinal in the worker's version trail (1, 2, 3 …) — not by its row id.
//
// It exists because three dispatch paths carried a pinned version NUMBER and
// asked for it BY ID:
//
//	db.GetWorkerVersionByID(ctx, tx, tenantID, workerID, fmt.Sprintf("v%d", n))
//
// worker_versions.id is a ULID (NewID) and worker_versions_pkey is on id, so
// "v3" matches no row — ever. Each call site reads the miss as "this step is
// not pinned" and falls back to GetLatestWorkerVersion(…, publishedOnly=true),
// so a step that pinned v3 silently ran whatever was latest published instead.
// Measured on the live plane before this function existed: 360 workflow_step_runs
// carried a non-zero _worker_version and 218 of them had a matching published
// row at that number — those 218 were resolving to the wrong version.
//
// worker_versions_worker_version_idx is UNIQUE (worker_id, version), so the
// (tenant, worker, number) triple addresses at most one row.
//
// DISPATCHABLE-ONLY (status <> 'draft'), deliberately — the status vocabulary
// is the schema's own documented dispatchability contract:
//
//   - a DRAFT is not dispatchable: publishing is what "mak[es] it
//     dispatchable" (proto/orchicon/api/v1/worker_service.proto:20-22). A
//     stray draft — an abandoned edit, which is exactly the state the TUI must
//     never leave behind — must not become the version a run executes;
//   - DEPRECATED stays IN: a deprecated version "is still dispatchable for
//     in-flight Workflows; no new Workflows may bind"
//     (worker_service.proto:25-27), so a run pinned to a since-deprecated
//     version still resolves to it.
//
// Returns ErrNotFound for an unknown number, a version that exists only as a
// draft, or another tenant's/worker's row — which is the callers' documented
// signal to fall back to the latest published version.
func GetWorkerVersionByNumber(ctx context.Context, tx pgx.Tx, tenantID, workerID string, version int) (WorkerVersionRow, error) {
	const q = `SELECT id, tenant_id, worker_id, version, version_note, status,
		model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
		gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		recovery_workflow_ref, labels, published_at, created_at
		FROM worker_versions
		WHERE tenant_id = $1 AND worker_id = $2 AND version = $3 AND status <> 'draft'`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workerID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.SkillFiles, &v.Permissions,
		&v.GatedTools, &v.BudgetOverrides, &v.ExecutionPolicyRef, &v.ConcurrencyLimit,
		&v.RecoveryWorkflowRef, &v.Labels, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: get worker version by number: %w", err)
	}
	return v, nil
}

// UpdateDraftVersion overwrites all mutable fields of a draft WorkerVersion row.
func UpdateDraftVersion(ctx context.Context, tx pgx.Tx, v WorkerVersionRow) (WorkerVersionRow, error) {
	const q = `UPDATE worker_versions
		SET model_ref = $3,
		    role = $4,
		    skills = $5,
		    behavior = $6,
		    agents_md = $7,
		    context_sources = $8,
		    skill_files = $9,
		    permissions = $10,
		    gated_tools = $11,
		    budget_overrides = $12,
		    execution_policy_ref = $13,
		    concurrency_limit = $14,
		    recovery_workflow_ref = $15,
		    labels = $16,
		    version_note = $17
		WHERE id = $1 AND tenant_id = $2 AND status = 'draft'
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			model_ref, role, skills, behavior, agents_md, context_sources, skill_files, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var row WorkerVersionRow
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID,
		v.ModelRef, v.Role, v.Skills, v.Behavior, v.AgentsMD, v.ContextSources, v.SkillFiles, v.Permissions,
		v.GatedTools, v.BudgetOverrides, v.ExecutionPolicyRef, v.ConcurrencyLimit,
		v.RecoveryWorkflowRef, v.Labels, v.VersionNote,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkerID, &row.Version, &row.VersionNote, &row.Status,
		&row.ModelRef, &row.Role, &row.Skills, &row.Behavior, &row.AgentsMD, &row.ContextSources, &row.SkillFiles, &row.Permissions,
		&row.GatedTools, &row.BudgetOverrides, &row.ExecutionPolicyRef, &row.ConcurrencyLimit,
		&row.RecoveryWorkflowRef, &row.Labels, &row.PublishedAt, &row.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: update draft version: %w", err)
	}
	return row, nil
}

// NextWorkerVersionNumber returns the next version number for a worker
// (max existing version + 1, or 1 if no versions exist).
func NextWorkerVersionNumber(ctx context.Context, tx pgx.Tx, tenantID, workerID string) (int, error) {
	var maxVersion int
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM worker_versions WHERE tenant_id = $1 AND worker_id = $2`,
		tenantID, workerID,
	).Scan(&maxVersion)
	if err != nil {
		return 0, fmt.Errorf("db: next worker version number: %w", err)
	}
	return maxVersion + 1, nil
}

// ListPublishedWorkerModelRefs returns each dispatchable worker's latest PUBLISHED version
// model_ref for a tenant — one entry per worker, in worker-id order (stable).
func ListPublishedWorkerModelRefs(ctx context.Context, tx pgx.Tx, tenantID string) ([]string, error) {
	const q = `SELECT DISTINCT ON (w.id) v.model_ref
		FROM workers w
		JOIN worker_versions v ON v.worker_id = w.id AND v.tenant_id = w.tenant_id
		WHERE w.tenant_id = $1 AND w.status IN ('published', 'deprecated')
			AND v.status = 'published'
		ORDER BY w.id, v.version DESC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list published worker model refs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("db: scan published worker model ref: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list published worker model refs: %w", err)
	}
	return out, nil
}
