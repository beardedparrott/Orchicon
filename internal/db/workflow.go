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
// (steps, inputs, outputs) lives in
// WorkflowVersionRow. type distinguishes one-shot workflows from
// repeatable templates (docs/11 §2.1).
type WorkflowRow struct {
	ID             string
	TenantID       string
	ProjectID      string // empty for tenant-level templates
	Name           string
	CurrentVersion int
	Status         string
	Type           string // "one_shot" or "template"
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
	RuntimeImage    string // resolved runtime container image tag at run start
	RuntimeReady    bool   // runtime-serve readiness gate: executions dispatch only once true
	WorktreeStatus  string // WorktreeReconciler provisioning state (pending/ready/skipped/failed/pruned)
	WorktreePath    string // isolated working tree path (git-backed runs)
	WorktreeBranch  string // deterministic branch created for the run
	Version         int
	StartedAt       *time.Time
	EndedAt         *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// WorkflowStepRunRow is the data-access shape of a workflow_step_runs
// table row (docs/09 §3.4). The runtime state of a single step within
// a WorkflowRun. iteration and superseded_by track loop decision
// re-entry (docs/11 §3.4). worktree_* carry the per-step-run isolated
// working tree provisioned for parallel-branch children
// (architecture-notes/concurrent-step-run-dispatch.md D2); non-branch
// steps keep them at their defaults ('pending'/empty).
type WorkflowStepRunRow struct {
	ID                string
	TenantID          string
	WorkflowRunID     string
	StepID            string
	StepName          string
	StepKind          string
	Status            string
	Attempt           int
	Result            []byte // jsonb
	WorkerExecutionID string
	Iteration         int    // re-entry count (0 for first dispatch)
	SupersededBy      string // step run id that superseded this one
	WorktreeStatus    string // WorktreeReconciler provisioning state (pending/ready/skipped/failed/pruned)
	WorktreePath      string // isolated working tree path (parallel-branch children)
	WorktreeBranch    string // deterministic branch created for the step run
	StartedAt         *time.Time
	EndedAt           *time.Time
	Version           int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateWorkflow inserts a new workflow header row within the given
// tenant transaction. The caller controls the transaction so the outbox
// row can be enqueued in the same atomic unit (docs/09 §6). Version
// starts at 1; current_version starts at 0 (no published versions yet).
func CreateWorkflow(ctx context.Context, tx pgx.Tx, w WorkflowRow) (WorkflowRow, error) {
	const q = `INSERT INTO workflows
		(id, tenant_id, project_id, name, current_version, status, type)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, project_id, name, current_version, status, type,
			version, created_at, updated_at`
	row := w
	err := tx.QueryRow(ctx, q,
		w.ID, w.TenantID, w.ProjectID, w.Name, w.CurrentVersion, w.Status, w.Type,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.Name, &row.CurrentVersion,
		&row.Status, &row.Type, &row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkflowRow{}, fmt.Errorf("db: create workflow: %w", err)
	}
	return row, nil
}

// GetWorkflow fetches a single workflow by id within the tenant scope.
func GetWorkflow(ctx context.Context, tx pgx.Tx, tenantID, id string) (WorkflowRow, error) {
	const q = `SELECT id, tenant_id, project_id, name, current_version, status, type,
		version, created_at, updated_at
		FROM workflows WHERE id = $1 AND tenant_id = $2`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Type, &w.Version, &w.CreatedAt, &w.UpdatedAt,
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
	TenantID      string
	ProjectID     string // empty = all (including templates)
	TemplatesOnly bool   // if true, return only templates (project_id = '')
	Status        string // empty = all statuses
	Type          string // filter by type: "template" or "one_shot"; empty = all
	PageSize      int
	AfterID       string
	Search        string
	SortBy        string // "name", "status", "created_at" (default "id")
	SortOrder     string // "asc" or "desc" (default "asc")
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
	if f.TemplatesOnly {
		where += fmt.Sprintf(` AND project_id = ''`)
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
	if f.Type != "" {
		where += fmt.Sprintf(` AND type = $%d`, idx)
		args = append(args, f.Type)
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
	q := fmt.Sprintf(`SELECT id, tenant_id, project_id, name, current_version, status, type,
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
			&w.Status, &w.Type, &w.Version, &w.CreatedAt, &w.UpdatedAt,
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
		RETURNING id, tenant_id, project_id, name, current_version, status, type,
			version, created_at, updated_at`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Type, &w.Version, &w.CreatedAt, &w.UpdatedAt,
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
		RETURNING id, tenant_id, project_id, name, current_version, status, type,
			version, created_at, updated_at`
	var w WorkflowRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, newVersion).Scan(
		&w.ID, &w.TenantID, &w.ProjectID, &w.Name, &w.CurrentVersion,
		&w.Status, &w.Type, &w.Version, &w.CreatedAt, &w.UpdatedAt,
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
		 steps, inputs, outputs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, tenant_id, workflow_id, version, version_note, status,
			steps, inputs, outputs, published_at, created_at`
	row := v
	err := tx.QueryRow(ctx, q,
		v.ID, v.TenantID, v.WorkflowID, v.Version, v.VersionNote, v.Status,
		v.Steps, v.Inputs, v.Outputs,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowID, &row.Version, &row.VersionNote,
		&row.Status, &row.Steps, &row.Inputs, &row.Outputs,
		&row.PublishedAt, &row.CreatedAt,
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
			steps, inputs, outputs, published_at, created_at`
	var v WorkflowVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workflowID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.PublishedAt, &v.CreatedAt,
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
		steps, inputs, outputs, published_at, created_at
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
		&v.PublishedAt, &v.CreatedAt,
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
		steps, inputs, outputs, published_at, created_at
		FROM workflow_versions
		WHERE tenant_id = $1 AND workflow_id = $2 AND version = $3`
	var v WorkflowVersionRow
	err := tx.QueryRow(ctx, q, tenantID, workflowID, version).Scan(
		&v.ID, &v.TenantID, &v.WorkflowID, &v.Version, &v.VersionNote,
		&v.Status, &v.Steps, &v.Inputs, &v.Outputs,
		&v.PublishedAt, &v.CreatedAt,
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
		steps, inputs, outputs, published_at, created_at
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
			&v.PublishedAt, &v.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteWorkflowVersion hard-deletes a single workflow version. At
// least one version must remain after deletion (docs/02 §2.4).
func DeleteWorkflowVersion(ctx context.Context, tx pgx.Tx, tenantID, workflowID, versionID string) error {
	// Verify the version exists and that at least one other version
	// would remain.
	var status string
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT wv.status, (SELECT count(*) FROM workflow_versions WHERE tenant_id = $1 AND workflow_id = $2) AS cnt
		 FROM workflow_versions wv WHERE tenant_id = $1 AND id = $3 AND workflow_id = $2`,
		tenantID, workflowID, versionID).Scan(&status, &count); err != nil {
		return fmt.Errorf("db: get workflow version: %w", err)
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
		 current_step, run_context, work_item_id, bound_worker_ref, runtime_image, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, tenant_id, workflow_id, workflow_version, project_id, status,
			current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
			runtime_ready, worktree_status, worktree_path, worktree_branch,
			version, started_at, ended_at, created_at, updated_at`
	row := r
	var wiID *string
	err := tx.QueryRow(ctx, q,
		r.ID, r.TenantID, r.WorkflowID, r.WorkflowVersion, r.ProjectID,
		r.Status, r.CurrentStep, r.RunContext,
		workItemVal(r.WorkItemID), boundWorkerRefVal(r.BoundWorkerRef),
		r.RuntimeImage, r.StartedAt,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowID, &row.WorkflowVersion,
		&row.ProjectID, &row.Status, &row.CurrentStep, &row.RunContext,
		&wiID, &row.BoundWorkerRef, &row.RuntimeImage, &row.RuntimeReady,
		&row.WorktreeStatus, &row.WorktreePath, &row.WorktreeBranch,
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
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
			version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs WHERE id = $1 AND tenant_id = $2`
	var r WorkflowRunRow
	var wiID *string
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
		&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
		&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
		&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
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
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
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
			&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
			&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
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
	// RuntimeImage is the resolved runtime container image tag captured
	// at run start.
	RuntimeImage *string
	// RuntimeReady flips the runtime-serve readiness gate. The reconciler
	// sets it false at run start and the async ensure-serving pass sets it
	// true once the workflow's runtime opencode serve is proven usable.
	RuntimeReady *bool
	// WorktreeStatus/Path/Branch are written by the WorktreeReconciler
	// once the run's isolated working tree is provisioned (or skipped/
	// failed). All three are set atomically on the ready transition.
	WorktreeStatus *string
	WorktreePath   *string
	WorktreeBranch *string
	// ClearEndedAt clears ended_at to NULL regardless of the EndedAt
	// pointer. Used when resuming a terminalized (failed) run — the ended
	// timestamp must be cleared so the restarted run's lifecycle is honest.
	ClearEndedAt bool
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
	if f.ClearEndedAt {
		q += `, ended_at = NULL`
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
	if f.RuntimeImage != nil {
		q += fmt.Sprintf(`, runtime_image = $%d`, setIdx)
		args = append(args, *f.RuntimeImage)
		setIdx++
	}
	if f.RuntimeReady != nil {
		q += fmt.Sprintf(`, runtime_ready = $%d`, setIdx)
		args = append(args, *f.RuntimeReady)
		setIdx++
	}
	if f.WorktreeStatus != nil {
		q += fmt.Sprintf(`, worktree_status = $%d`, setIdx)
		args = append(args, *f.WorktreeStatus)
		setIdx++
	}
	if f.WorktreePath != nil {
		q += fmt.Sprintf(`, worktree_path = $%d`, setIdx)
		args = append(args, *f.WorktreePath)
		setIdx++
	}
	if f.WorktreeBranch != nil {
		q += fmt.Sprintf(`, worktree_branch = $%d`, setIdx)
		args = append(args, *f.WorktreeBranch)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
			version, started_at, ended_at, created_at, updated_at`
	var r WorkflowRunRow
	var wiID *string
	err := tx.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
		&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
		&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
		&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
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
			worktree_status, worktree_path, worktree_branch,
			started_at, ended_at, version, created_at, updated_at`
	row := s
	var supBy *string
	err := tx.QueryRow(ctx, q,
		s.ID, s.TenantID, s.WorkflowRunID, s.StepID, s.StepName, s.StepKind,
		s.Status, s.Attempt, s.Result, s.WorkerExecutionID,
		s.Iteration, iterSupersededBy(s.SupersededBy),
		s.StartedAt,
	).Scan(
		&row.ID, &row.TenantID, &row.WorkflowRunID, &row.StepID, &row.StepName,
		&row.StepKind, &row.Status, &row.Attempt, &row.Result,
		&row.WorkerExecutionID, &row.Iteration, &supBy,
		&row.WorktreeStatus, &row.WorktreePath, &row.WorktreeBranch,
		&row.StartedAt, &row.EndedAt, &row.Version,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: create workflow step run: %w", err)
	}
	if supBy != nil {
		row.SupersededBy = *supBy
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
		iteration, superseded_by, worktree_status, worktree_path, worktree_branch,
		started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs WHERE id = $1 AND tenant_id = $2`
	var s WorkflowStepRunRow
	var supBy *string
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
		&s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: get workflow step run: %w", err)
	}
	if supBy != nil {
		s.SupersededBy = *supBy
	}
	return s, nil
}

// GetWorkflowStepRunByStep returns the step run for a given
// (workflow_run_id, step_id) pair. Used by the reconciler to look up
// the runtime state of a step within a run.
func GetWorkflowStepRunByStep(ctx context.Context, tx pgx.Tx, tenantID, runID, stepID string) (WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, worktree_status, worktree_path, worktree_branch,
		started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs
		WHERE tenant_id = $1 AND workflow_run_id = $2 AND step_id = $3
		ORDER BY created_at DESC LIMIT 1`
	var s WorkflowStepRunRow
	var supBy *string
	err := tx.QueryRow(ctx, q, tenantID, runID, stepID).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
		&s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: get workflow step run by step: %w", err)
	}
	if supBy != nil {
		s.SupersededBy = *supBy
	}
	return s, nil
}

// GetWorkflowStepRunByExecution returns the workflow step run linked to a
// worker execution (via worker_execution_id). Used to resolve the actual
// per-step system prompt (_prompt) an execution was dispatched with — the
// execution page should show THIS, not the shared work item's prompt_context
// (which holds the FIRST step's composite and never changes — a field incident
// showed every worker as "DevOps Engineer").
func GetWorkflowStepRunByExecution(ctx context.Context, tx pgx.Tx, tenantID, executionID string) (WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, worktree_status, worktree_path, worktree_branch,
		started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs
		WHERE tenant_id = $1 AND worker_execution_id = $2
		ORDER BY created_at DESC LIMIT 1`
	var s WorkflowStepRunRow
	var supBy *string
	err := tx.QueryRow(ctx, q, tenantID, executionID).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
		&s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: get workflow step run by execution: %w", err)
	}
	if supBy != nil {
		s.SupersededBy = *supBy
	}
	return s, nil
}

// ListWorkflowStepRuns returns all step runs for a workflow run.
func ListWorkflowStepRuns(ctx context.Context, tx pgx.Tx, tenantID, runID string) ([]WorkflowStepRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, 		worker_execution_id,
		iteration, superseded_by, worktree_status, worktree_path, worktree_branch,
		started_at, ended_at, version,
		created_at, updated_at
		FROM workflow_step_runs
		WHERE tenant_id = $1 AND workflow_run_id = $2
		ORDER BY created_at ASC, id ASC`
	rows, err := tx.Query(ctx, q, tenantID, runID)
	if err != nil {
		return nil, fmt.Errorf("db: list workflow step runs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowStepRunRow
	for rows.Next() {
		var s WorkflowStepRunRow
		var supBy *string
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
			&s.StepKind, &s.Status, &s.Attempt, &s.Result,
			&s.WorkerExecutionID,
			&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
			&s.StartedAt, &s.EndedAt, &s.Version,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow step run: %w", err)
		}
		if supBy != nil {
			s.SupersededBy = *supBy
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
	WorktreeStatus    *string
	WorktreePath      *string
	WorktreeBranch    *string
	StartedAt         *time.Time
	EndedAt           *time.Time
	// ClearEndedAt clears ended_at to NULL regardless of the EndedAt
	// pointer (used when retrying a failed/blocked step run so the
	// re-dispatched run's lifecycle is honest).
	ClearEndedAt bool
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
	if f.WorktreeStatus != nil {
		q += fmt.Sprintf(`, worktree_status = $%d`, setIdx)
		args = append(args, *f.WorktreeStatus)
		setIdx++
	}
	if f.WorktreePath != nil {
		q += fmt.Sprintf(`, worktree_path = $%d`, setIdx)
		args = append(args, *f.WorktreePath)
		setIdx++
	}
	if f.WorktreeBranch != nil {
		q += fmt.Sprintf(`, worktree_branch = $%d`, setIdx)
		args = append(args, *f.WorktreeBranch)
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
	if f.ClearEndedAt {
		q += `, ended_at = NULL`
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, workflow_run_id, step_id, step_name, step_kind,
		status, attempt, result, worker_execution_id,
		iteration, superseded_by, worktree_status, worktree_path, worktree_branch,
		started_at, ended_at, version,
		created_at, updated_at`
	var s WorkflowStepRunRow
	var supBy *string
	err := tx.QueryRow(ctx, q, args...).Scan(
		&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
		&s.StepKind, &s.Status, &s.Attempt, &s.Result,
		&s.WorkerExecutionID,
		&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
		&s.StartedAt, &s.EndedAt, &s.Version,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return WorkflowStepRunRow{}, fmt.Errorf("db: update workflow step run: %w", err)
	}
	if supBy != nil {
		s.SupersededBy = *supBy
	}
	return s, nil
}

// ListPendingWorkflowRuns returns workflow runs in a non-terminal state
// (pending/running/paused) for a tenant, ordered by creation time. Used
// by the WorkflowReconciler to find runs to progress (docs/03 §2).
func ListPendingWorkflowRuns(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkflowRunRow, error) {
	const q = `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
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
			&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
			&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
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

// ListWorktreeCandidates returns workflow runs the WorktreeReconciler
// should provision an isolated working tree for: non-terminal runs that
// have a bound project (one-shot runs bind their project on first
// dispatch — UpdateWorkflowRunFields.ProjectID — so empty project_id
// runs are skipped) and have not yet been provisioned (worktree_status
// 'pending'; 'ready'/'skipped'/'failed' runs are left alone). The
// 'pending' default makes the scan self-healing for runs armed before
// the columns existed. Batch-capped so one pass can't monopolize the
// reconciler goroutine.
func ListWorktreeCandidates(ctx context.Context, tx pgx.Tx, tenantID string, limit int) ([]WorkflowRunRow, error) {
	if limit <= 0 {
		limit = 16
	}
	const q = `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
			version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs
		WHERE tenant_id = $1 AND status IN ('pending', 'running') AND project_id <> ''
		  AND worktree_status = 'pending'
		ORDER BY created_at ASC
		LIMIT $2`
	rows, err := tx.Query(ctx, q, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list worktree candidates: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		var wiID *string
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
			&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
			&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
			&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
			&r.Version, &r.StartedAt, &r.EndedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan worktree candidate: %w", err)
		}
		if wiID != nil {
			r.WorkItemID = *wiID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListTerminalRunsWithWorktrees returns workflow runs in a terminal state
// (completed/failed/aborted) that still have a recorded worktree to reap
// (worktree_status 'ready' with a recorded path). This is the scan-side
// discovery surface for the WorktreeReconciler's prune pass — terminal
// runs whose worktrees were already pruned are excluded by the
// 'ready' + non-empty path predicate, so re-scanning is a no-op.
// Batch-capped to mirror ListWorktreeCandidates.
func ListTerminalRunsWithWorktrees(ctx context.Context, tx pgx.Tx, tenantID string, limit int) ([]WorkflowRunRow, error) {
	if limit <= 0 {
		limit = 16
	}
	const q = `SELECT id, tenant_id, workflow_id, workflow_version, project_id, status,
		current_step, run_context, work_item_id, bound_worker_ref, runtime_image,
		runtime_ready, worktree_status, worktree_path, worktree_branch,
			version, started_at, ended_at, created_at, updated_at
		FROM workflow_runs
		WHERE tenant_id = $1 AND status IN ('completed', 'failed', 'aborted')
		  AND worktree_status = 'ready' AND worktree_path <> ''
		ORDER BY created_at ASC
		LIMIT $2`
	rows, err := tx.Query(ctx, q, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list terminal runs with worktrees: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunRow
	for rows.Next() {
		var r WorkflowRunRow
		var wiID *string
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.WorkflowID, &r.WorkflowVersion,
			&r.ProjectID, &r.Status, &r.CurrentStep, &r.RunContext,
			&wiID, &r.BoundWorkerRef, &r.RuntimeImage, &r.RuntimeReady,
			&r.WorktreeStatus, &r.WorktreePath, &r.WorktreeBranch,
			&r.Version, &r.StartedAt, &r.EndedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan terminal run: %w", err)
		}
		if wiID != nil {
			r.WorkItemID = *wiID
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListWorktreeStepRunCandidates returns workflow step runs the
// WorktreeReconciler should provision a per-branch isolated working tree
// for: step runs belonging to NON-terminal runs that have a bound project,
// whose worktree_status is still 'pending', and whose own step-run status
// is NOT terminal (a succeeded/failed/skipped/blocked step run will never
// dispatch, so it never needs a branch worktree — excluding it keeps
// non-branch residue from starving the scan batch). The caller filters to
// parallel-branch children (the DAG shape decides who gets a branch
// worktree — see parallelBranchChildIDs in the scheduler). The 'pending'
// default makes the scan self-healing for step runs created before the
// columns existed. Batch-capped (paged after afterCreated/afterID so the
// scan can walk past residue rows) so one pass can't monopolize the
// reconciler goroutine.
func ListWorktreeStepRunCandidates(ctx context.Context, tx pgx.Tx, tenantID string, limit int, afterCreated time.Time, afterID string) ([]WorkflowStepRunRow, error) {
	if limit <= 0 {
		limit = 16
	}
	q := `SELECT s.id, s.tenant_id, s.workflow_run_id, s.step_id, s.step_name,
		s.step_kind, s.status, s.attempt, s.result, s.worker_execution_id,
		s.iteration, s.superseded_by, s.worktree_status, s.worktree_path, s.worktree_branch,
		s.started_at, s.ended_at, s.version, s.created_at, s.updated_at
		FROM workflow_step_runs s
		JOIN workflow_runs r ON r.id = s.workflow_run_id AND r.tenant_id = s.tenant_id
		WHERE s.tenant_id = $1 AND r.status IN ('pending', 'running') AND r.project_id <> ''
		  AND s.worktree_status = 'pending'
		  AND s.status NOT IN ('succeeded', 'failed', 'skipped', 'blocked')`
	args := []any{tenantID, limit}
	if !afterCreated.IsZero() {
		q += fmt.Sprintf(` AND (s.created_at, s.id) > ($%d, $%d)`, len(args)+1, len(args)+2)
		args = append(args, afterCreated, afterID)
	}
	q += `
		ORDER BY s.created_at ASC, s.id ASC
		LIMIT $2`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list worktree step run candidates: %w", err)
	}
	defer rows.Close()
	var out []WorkflowStepRunRow
	for rows.Next() {
		var s WorkflowStepRunRow
		var supBy *string
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
			&s.StepKind, &s.Status, &s.Attempt, &s.Result,
			&s.WorkerExecutionID,
			&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
			&s.StartedAt, &s.EndedAt, &s.Version,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan worktree step run candidate: %w", err)
		}
		if supBy != nil {
			s.SupersededBy = *supBy
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListTerminalStepRunsWithWorktrees returns workflow step runs with a
// recorded branch worktree ('ready' + non-empty path) that can be reaped:
// step runs that are themselves terminal (succeeded/failed/skipped/
// blocked/superseded — superseded runs keep their terminal status) OR
// belong to a terminal run (completed/failed/aborted). This is the
// scan-side discovery surface for the WorktreeReconciler's step-run prune
// pass — step runs whose worktrees were already pruned are excluded by the
// 'ready' + non-empty path predicate, so re-scanning is a no-op.
// Batch-capped to mirror ListWorktreeStepRunCandidates.
func ListTerminalStepRunsWithWorktrees(ctx context.Context, tx pgx.Tx, tenantID string, limit int) ([]WorkflowStepRunRow, error) {
	if limit <= 0 {
		limit = 16
	}
	const q = `SELECT s.id, s.tenant_id, s.workflow_run_id, s.step_id, s.step_name,
		s.step_kind, s.status, s.attempt, s.result, s.worker_execution_id,
		s.iteration, s.superseded_by, s.worktree_status, s.worktree_path, s.worktree_branch,
		s.started_at, s.ended_at, s.version, s.created_at, s.updated_at
		FROM workflow_step_runs s
		JOIN workflow_runs r ON r.id = s.workflow_run_id AND r.tenant_id = s.tenant_id
		WHERE s.tenant_id = $1 AND s.worktree_status = 'ready' AND s.worktree_path <> ''
		  AND (s.status IN ('succeeded', 'failed', 'skipped', 'blocked')
		       OR r.status IN ('completed', 'failed', 'aborted'))
		ORDER BY s.created_at ASC
		LIMIT $2`
	rows, err := tx.Query(ctx, q, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list terminal step runs with worktrees: %w", err)
	}
	defer rows.Close()
	var out []WorkflowStepRunRow
	for rows.Next() {
		var s WorkflowStepRunRow
		var supBy *string
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.WorkflowRunID, &s.StepID, &s.StepName,
			&s.StepKind, &s.Status, &s.Attempt, &s.Result,
			&s.WorkerExecutionID,
			&s.Iteration, &supBy, &s.WorktreeStatus, &s.WorktreePath, &s.WorktreeBranch,
			&s.StartedAt, &s.EndedAt, &s.Version,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan terminal step run: %w", err)
		}
		if supBy != nil {
			s.SupersededBy = *supBy
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
