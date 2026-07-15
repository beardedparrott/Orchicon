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
	ID        string
	TenantID  string
	Name      string
	Slug      string
	Status    string
	Goals     []byte
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrNotFound is returned when a single-row query matches no rows. The
// data-access layer treats this as a not-found condition; the API layer
// maps it to connect.CodeNotFound.
var ErrNotFound = errors.New("db: not found")

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
		(id, tenant_id, name, slug, status, goals)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at`
	row := p
	err := tx.QueryRow(ctx, q,
		p.ID, p.TenantID, p.Name, p.Slug, p.Status, p.Goals,
	).Scan(
		&row.ID, &row.TenantID, &row.Name, &row.Slug, &row.Status, &row.Goals,
		&row.Version, &row.CreatedAt, &row.UpdatedAt,
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
		created_at, updated_at
		FROM projects WHERE id = $1 AND tenant_id = $2`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
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
	if f.AfterID != "" {
		where += fmt.Sprintf(` AND id > $%d`, idx)
		args = append(args, f.AfterID)
		idx++
	}
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
	q := fmt.Sprintf(`SELECT id, tenant_id, name, slug, status, goals, version,
		created_at, updated_at
		FROM projects
		WHERE %s
		ORDER BY %s %s LIMIT $%d`, where, sortBy, sortOrder, idx)
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
			&p.Goals, &p.Version, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
	Name  *string
	Goals *[]byte
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
	if f.Goals != nil {
		q += fmt.Sprintf(`, goals = $%d`, setIdx)
		args = append(args, *f.Goals)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: update project: %w", err)
	}
	return p, nil
}

// DeleteProject hard-deletes a project and cascades to all owned entities
// (work items, workflows, workflow versions, workflow runs, step runs).
// The tenant_id is injected into the WHERE clause for isolation.
func DeleteProject(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	// Cascade: delete step runs for all workflow runs in this project
	if _, err := tx.Exec(ctx,
		`DELETE FROM workflow_step_runs
		 WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE tenant_id = $1 AND project_id = $2)`,
		tenantID, id); err != nil {
		return fmt.Errorf("db: delete project cascade step runs: %w", err)
	}
	// Cascade: delete workflow runs
	if _, err := tx.Exec(ctx,
		`DELETE FROM workflow_runs WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, id); err != nil {
		return fmt.Errorf("db: delete project cascade workflow runs: %w", err)
	}
	// Cascade: delete workflow versions
	if _, err := tx.Exec(ctx,
		`DELETE FROM workflow_versions
		 WHERE workflow_id IN (SELECT id FROM workflows WHERE tenant_id = $1 AND project_id = $2)`,
		tenantID, id); err != nil {
		return fmt.Errorf("db: delete project cascade workflow versions: %w", err)
	}
	// Cascade: delete workflows
	if _, err := tx.Exec(ctx,
		`DELETE FROM workflows WHERE tenant_id = $1 AND project_id = $2`,
		tenantID, id); err != nil {
		return fmt.Errorf("db: delete project cascade workflows: %w", err)
	}
	// Cascade: delete work item dependencies
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_item_dependencies WHERE project_id = $1 AND tenant_id = $2`,
		id, tenantID); err != nil {
		return fmt.Errorf("db: delete project cascade work item dependencies: %w", err)
	}
	// Cascade: delete work items
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_items WHERE project_id = $1 AND tenant_id = $2`,
		id, tenantID); err != nil {
		return fmt.Errorf("db: delete project cascade work items: %w", err)
	}
	// Delete the project itself
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
		RETURNING id, tenant_id, name, slug, status, goals, version, created_at, updated_at`
	var p ProjectRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion).Scan(
		&p.ID, &p.TenantID, &p.Name, &p.Slug, &p.Status, &p.Goals,
		&p.Version, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	if err != nil {
		return ProjectRow{}, fmt.Errorf("db: archive project: %w", err)
	}
	return p, nil
}
