package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkerRow is the data-access shape of a workers table row — the
// immutable header (docs/05 §3.1, docs/09 §3.3). The mutable snapshot
// lives in WorkerVersionRow. tenant_id is the primary isolation layer;
// RLS is the backstop (docs/09 §8.5).
type WorkerRow struct {
	ID             string
	TenantID       string
	Name           string
	Slug           string
	Description    string
	Purpose        string
	Status         string
	CurrentVersion int
	CreatedBy      string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WorkerVersionRow is the data-access shape of a worker_versions table
// row — the mutable snapshot of a Worker's fields at a specific version
// (docs/05 §5, docs/09 §3.3). Once published, a version is immutable;
// changes create a new version. JSON-typed columns (permissions,
// budget_overrides, etc.) are stored as raw []byte and validated at the
// API boundary (AGENTS.md security standards).
type WorkerVersionRow struct {
	ID                 string
	TenantID           string
	WorkerID           string
	Version            int
	VersionNote        string
	Status             string
	RuntimeRef         string
	ModelRef           string
	SystemPrompt       string
	Role               string
	Skills             string
	Behavior           string
	AgentsMD           string
	ContextSources     []byte // jsonb
	Permissions        []byte // jsonb
	GatedTools         []byte // jsonb
	BudgetOverrides    []byte // jsonb
	ExecutionPolicyRef string
	ConcurrencyLimit   int
	RecoveryWorkflowRef string
	Labels             []byte // jsonb
	PublishedAt        *time.Time
	CreatedAt          time.Time
}

// CreateWorker inserts a new worker header row within the given tenant
// transaction. The caller controls the transaction so the outbox row can
// be enqueued in the same atomic unit (docs/09 §6). Version starts at 1;
// current_version starts at 0 (no published versions yet).
func CreateWorker(ctx context.Context, tx pgx.Tx, w WorkerRow) (WorkerRow, error) {
	const q = `INSERT INTO workers
		(id, tenant_id, name, slug, description, purpose, status, current_version, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, tenant_id, name, slug, description, purpose, status,
			current_version, created_by, version, created_at, updated_at`
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.Name, w.Slug, w.Description, w.Purpose,
		w.Status, w.CurrentVersion, w.CreatedBy,
	).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.Slug, &row.Description,
		&row.Purpose, &row.Status, &row.CurrentVersion, &row.CreatedBy,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: create worker: %w", err)
	}
	return row, nil
}

// GetWorker fetches a single worker by id within the tenant scope.
func GetWorker(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkerRow, error) {
	const q = `SELECT id, tenant_id, name, slug, description, purpose, status,
		current_version, created_by, version, created_at, updated_at
		FROM workers WHERE id = $1 AND tenant_id = $2`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt,
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
}

// ListWorkers returns a page of workers for the tenant with cursor-based
// pagination, optional search/filter, and configurable sort.
func ListWorkers(ctx context.Context, tx pgx.Tx, f ListWorkersFilter) ([]WorkerRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	args := []any{f.TenantID}
	where := `tenant_id = $1`
	idx := 2
	if f.AfterID != "" {
		where += fmt.Sprintf(` AND id > $%d`, idx)
		args = append(args, f.AfterID)
		idx++
	}
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
	q := fmt.Sprintf(`SELECT id, tenant_id, name, slug, description, purpose, status,
		current_version, created_by, version, created_at, updated_at
		FROM workers
		WHERE %s
		ORDER BY %s %s LIMIT $%d`, where, sortBy, sortOrder, idx)
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
			&w.Purpose, &w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan worker: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkerStatus transitions a worker's status with optimistic
// concurrency. The tenant_id is injected into the WHERE clause. Returns
// ErrNotFound if no row matches the id+tenant+version.
func UpdateWorkerStatus(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, status string) (WorkerRow, error) {
	const q = `UPDATE workers
		SET status = $4, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, description, purpose, status,
			current_version, created_by, version, created_at, updated_at`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: update worker status: %w", err)
	}
	return w, nil
}

// UpdateWorkerCurrentVersion bumps the current_version pointer to the
// newly published version. Uses optimistic concurrency on the header row.
func UpdateWorkerCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion, newVersion int) (WorkerRow, error) {
	const q = `UPDATE workers
		SET current_version = $4, status = 'published', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, description, purpose, status,
			current_version, created_by, version, created_at, updated_at`
	var w WorkerRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, newVersion).Scan(
		&w.ID, &w.TenantID, &w.Name, &w.Slug, &w.Description, &w.Purpose,
		&w.Status, &w.CurrentVersion, &w.CreatedBy, &w.Version,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("db: update worker current_version: %w", err)
	}
	return w, nil
}

// CreateWorkerVersion inserts a new worker version snapshot row within
// the given tenant transaction. The version number is computed by the
// caller (max+1). Status starts as "draft".
func CreateWorkerVersion(ctx context.Context, tx pgx.Tx, v WorkerVersionRow) (WorkerVersionRow, error) {
	const q = `INSERT INTO worker_versions
		(id, tenant_id, worker_id, version, version_note, status,
		 runtime_ref, model_ref, role, skills, behavior, agents_md,
		 context_sources, permissions,
		 gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		 recovery_workflow_ref, labels)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
		 $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			runtime_ref, model_ref, role, skills, behavior, agents_md,
			context_sources, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	row := v
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID, v.WorkerID, v.Version, v.VersionNote, v.Status,
		v.RuntimeRef, v.ModelRef, v.Role, v.Skills, v.Behavior, v.AgentsMD, v.ContextSources, v.Permissions,
		v.GatedTools, v.BudgetOverrides, v.ExecutionPolicyRef, v.ConcurrencyLimit,
		v.RecoveryWorkflowRef, v.Labels,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkerID, &row.Version, &row.VersionNote, &row.Status,
		&row.RuntimeRef, &row.ModelRef, &row.Role, &row.Skills, &row.Behavior, &row.AgentsMD, &row.ContextSources, &row.Permissions,
		&row.GatedTools, &row.BudgetOverrides, &row.ExecutionPolicyRef, &row.ConcurrencyLimit,
		&row.RecoveryWorkflowRef, &row.Labels, &row.PublishedAt, &row.CreatedAt,
	)
	if err != nil {
		return WorkerVersionRow{}, fmt.Errorf("db: create worker version: %w", err)
	}
	return row, nil
}

// PublishWorkerVersion transitions a draft version to published,
// setting published_at. Uses status CAS (draft → published). Returns
// ErrNotFound if the version is not in draft state.
func PublishWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, version int) (WorkerVersionRow, error) {
	const q = `UPDATE worker_versions
		SET status = 'published', published_at = now()
		WHERE tenant_id = $1 AND worker_id = $2 AND version = $3 AND status = 'draft'
		RETURNING id, tenant_id, worker_id, version, version_note, status,
			runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workerID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.RuntimeRef, &v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.Permissions,
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

// GetLatestWorkerVersion returns the latest version (by version number)
// for a worker. If includePublishedOnly is true, returns the latest
// published version; otherwise returns the newest version regardless of
// status.
func GetLatestWorkerVersion(ctx context.Context, tx pgx.Tx, tenantID, workerID string, publishedOnly bool) (WorkerVersionRow, error) {
	q := `SELECT id, tenant_id, worker_id, version, version_note, status,
		runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
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
		&v.RuntimeRef, &v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.Permissions,
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
		runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
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
			&v.RuntimeRef, &v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.Permissions,
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
			runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workerID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.RuntimeRef, &v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.Permissions,
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

// DeleteWorker hard-deletes a worker and cascades to all owned entities
// (worker versions, edit locks). The tenant_id is injected into the
// WHERE clause for isolation. Returns ErrNotFound if no row matches.
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

// GetWorkerVersionByID fetches a single worker version by its ID within
// the given tenant scope.
func GetWorkerVersionByID(ctx context.Context, tx pgx.Tx, tenantID, workerID, versionID string) (WorkerVersionRow, error) {
	const q = `SELECT id, tenant_id, worker_id, version, version_note, status,
		runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
		gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
		recovery_workflow_ref, labels, published_at, created_at
		FROM worker_versions
		WHERE id = $1 AND worker_id = $2 AND tenant_id = $3`
	var v WorkerVersionRow
	err := tx.QueryRow(ctx, q, versionID, workerID, tenantID).Scan(
		&v.ID, &v.TenantID, &v.WorkerID, &v.Version, &v.VersionNote, &v.Status,
		&v.RuntimeRef, &v.ModelRef, &v.Role, &v.Skills, &v.Behavior, &v.AgentsMD, &v.ContextSources, &v.Permissions,
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

// UpdateDraftVersion overwrites all mutable fields of a draft
// WorkerVersion row. Only versions with status='draft' may be updated.
// The caller is responsible for merging request fields into a full
// WorkerVersionRow before calling (the service layer reads the current,
// applies overrides, and passes the merged row here).
func UpdateDraftVersion(ctx context.Context, tx pgx.Tx, v WorkerVersionRow) (WorkerVersionRow, error) {
	const q = `UPDATE worker_versions
		SET runtime_ref = $3,
		    model_ref = $4,
		    role = $5,
		    skills = $6,
		    behavior = $7,
		    agents_md = $8,
		    context_sources = $9,
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
			runtime_ref, model_ref, role, skills, behavior, agents_md, context_sources, permissions,
			gated_tools, budget_overrides, execution_policy_ref, concurrency_limit,
			recovery_workflow_ref, labels, published_at, created_at`
	var row WorkerVersionRow
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID,
		v.RuntimeRef, v.ModelRef, v.Role, v.Skills, v.Behavior, v.AgentsMD, v.ContextSources, v.Permissions,
		v.GatedTools, v.BudgetOverrides, v.ExecutionPolicyRef, v.ConcurrencyLimit,
		v.RecoveryWorkflowRef, v.Labels, v.VersionNote,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkerID, &row.Version, &row.VersionNote, &row.Status,
		&row.RuntimeRef, &row.ModelRef, &row.Role, &row.Skills, &row.Behavior, &row.AgentsMD, &row.ContextSources, &row.Permissions,
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
