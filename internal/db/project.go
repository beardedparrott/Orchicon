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

// ListProjectsFilter scopes a list query to a tenant. Excluded statuses
// (e.g. deleted) are filtered out by default; the caller may override.
type ListProjectsFilter struct {
	TenantID        string
	ExcludeStatuses []string // e.g. []string{"deleted"}
	PageSize        int
	AfterID         string // cursor: list rows with id > AfterID (ULID ordering)
}

// ListProjects returns a page of projects for the tenant, ordered by ULID
// id for stable cursor pagination (docs/07 §5.2). The cursor is the last
// id of the page; the client passes it as page_token. The tenant_id is
// injected into the WHERE clause as the primary isolation layer.
func ListProjects(ctx context.Context, tx pgx.Tx, f ListProjectsFilter) ([]ProjectRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, name, slug, status, goals, version,
		created_at, updated_at
		FROM projects
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)`
	args := []any{f.TenantID, f.AfterID}
	if len(f.ExcludeStatuses) > 0 {
		q += fmt.Sprintf(` AND status <> ALL($%d)`, len(args)+1)
		args = append(args, f.ExcludeStatuses)
	}
	q += ` ORDER BY id ASC LIMIT $` + fmt.Sprint(len(args)+1)
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
