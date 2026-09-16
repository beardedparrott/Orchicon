package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectRow is the data-access shape of a projects table row. It maps
// 1:1 to domain.Project but stays in the db package so all SQL is
// centralized here (AGENTS.md invariant #4). Callers translate to/from
// the domain or API types at the service boundary.
type ProjectRow struct {
	ID           string
	TenantID     string
	Name         string
	Slug         string
	Status       string
	Goals        []byte
	Version      int
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ProjectDir   string
	ContextFiles []byte // jsonb: absolute file paths selected as context

	// MaxConcurrentRuns caps how many executions may run concurrently for
	// this project (concurrency guards). 0 = no additional restriction
	// (the tenant cap, or no cap, applies).
	MaxConcurrentRuns int

	// Git work-tree detection cache (non-repo in-place fallback). The
	// WorktreeReconciler shells out to `git rev-parse --is-inside-work-tree`
	// once and records the result here so the loop never repeats the
	// subprocess on every pass. git_detected_at == nil means the cache is
	// empty/undetermined and the reconciler must detect.
	GitWorkTree   bool
	GitDetectedAt *time.Time

	// RepoSlug is the cached git origin ("owner/repo") of the project_dir,
	// captured alongside git detection. Used to derive deterministic
	// per-branch PR links without a provider call. NULL when the project is
	// not git-backed or the origin is unknown.
	RepoSlug *string

	// GitStrategy controls how worktrees materialize: local=push branch only, pr=push+PR, none=ephemeral. Values: local, pr, none.
	GitStrategy string

	// DefaultRuntimeImage is the project-level default runtime container
	// image tag (always-container runtime). NULL (nil) = inherit
	// tenant/base. Copied onto work items at create time when the caller
	// passes an empty runtime_image; explicit per-item values win, and
	// updating the default never retro-mutates existing items.
	DefaultRuntimeImage *string

	// ExecutionMode controls where native executions run: "runtime"
	// (default) = always-container, "local" = in-process allowed with an
	// honest prompt block + a hard DSN fence.
	ExecutionMode string
}

// Execution modes for ProjectRow.ExecutionMode (always-container runtime):
//   - ExecutionModeRuntime (default): executions run inside the run's
//     container; runtime mode fails LOUD without a daemon.
//   - ExecutionModeLocal: in-process execution allowed; the prompt renders
//     an honest local block and the dispatcher enforces the DSN fence.
const (
	ExecutionModeRuntime = "runtime"
	ExecutionModeLocal   = "local"
)

// BaseRuntimeImage is the fallback image of the resolve chain
// (explicit work-item image -> project default -> base). It is never
// empty and never the no-serve sentinel.
const BaseRuntimeImage = "orchicon-runtime:base"

// ErrNotFound is returned when a single-row query matches no rows. The
// data-access layer treats this as a not-found condition; the API layer
// maps it to connect.CodeNotFound.
var ErrNotFound = errors.New("db: not found")

// ErrVersionConflict is returned when an optimistic-concurrency UPDATE
// matches no row because the version is stale but the row still exists.
// It is distinct from ErrNotFound so callers can retry or map the error
// without the misleading "db: not found" for an existing item.
var ErrVersionConflict = errors.New("db: version conflict")

// CreateProject inserts a new project row within the given tenant
// transaction. The caller controls the transaction so the outbox row can
// be enqueued in the same atomic unit (docs/09 §6). Optimistic
// concurrency is not needed on insert; version starts at 1. The
// generated id, timestamps, and version are returned via RETURNING.
//
// The tenant_id is written from p.TenantID (the primary isolation layer)
// and RLS is the backstop (docs/09 §8.5).
func CreateProject(ctx context.Context, tx pgx.Tx, p ProjectRow) (ProjectRow, error) {
	const q = `INSERT INTO projects
		(id, tenant_id, name, slug, status, goals, project_dir, max_concurrent_runs, git_strategy, default_runtime_image, execution_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at,
			project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode`
	row := p
	if row.GitStrategy == "" {
		row.GitStrategy = "local"
	}
	p.GitStrategy = row.GitStrategy
	if row.ExecutionMode == "" {
		row.ExecutionMode = ExecutionModeRuntime
	}
	p.ExecutionMode = row.ExecutionMode
	err := tx.QueryRow(ctx, q,
		p.ID, p.TenantID, p.Name, p.Slug, p.Status, p.Goals, p.ProjectDir, p.MaxConcurrentRuns, p.GitStrategy, p.DefaultRuntimeImage, p.ExecutionMode,
	).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.Slug, &row.Status, &row.Goals,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
		&row.ProjectDir, &row.ContextFiles, &row.MaxConcurrentRuns, &row.GitWorkTree, &row.GitDetectedAt, &row.RepoSlug, &row.GitStrategy, &row.DefaultRuntimeImage, &row.ExecutionMode,
	)
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: create project: %w", err)
	}
	return row, nil
}

// GetProject fetches a single project by id within the tenant scope.
// The tenant_id is injected into the WHERE clause as the primary
// isolation layer; RLS is the backstop (docs/09 §8.5).
func GetProject(ctx context.Context, tx pgx.Tx, tenantID, id string) (ProjectRow, error) {
	const q = `SELECT id, tenant_id, name, slug, status, goals, version,
		created_at, updated_at, project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode
		FROM projects WHERE id = $1 AND tenant_id = $2`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.MaxConcurrentRuns, &p.GitWorkTree, &p.GitDetectedAt, &p.RepoSlug, &p.GitStrategy, &p.DefaultRuntimeImage, &p.ExecutionMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: get project: %w", err)
	}
	return p, nil
}

// ListProjectsFilter scopes a list query to a tenant with optional
// search, status filter, and sort.
type ListProjectsFilter struct {
	TenantID        string
	ExcludeStatuses []string
	PageSize        int
	AfterID         string
	Search          string
	Status          string
	SortBy          string // "name", "status", "created_at" (default)
	SortOrder       string // "asc" or "desc" (default "asc")
}

// ListProjects returns a page of projects for the tenant with cursor-based
// pagination, optional search/filter, and configurable sort.
func ListProjects(ctx context.Context, tx pgx.Tx, f ListProjectsFilter) ([]ProjectRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	args := []any{f.TenantID}
	where := `tenant_id = $1`
	idx := 2
	if f.Search != "" {
		where += fmt.Sprintf(` AND (name ILIKE $%d OR slug ILIKE $%d)`, idx, idx)
		args = append(args, "%"+f.Search+"%")
		idx++
	}
	if f.Status != "" {
		where += fmt.Sprintf(` AND status = $%d`, idx)
		args = append(args, f.Status)
		idx++
	}
	if len(f.ExcludeStatuses) > 0 {
		where += fmt.Sprintf(` AND status <> ALL($%d)`, idx)
		args = append(args, f.ExcludeStatuses)
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
	// THE CURSOR MUST AGREE WITH THE ORDER. It used to be a bare `id > $n` while the default ordering
	// is (created_at ASC, ...) — two different orders, so page 2 could both repeat and skip rows. That
	// is not theoretical here: the live tenant has 499 project pairs whose id order DISAGREES with
	// their created_at order, because an id is minted when a row is CREATED while created_at is
	// assigned by the database, and a bulk import or a restored dump reorders the two.
	//
	// The cursor is now a keyset on the SAME (sort key, id) tuple the ORDER BY uses — the pattern
	// ListExecutions and ListWorkItems already use — so page N+1 continues exactly where page N
	// stopped, whatever the direction and whichever column is the sort key.
	if f.AfterID != "" {
		cmp := ">"
		if sortOrder == "DESC" {
			cmp = "<"
		}
		where += fmt.Sprintf(` AND (%s, id) %s (
			SELECT p2.%s, p2.id FROM projects p2
			WHERE p2.tenant_id = $1 AND p2.id = $%d)`, sortBy, cmp, sortBy, idx)
		args = append(args, f.AfterID)
		idx++
	}
	q := fmt.Sprintf(`SELECT id, tenant_id, name, slug, status, goals, version,
		created_at, updated_at, project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode
		FROM projects
		WHERE %s
		ORDER BY %s %s, id %s LIMIT $%d`, where, sortBy, sortOrder, sortOrder, idx)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list projects: %w", err)
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		var p ProjectRow
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status,
			&p.Goals, &p.Version, &p.CreatedAt, &p.UpdatedAt,
			&p.ProjectDir, &p.ContextFiles, &p.MaxConcurrentRuns, &p.GitWorkTree, &p.GitDetectedAt, &p.RepoSlug, &p.GitStrategy, &p.DefaultRuntimeImage, &p.ExecutionMode); err != nil {
			return nil, fmt.Errorf("db: scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProjectFields is a partial update applied with optimistic
// concurrency: the row is updated only if its version matches
// expectedVersion, then version is bumped (docs/09 §5). Only non-nil
// fields are written; nil fields are left untouched (field-mask
// semantics — docs/07 §5.4).
type UpdateProjectFields struct {
	Name                *string
	Slug                *string
	Goals               *[]byte
	ProjectDir          *string
	ContextFiles        *[]byte
	MaxConcurrentRuns   *int
	GitStrategy         *string
	DefaultRuntimeImage *string
	ExecutionMode       *string
}

// UpdateProject applies a partial update with optimistic concurrency.
// The tenant_id is injected into the WHERE clause as the primary
// isolation layer. Returns ErrNotFound if no row matches the
// id+tenant+version.
func UpdateProject(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateProjectFields) (ProjectRow, error) {
	q := `UPDATE projects SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Name != nil {
		q += fmt.Sprintf(`, name = $%d`, setIdx)
		args = append(args, *f.Name)
		setIdx++
	}
	if f.Slug != nil {
		q += fmt.Sprintf(`, slug = $%d`, setIdx)
		args = append(args, *f.Slug)
		setIdx++
	}
	if f.Goals != nil {
		q += fmt.Sprintf(`, goals = $%d`, setIdx)
		args = append(args, *f.Goals)
		setIdx++
	}
	if f.ProjectDir != nil {
		q += fmt.Sprintf(`, project_dir = $%d`, setIdx)
		args = append(args, *f.ProjectDir)
		setIdx++
		// A project_dir change invalidates the cached git detection: reset
		// the cache to "undetermined" (git_detected_at = NULL) so the
		// WorktreeReconciler re-detects on its next pass instead of trusting
		// a stale value for the old directory.
		q += fmt.Sprintf(`, git_work_tree = false, git_detected_at = NULL`)
	}
	if f.ContextFiles != nil {
		q += fmt.Sprintf(`, context_files = $%d`, setIdx)
		args = append(args, *f.ContextFiles)
		setIdx++
	}
	if f.MaxConcurrentRuns != nil {
		q += fmt.Sprintf(`, max_concurrent_runs = $%d`, setIdx)
		args = append(args, *f.MaxConcurrentRuns)
		setIdx++
	}
	if f.GitStrategy != nil {
		q += fmt.Sprintf(`, git_strategy = $%d`, setIdx)
		args = append(args, *f.GitStrategy)
		setIdx++
	}
	if f.DefaultRuntimeImage != nil {
		q += fmt.Sprintf(`, default_runtime_image = $%d`, setIdx)
		args = append(args, *f.DefaultRuntimeImage)
		setIdx++
	}
	if f.ExecutionMode != nil {
		q += fmt.Sprintf(`, execution_mode = $%d`, setIdx)
		args = append(args, *f.ExecutionMode)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at, project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.MaxConcurrentRuns, &p.GitWorkTree, &p.GitDetectedAt, &p.RepoSlug, &p.GitStrategy, &p.DefaultRuntimeImage, &p.ExecutionMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: update project: %w", err)
	}
	return p, nil
}

// UpdateProjectGitDetection records the WorktreeReconciler's cached git
// work-tree detection on the project row (non-repo in-place fallback),
// along with the remote origin's owner/repo slug (for deterministic
// per-branch PR links). The update is guarded by optimistic concurrency; a
// lost-write race with a concurrent reconciler pass simply converges on the
// next pass, so the caller treats a failure as best-effort.
func UpdateProjectGitDetection(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, isWorkTree bool, repoSlug string) (ProjectRow, error) {
	q := `UPDATE projects SET updated_at = now(), version = version + 1,
		git_work_tree = $4, git_detected_at = now(), repo_slug = $5
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at, project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, isWorkTree, repoSlug).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.MaxConcurrentRuns, &p.GitWorkTree, &p.GitDetectedAt, &p.RepoSlug, &p.GitStrategy, &p.DefaultRuntimeImage, &p.ExecutionMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: update project git detection: %w", err)
	}
	return p, nil
}

// DeleteProject removes a project and EVERYTHING that belongs to it.
//
// THE CASCADE IS THE CONTRACT, and it used to be incomplete in a way that reached production and
// stayed there. The work hierarchy was covered (step runs, runs, versions, workflows, dependencies,
// work items) but the TELEMETRY tables were not — and none of them has an FK to projects, so nothing
// blocked the delete and nothing pointed at the leftovers afterwards. Measured on the live dev
// tenant after every project had been removed: 1264 orphaned worker_executions, 150 orphaned
// recovery_executions, 9959 orphaned usage_records and 132 orphaned recurring_run_history rows, all
// carrying a project_id that no longer existed. Deleting a project in the GUI left all of it behind;
// the DB-backed tests, which create and drop projects constantly, left a large share of it.
//
// Nine tables carry a project_id and only ONE of them (project_mcp_servers) declares a foreign key,
// which is exactly why this has to be explicit: a missing delete is silent rather than an error.
//
// ORDER MATTERS, leaves first, so nothing is left mid-cascade if a later statement fails and so the
// row counts a caller observes step down rather than flicker:
//
//  1. execution_session_parts  — children of the executions (they DO cascade, but deleting them
//     first keeps this function correct even if that FK is ever changed)
//  2. usage_records            — reference executions AND the project
//  3. worker_executions        — the project's executions
//  4. continuation_plans       — children of the recoveries
//  5. recovery_step_runs       — children of the recoveries
//  6. recovery_executions      — the project's recoveries
//  7. recurring_run_history    — references the runs (deleted below) and the work items
//  8. workflow_step_runs       — children of the runs
//  9. workflow_runs
//
// 10. workflow_versions        — children of the workflows
// 11. workflows
// 12. work_item_attachments    — children of the work items
// 13. work_item_dependencies
// 14. work_items
// 15. project_mcp_servers      — the one real FK to projects
// 16. the project
//
// Every statement is scoped by BOTH tenant_id and the project, so a cross-tenant id can never match
// (defence in depth behind row-level security).
func DeleteProject(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	// Each entry is one cascade step; the loop keeps the order auditable in one place rather than
	// spread over 60 lines of near-identical statements, where a missing one is invisible.
	steps := []struct {
		what string
		q    string
	}{
		{"execution session parts", `DELETE FROM execution_session_parts
			WHERE execution_id IN (SELECT id FROM worker_executions WHERE tenant_id = $1 AND project_id = $2)`},
		{"usage records", `DELETE FROM usage_records WHERE tenant_id = $1 AND project_id = $2`},
		{"worker executions", `DELETE FROM worker_executions WHERE tenant_id = $1 AND project_id = $2`},
		{"continuation plans", `DELETE FROM continuation_plans
			WHERE recovery_id IN (SELECT id FROM recovery_executions WHERE tenant_id = $1 AND project_id = $2)`},
		{"recovery step runs", `DELETE FROM recovery_step_runs
			WHERE recovery_id IN (SELECT id FROM recovery_executions WHERE tenant_id = $1 AND project_id = $2)`},
		{"recovery executions", `DELETE FROM recovery_executions WHERE tenant_id = $1 AND project_id = $2`},
		{"recurring run history", `DELETE FROM recurring_run_history
			WHERE tenant_id = $1 AND (
				workflow_run_id IN (SELECT id FROM workflow_runs WHERE tenant_id = $1 AND project_id = $2)
				OR work_item_id IN (SELECT id FROM work_items WHERE tenant_id = $1 AND project_id = $2))`},
		{"workflow step runs", `DELETE FROM workflow_step_runs
			WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE tenant_id = $1 AND project_id = $2)`},
		{"workflow runs", `DELETE FROM workflow_runs WHERE tenant_id = $1 AND project_id = $2`},
		{"workflow versions", `DELETE FROM workflow_versions
			WHERE workflow_id IN (SELECT id FROM workflows WHERE tenant_id = $1 AND project_id = $2)`},
		{"workflows", `DELETE FROM workflows WHERE tenant_id = $1 AND project_id = $2`},
		{"work item attachments", `DELETE FROM work_item_attachments
			WHERE work_item_id IN (SELECT id FROM work_items WHERE tenant_id = $1 AND project_id = $2)`},
		{"work item dependencies", `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND project_id = $2`},
		{"work items", `DELETE FROM work_items WHERE tenant_id = $1 AND project_id = $2`},
		{"project mcp servers", `DELETE FROM project_mcp_servers WHERE tenant_id = $1 AND project_id = $2`},
	}
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.q, tenantID, id); err != nil {
			return fmt.Errorf("db: delete project cascade %s: %w", s.what, err)
		}
	}
	// The project itself. A missing row is not an error here: the caller may be retrying a delete
	// whose cascade already ran, and the contract this function owes is "the project and its data are
	// gone" — which is true either way.
	if _, err := tx.Exec(ctx,
		`DELETE FROM projects WHERE id = $1 AND tenant_id = $2`,
		id, tenantID); err != nil {
		return fmt.Errorf("db: delete project: %w", err)
	}
	return nil
}

// ArchiveProject transitions a project to archived status with optimistic
// concurrency. The tenant_id is injected into the WHERE clause. Returns
// the updated row or ErrNotFound.
func ArchiveProject(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int) (ProjectRow, error) {
	const q = `UPDATE projects
		SET status = 'archived', updated_at = now(), version = version + 1
		WHERE tenant_id = $1 AND id = $2 AND version = $3
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at,
			project_dir, context_files, max_concurrent_runs, git_work_tree, git_detected_at, repo_slug, git_strategy, default_runtime_image, execution_mode`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
		&p.ProjectDir, &p.ContextFiles, &p.MaxConcurrentRuns, &p.GitWorkTree, &p.GitDetectedAt, &p.RepoSlug, &p.GitStrategy, &p.DefaultRuntimeImage, &p.ExecutionMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: archive project: %w", err)
	}
	return p, nil
}

// RequireProjectActive checks that the project with the given ID exists
// and has status='active'. Returns ErrNotFound if the project doesn't
// exist, or a descriptive error if it's in another state.
func RequireProjectActive(ctx context.Context, tx pgx.Tx, tenantID, projectID string) error {
	const q = `SELECT status FROM projects WHERE id = $1 AND tenant_id = $2`
	var status string
	err := tx.QueryRow(ctx, q, projectID, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("db: check project active: %w", err)
	}
	if status != "active" {
		return fmt.Errorf("project is %q, must be active", status)
	}
	return nil
}
