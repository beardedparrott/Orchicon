package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkflowRow is the data-access shape of a workflows table row — the
// immutable header (docs/02 §2.4, docs/09 §3.4). The mutable snapshot
// (steps, inputs, outputs, recovery_policy_ref) lives in
// WorkflowVersionRow. project_id is empty for tenant-level templates.
// tenant_id is the primary isolation layer; RLS is the backstop
// (docs/09 §8.5).
type WorkflowRow struct {
	ID             string
	TenantID       string
	ProjectID      string // empty for tenant-level templates
	Name           string
	CurrentVersion int
	Status         string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WorkflowVersionRow is the data-access shape of a workflow_versions
// table row — the snapshot of a Workflow's steps at a specific version
// (docs/02 §2.4, docs/09 §3.4). Once published, a version is immutable;
// changes create a new version. The steps field is a JSON array of Step
// messages (validated at the API boundary).
type WorkflowVersionRow struct {
	ID                 string
	TenantID           string
	WorkflowID         string
	Version            int
	VersionNote        string
	Status             string
	Steps              []byte // jsonb: array of Step messages
	Inputs             []byte // jsonb
	Outputs            []byte // jsonb
	RecoveryPolicyRef  string
	PublishedAt        *time.Time
	CreatedAt          time.Time
}

// WorkflowRunRow is the data-access shape of a workflow_runs table row
// (docs/02 §2.4, docs/09 §3.4). A single execution of a published
// Workflow version, progressed by the WorkflowReconciler.
// work_item_id and bound_worker_ref are populated for template-bound
// runs (docs/11 §5.1).
type WorkflowRunRow struct {
	ID              string
	TenantID        string
	WorkflowID      string
	WorkflowVersion int
	ProjectID       string
	Status          string
	CurrentStep     string
	RunContext      []byte // jsonb
	WorkItemID      string // bound work item id; empty for one-shot runs
	BoundWorkerRef  []byte // jsonb; reserved for future use
	Version         int
	StartedAt       *time.Time
	EndedAt         *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// WorkflowStepRunRow is the data-access shape of a workflow_step_runs
// table row (docs/09 §3.4). The runtime state of a single step within
// a WorkflowRun. iteration and superseded_by track loop decision
// re-entry (docs/11 §3.4).
type WorkflowStepRunRow struct {
	ID                 string
	TenantID           string
	WorkflowRunID      string
	StepID             string
	StepName           string
	StepKind           string
	Status             string
	Attempt            int
	Result             []byte // jsonb
	WorkerExecutionID  string
	Iteration          int    // re-entry count (0 for first dispatch)
	SupersededBy       string // step run id that superseded this one
	StartedAt          *time.Time
	EndedAt            *time.Time
	Version            int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateWorkflow inserts a new workflow header row within the given
// tenant transaction. The caller controls the transaction so the outbox
// row can be enqueued in the same atomic unit (docs/09 §6). Version
// starts at 1; current_version starts at 0 (no published versions yet).
func CreateWorkflow(ctx context.Context, tx pgx.Tx, w WorkflowRow) (WorkflowRow, error) {
	const q = `INSERT INTO workflows
		(id, tenant_id, project_id, name, current_version, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, project_id, name, current_version, status,
			version, created_at, updated_at`
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.ProjectID, w.Name, w.CurrentVersion, w.Status,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.Name, &row.CurrentVersion,
		&row.Status, &row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("db: create workflow: %w", err)
	}
	return row, nil
}

// GetWorkflow fetches a single workflow by id within the tenant scope.
func GetWorkflow(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkflowRow, error) {
	const q = `SELECT id, tenant_id, project_id, name, current_version, status,
		version, created_at, updated_at
		FROM workflows WHERE id = $1 AND tenant_id = $2`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Version, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("db: get workflow: %w", err)
	}
	return w, nil
}

// ListWorkflowsFilter scopes a list query to a tenant, optionally
// filtered by project, status, search, and sort.
type ListWorkflowsFilter struct {
	TenantID  string
	ProjectID string // empty = all (including templates)
	Status    string // empty = all statuses
	PageSize  int
	AfterID   string
	Search    string
	SortBy    string // "name", "status", "created_at" (default "id")
	SortOrder string // "asc" or "desc" (default "asc")
}

// ListWorkflows returns a page of workflows for the tenant with
// cursor-based pagination, optional search/filter, and configurable sort
// (docs/07 §5.2).
func ListWorkflows(ctx context.Context, tx pgx.Tx, f ListWorkflowsFilter) ([]WorkflowRow, error) {
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
	if f.ProjectID != "" {
		where += fmt.Sprintf(` AND project_id = $%d`, idx)
		args = append(args, f.ProjectID)
		idx++
	}
	if f.Status != "" {
		where += fmt.Sprintf(` AND status = $%d`, idx)
		args = append(args, f.Status)
		idx++
	}
	if f.Search != "" {
		where += fmt.Sprintf(` AND name ILIKE $%d`, idx)
		args = append(args, "%"+f.Search+"%")
		idx++
	}
	sortBy := "id"
	if f.SortBy == "name" || f.SortBy == "status" || f.SortBy == "created_at" {
		sortBy = f.SortBy
	}
	sortOrder := "ASC"
	if f.SortOrder == "desc" {
		sortOrder = "DESC"
	}
	q := fmt.Sprintf(`SELECT id, tenant_id, project_id, name, current_version, status,
		version, created_at, updated_at
		FROM workflows
		WHERE %s
		ORDER BY %s %s LIMIT $%d`, where, sortBy, sortOrder, idx)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list workflows: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRow
	for rows.Next() {
		var w WorkflowRow
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
			&w.Status, &w.Version, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWorkflow hard-deletes a workflow and all its child rows (runs,
// step runs, versions, edit locks) within the tenant scope. This is an
// irreversible operation (docs/02 §2.4 — use Deprecate for soft hide).
func DeleteWorkflow(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM workflow_step_runs WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE tenant_id = $1 AND workflow_id = $2)`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete workflow step runs: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workflow_runs WHERE tenant_id = $1 AND workflow_id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete workflow runs: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete workflow versions: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM edit_locks WHERE resource_id = $2 AND resource_type = 'workflow' AND tenant_id = $1`, tenantID, id); err != nil {
		return fmt.Errorf("db: delete workflow edit locks: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM workflows WHERE id = $2 AND tenant_id = $1`, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete workflow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateWorkflowStatus transitions a workflow's status with optimistic
// concurrency (docs/09 §5). tenant_id injected into WHERE.
func UpdateWorkflowStatus(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, status string) (WorkflowRow, error) {
	const q = `UPDATE workflows
		SET status = $4, updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, project_id, name, current_version, status,
			version, created_at, updated_at`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Version, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("db: update workflow status: %w", err)
	}
	return w, nil
}

// UpdateWorkflowCurrentVersion bumps the current_version pointer to the
// newly published version. Uses optimistic concurrency on the header.
func UpdateWorkflowCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion, newVersion int) (WorkflowRow, error) {
	const q = `UPDATE workflows
		SET current_version = $4, status = 'published', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, project_id, name, current_version, status,
			version, created_at, updated_at`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, newVersion).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Version, &w.CreatedAt, &w.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("db: update workflow current_version: %w", err)
	}
	return w, nil
}

// CreateWorkflowVersion inserts a new workflow version snapshot row
// within the given tenant transaction. The version number is computed by
// the caller (max+1). Status starts as "draft".
func CreateWorkflowVersion(ctx context.Context, tx pgx.Tx, v WorkflowVersionRow) (WorkflowVersionRow, error) {
	const q = `INSERT INTO workflow_versions
		(id, tenant_id, workflow_id, version, version_note, status,
		 steps, inputs, outputs, recovery_policy_ref)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, tenant_id, workflow_id, version, version_note, status,
			steps, inputs, outputs, recovery_policy_ref, published_at, created_at`
	row := v
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID, v.WorkflowID, v.Version, v.VersionNote, v.Status,
		v.Steps, v.Inputs, v.Outputs, v.RecoveryPolicyRef,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowID, &row.Version, &row.VersionNote,
		&row.Status, &row.Steps, &row.Inputs, &row.Outputs,
		&row.RecoveryPolicyRef, &row.PublishedAt, &row.CreatedAt,
	)
	if err != nil {
		return WorkflowVersionRow{}, fmt.Errorf("db: create workflow version: %w", err)
	}
	return row, nil
}

// PublishWorkflowVersion transitions a draft version to published,
// setting published_at. Uses status CAS (draft → published). Returns
// ErrNotFound if the version is not in draft state.
func PublishWorkflowVersion(ctx context.Context, tx pgx.Tx, tenantID, workflowID string, version int) (WorkflowVersionRow, error) {
	const q = `UPDATE workflow_versions
		SET status = 'published', published_at = now()
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3 AND status = 'draft'
		RETURNING id, tenant_id, workflow_id, version, version_note, status,
			steps, inputs, outputs, recovery_policy_ref, published_at, created_at`
	var v WorkflowVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workflowID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.RecoveryPolicyRef, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowVersionRow{}, fmt.Errorf("db: publish workflow version: %w", err)
	}
	return v, nil
}

// GetLatestWorkflowVersion returns the latest version (by version
// number) for a workflow. If publishedOnly is true, returns the latest
// published version; otherwise returns the newest version regardless of
// status.
func GetLatestWorkflowVersion(ctx context.Context, tx pgx.Tx, tenantID, workflowID string, publishedOnly bool) (WorkflowVersionRow, error) {
	q := `SELECT id, tenant_id, workflow_id, version, version_note, status,
		steps, inputs, outputs, recovery_policy_ref, published_at, created_at
		FROM workflow_versions
		WHERE tenant_id = $1 AND workflow_id = $2`
	args := []any{tenantID, workflowID}
	if publishedOnly {
		q += ` AND status = 'published'`
	}
	q += ` ORDER BY version DESC LIMIT 1`
	var v WorkflowVersionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.RecoveryPolicyRef, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowVersionRow{}, fmt.Errorf("db: get latest workflow version: %w", err)
	}
	return v, nil
}

// GetWorkflowVersion returns a specific workflow version by id within
// the tenant scope.
func GetWorkflowVersion(ctx context.Context, tx pgx.Tx, tenantID, workflowID string, version int) (WorkflowVersionRow, error) {
	const q = `SELECT id, tenant_id, workflow_id, version, version_note, status,
		steps, inputs, outputs, recovery_policy_ref, published_at, created_at
		FROM workflow_versions
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3`
	var v WorkflowVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workflowID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.RecoveryPolicyRef, &v.PublishedAt, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowVersionRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowVersionRow{}, fmt.Errorf("db: get workflow version: %w", err)
	}
	return v, nil
}

// ListWorkflowVersions returns all versions of a workflow, newest first.
func ListWorkflowVersions(ctx context.Context, tx pgx.Tx, tenantID, workflowID string) ([]WorkflowVersionRow, error) {
	const q = `SELECT id, tenant_id, workflow_id, version, version_note, status,
		steps, inputs, outputs, recovery_policy_ref, published_at, created_at
		FROM workflow_versions
		WHERE tenant_id = $1 AND workflow_id = $2
		ORDER BY version DESC`
	rows, err := tx.Query(ctx, q, tenantID, workflowID)
	if err != nil {
		return nil, fmt.Errorf("db: list workflow versions: %w", err)
	}
	defer rows.Close()
	var out []WorkflowVersionRow
	for rows.Next() {
		var v WorkflowVersionRow
		if err := rows.Scan(
			&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
			&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
			&v.RecoveryPolicyRef, &v.PublishedAt, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteWorkflowVersion hard-deletes a single workflow version. Only
// draft versions may be deleted. At least one version must remain after
// deletion, and the version is identified by its ULID id (not version
// number).
func DeleteWorkflowVersion(ctx context.Context, tx pgx.Tx, tenantID, workflowID, versionID string) error {
	// Verify the version exists, is a draft, and that at least one other
	// version would remain.
	var status string
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT wv.status, (SELECT count(*) FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2) AS cnt
		 FROM workflow_versions wv WHERE tenant_id = $1 AND id = $3 AND workflow_id = $2`,
		tenantID, workflowID, versionID).Scan(&status, &count); err != nil {
		return fmt.Errorf("db: get workflow version: %w", err)
	}
	if status != "draft" {
		return fmt.Errorf("db: cannot delete %s version", status)
	}
	if count < 2 {
		return fmt.Errorf("db: cannot delete the last version")
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM workflow_versions WHERE tenant_id = $1 AND id = $3 AND workflow_id = $2`,
		tenantID, workflowID, versionID)
	if err != nil {
		return fmt.Errorf("db: delete workflow version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NextWorkflowVersionNumber returns the next version number for a
// workflow (max existing version + 1, or 1 if no versions exist).
func NextWorkflowVersionNumber(ctx context.Context, tx pgx.Tx, tenantID, workflowID string) (int, error) {
	var maxVersion int
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2`,
		tenantID, workflowID,
	).Scan(&maxVersion)
	if err != nil {
		return 0, fmt.Errorf("db: next workflow version number: %w", err)
	}
	return maxVersion + 1, nil
}

// --- WorkflowRun -----------------------------------------------------------

// CreateWorkflowRun inserts a new workflow run row within the given
// tenant transaction (docs/03 §2: StartWorkflow).
// workItemVal returns nil for empty string (SQL NULL) so FK constraints
// are not violated by the zero value. Used by CreateWorkflowRun and
// UpdateWorkflowRun for work_item_id.
func workItemVal(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// boundWorkerRefVal returns nil for empty slice (SQL NULL).
func boundWorkerRefVal(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func CreateWorkflowRun(ctx context.Context, tx pgx.Tx, r WorkflowRunRow) (WorkflowRunRow, error) {
	const q = `INSERT INTO workflow_runs
		(id, tenant_id, workflow_id, workflow_version, project_id, status,
		 current_step, run_context, work_item_id, bound_worker_ref, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, workflow_id, workflow_version, project_id, status,
			current_step, run_context, work_item_id, bound_worker_ref,
			version, started_at, ended_at, created_at, updated_at`
	row := r
	var wiID *string
	err := tx.QueryRow(ctx, q,
		r.ID, r.TenantID, r.WorkflowID, r.WorkflowVersion, r.ProjectID,
		r.Status, r.CurrentStep, r.RunContext,
		workItemVal(r.WorkItemID), boundWorkerRefVal(r.BoundWorkerRef),
		r.StartedAt,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowID, &row.WorkflowVersion,
		&row.ProjectID, &row.Status, &row.CurrentStep, &row.RunContext,
		&wiID, &row.BoundWorkerRef,
		&row.Version, &row.StartedAt, &row.EndedAt,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkflowRunRow{}, fmt.Errorf("db: create workflow run: %w", err)
	}
	if wiID != nil {
		row.WorkItemID = *wiID
	}
	return row, nil
}

// GetWorkflowRun fetches a single workflow run by id within the tenant.
func GetWorkflowRun(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkflowRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref,
		version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs WHERE id = $1 AND tenant_id = $2`
	var r WorkflowRunRow
	var wiID *string
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
		&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
		&wiID, &r.BoundWorkerRef,
		&r.Version, &r.StartedAt, &r.EndedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRow{}, fmt.Errorf("db: get workflow run: %w", err)
	}
	if wiID != nil {
		r.WorkItemID = *wiID
	}
	return r, nil
}

// ListWorkflowRunsFilter scopes a list query to a workflow, optionally
// filtered by status.
type ListWorkflowRunsFilter struct {
	TenantID   string
	WorkflowID string
	Status     string
	PageSize   int
	AfterID    string
}

// ListWorkflowRuns returns a page of workflow runs for a workflow.
func ListWorkflowRuns(ctx context.Context, tx pgx.Tx, f ListWorkflowRunsFilter) ([]WorkflowRunRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref,
		version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)`
	args := []any{f.TenantID, f.AfterID}
	if f.WorkflowID != "" {
		q += fmt.Sprintf(` AND workflow_id = $%d`, len(args)+1)
		args = append(args, f.WorkflowID)
	}
	if f.Status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	q += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list workflow runs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		var wiID *string
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
			&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
			&wiID, &r.BoundWorkerRef,
			&r.Version, &r.StartedAt, &r.EndedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow run: %w", err)
		}
		if wiID != nil {
			r.WorkItemID = *wiID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateWorkflowRunFields is a partial update applied with optimistic
// concurrency (docs/09 §5).
type UpdateWorkflowRunFields struct {
	Status      *string
	CurrentStep *string
	RunContext  *[]byte
	StartedAt   *time.Time
	EndedAt     *time.Time
	// ProjectID lets a workflow step bind the run to a project on the
	// first dispatch (PROJECT kind steps write this; idempotent).
	ProjectID *string
	// WorkItemID links a bound run to its work item (docs/11 §2.1).
	WorkItemID     *string
	BoundWorkerRef *[]byte
}

// UpdateWorkflowRun applies a partial update with optimistic concurrency.
func UpdateWorkflowRun(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateWorkflowRunFields) (WorkflowRunRow, error) {
	q := `UPDATE workflow_runs SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.CurrentStep != nil {
		q += fmt.Sprintf(`, current_step = $%d`, setIdx)
		args = append(args, *f.CurrentStep)
		setIdx++
	}
	if f.RunContext != nil {
		q += fmt.Sprintf(`, run_context = $%d`, setIdx)
		args = append(args, *f.RunContext)
		setIdx++
	}
	if f.StartedAt != nil {
		q += fmt.Sprintf(`, started_at = $%d`, setIdx)
		args = append(args, *f.StartedAt)
		setIdx++
	}
	if f.EndedAt != nil {
		q += fmt.Sprintf(`, ended_at = $%d`, setIdx)
		args = append(args, *f.EndedAt)
		setIdx++
	}
	if f.ProjectID != nil {
		q += fmt.Sprintf(`, project_id = $%d`, setIdx)
		args = append(args, *f.ProjectID)
		setIdx++
	}
	if f.WorkItemID != nil {
		q += fmt.Sprintf(`, work_item_id = $%d`, setIdx)
		args = append(args, workItemVal(*f.WorkItemID))
		setIdx++
	}
	if f.BoundWorkerRef != nil {
		q += fmt.Sprintf(`, bound_worker_ref = $%d`, setIdx)
		args = append(args, boundWorkerRefVal(*f.BoundWorkerRef))
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref,
		version, started_at, ended_at, created_at, updated_at`
	var r WorkflowRunRow
	var wiID *string
	err := tx.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
		&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
		&wiID, &r.BoundWorkerRef,
		&r.Version, &r.StartedAt, &r.EndedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowRunRow{}, fmt.Errorf("db: update workflow run: %w", err)
	}
	if wiID != nil {
		r.WorkItemID = *wiID
	}
	return r, nil
}

// --- WorkflowStepRun -------------------------------------------------------

// CreateWorkflowStepRun inserts a new step run row within the given
// tenant transaction.
func CreateWorkflowStepRun(ctx context.Context, tx pgx.Tx, s WorkflowStepRunRow) (WorkflowStepRunRow, error) {
	if s.Result == nil {
		s.Result = []byte("{}")
	}
	const q = `INSERT INTO workflow_step_runs
		(id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		 status, attempt, result, worker_execution_id, iteration, superseded_by, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
			status, attempt, result, worker_execution_id, iteration, superseded_by,
			started_at, ended_at, version, created_at, updated_at`
	row := s
	err := tx.QueryRow(ctx, q,
		s.ID, s.TenantID, s.WorkflowRunID, s.StepID, s.StepName, s.StepKind,
		s.Status, s.Attempt, s.Result, s.WorkerExecutionID,
		s.Iteration, iterSupersededBy(s.SupersededBy),
		s.StartedAt,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowRunID, &row.StepID, &row.StepName,
		&row.StepKind, &row.Status, &row.Attempt, &row.Result,
		&row.WorkerExecutionID, &row.Iteration, &row.SupersededBy,
		&row.StartedAt, &row.EndedAt, &row.Version,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: create workflow step run: %w", err)
	}
	return row, nil
}

// iterSupersededBy returns nil for empty string (SQL NULL).
func iterSupersededBy(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GetWorkflowStepRun fetches a single step run by id within the tenant.
func GetWorkflowStepRun(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs WHERE id = $1 AND tenant_id = $2`
	var s WorkflowStepRunRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &s.SupersededBy, &s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: get workflow step run: %w", err)
	}
	return s, nil
}

// GetWorkflowStepRunByStep returns the step run for a given
// (workflow_run_id, step_id) pair. Used by the reconciler to look up
// the runtime state of a step within a run.
func GetWorkflowStepRunByStep(ctx context.Context, tx pgx.Tx, tenantID, runID, stepID string) (WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs
		WHERE tenant_id = $1 AND workflow_run_id = $2 AND step_id = $3
		ORDER BY created_at DESC LIMIT 1`
	var s WorkflowStepRunRow
	err := tx.QueryRow(ctx, q, tenantID, runID, stepID).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &s.SupersededBy, &s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: get workflow step run by step: %w", err)
	}
	return s, nil
}

// ListWorkflowStepRuns returns all step runs for a workflow run.
func ListWorkflowStepRuns(ctx context.Context, tx pgx.Tx, tenantID, runID string) ([]WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs
		WHERE tenant_id = $1 AND workflow_run_id = $2
		ORDER BY created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("db: list workflow step runs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowStepRunRow
	for rows.Next() {
		var s WorkflowStepRunRow
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
			&s.StepKind, &s.Status, &s.Attempt, &s.Result,
			&s.WorkerExecutionID,
			&s.Iteration, &s.SupersededBy, &s.StartedAt, &s.EndedAt, &s.Version,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow step run: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateWorkflowStepRunFields is a partial update applied with
// optimistic concurrency (docs/09 §5).
type UpdateWorkflowStepRunFields struct {
	Status            *string
	Attempt           *int
	Result            *[]byte
	WorkerExecutionID *string
	Iteration         *int
	SupersededBy      *string
	StartedAt         *time.Time
	EndedAt           *time.Time
}

// UpdateWorkflowStepRun applies a partial update with optimistic
// concurrency.
func UpdateWorkflowStepRun(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateWorkflowStepRunFields) (WorkflowStepRunRow, error) {
	q := `UPDATE workflow_step_runs SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.Attempt != nil {
		q += fmt.Sprintf(`, attempt = $%d`, setIdx)
		args = append(args, *f.Attempt)
		setIdx++
	}
	if f.Result != nil {
		q += fmt.Sprintf(`, result = $%d`, setIdx)
		args = append(args, *f.Result)
		setIdx++
	}
	if f.WorkerExecutionID != nil {
		q += fmt.Sprintf(`, worker_execution_id = $%d`, setIdx)
		args = append(args, *f.WorkerExecutionID)
		setIdx++
	}
	if f.Iteration != nil {
		q += fmt.Sprintf(`, iteration = $%d`, setIdx)
		args = append(args, *f.Iteration)
		setIdx++
	}
	if f.SupersededBy != nil {
		q += fmt.Sprintf(`, superseded_by = $%d`, setIdx)
		args = append(args, iterSupersededBy(*f.SupersededBy))
		setIdx++
	}
	if f.StartedAt != nil {
		q += fmt.Sprintf(`, started_at = $%d`, setIdx)
		args = append(args, *f.StartedAt)
		setIdx++
	}
	if f.EndedAt != nil {
		q += fmt.Sprintf(`, ended_at = $%d`, setIdx)
		args = append(args, *f.EndedAt)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, worker_execution_id,
		iteration, superseded_by, started_at, ended_at, version,
		created_at, updated_at`
	var s WorkflowStepRunRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &s.SupersededBy, &s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: update workflow step run: %w", err)
	}
	return s, nil
}

// ListPendingWorkflowRuns returns workflow runs in a non-terminal state
// (pending/running/paused) for a tenant, ordered by creation time. Used
// by the WorkflowReconciler to find runs to progress (docs/03 §2).
func ListPendingWorkflowRuns(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkflowRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref,
		version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs
		WHERE tenant_id = $1 AND status IN ('pending', 'running', 'paused')
		ORDER BY created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list pending workflow runs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		var wiID *string
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
			&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
			&wiID, &r.BoundWorkerRef,
			&r.Version, &r.StartedAt, &r.EndedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow run: %w", err)
		}
		if wiID != nil {
			r.WorkItemID = *wiID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
