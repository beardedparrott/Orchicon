package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// RuntimeImageRow is the data-access shape of a runtime_images table row.
type RuntimeImageRow struct {
	ID                 string
	TenantID           string
	Name               string
	Slug               string
	Description        string
	BaseImageRef       string
	AptPackages        []byte // jsonb
	Toolchains         []byte // jsonb
	Env                []byte // jsonb
	DockerfileOverride string
	Tag                string
	Status             string
	BuildLog           string
	Error              string
	Version            int
	BuiltVersion       int // spec version the current ready image was built from (0 = never built)
	Source             string // "stock" (canned, seeded) or "custom" (tenant-created)
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// RuntimeImageFilter is the list filter for ListRuntimeImages.
type RuntimeImageFilter struct {
	TenantID string
	Status   string // optional status filter; empty = all
	Source   string // optional source filter ("stock"/"custom"); empty = all
	Search   string // optional free-text on name/slug
}

const runtimeImageCols = `id, tenant_id, name, slug, description, base_image_ref,
	apt_packages, toolchains, env, dockerfile_override, tag, status, build_log, error,
	version, built_version, source, created_at, updated_at`

func scanRuntimeImage(row pgx.Row) (RuntimeImageRow, error) {
	var r RuntimeImageRow
	err := row.Scan(&r.ID, &r.TenantID, &r.Name, &r.Slug, &r.Description, &r.BaseImageRef,
		&r.AptPackages, &r.Toolchains, &r.Env, &r.DockerfileOverride, &r.Tag, &r.Status,
		&r.BuildLog, &r.Error, &r.Version, &r.BuiltVersion, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// CreateRuntimeImage inserts a new runtime image spec row.
func CreateRuntimeImage(ctx context.Context, tx pgx.Tx, r RuntimeImageRow) (RuntimeImageRow, error) {
	q := `INSERT INTO runtime_images
		(id, tenant_id, name, slug, description, base_image_ref,
		 apt_packages, toolchains, env, dockerfile_override, tag, status,
		 build_log, error, version, source)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,1,$15)
		RETURNING ` + runtimeImageCols
	return scanRuntimeImage(tx.QueryRow(ctx, q, r.ID, r.TenantID, r.Name, r.Slug, r.Description,
		r.BaseImageRef, r.AptPackages, r.Toolchains, r.Env, r.DockerfileOverride, r.Tag,
		r.Status, r.BuildLog, r.Error, r.Source))
}

// GetRuntimeImage loads one image spec by id (tenant-scoped).
func GetRuntimeImage(ctx context.Context, tx pgx.Tx, tenantID, id string) (RuntimeImageRow, error) {
	q := `SELECT ` + runtimeImageCols + ` FROM runtime_images WHERE id = $1 AND tenant_id = $2`
	r, err := scanRuntimeImage(tx.QueryRow(ctx, q, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// GetRuntimeImageByTag loads one image spec by tag (tenant-scoped). Used by
// the canned-image seeder to match a daemon-reported stock tag to its row.
func GetRuntimeImageByTag(ctx context.Context, tx pgx.Tx, tenantID, tag string) (RuntimeImageRow, error) {
	q := `SELECT ` + runtimeImageCols + ` FROM runtime_images WHERE tenant_id = $1 AND tag = $2`
	r, err := scanRuntimeImage(tx.QueryRow(ctx, q, tenantID, tag))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// GetRuntimeImageForUpdate loads one image spec by id (tenant-scoped) and
// locks the row for the rest of the transaction (SELECT ... FOR UPDATE).
// marking transition is StatusOnly (it must NOT bump `version`, the "spec
// changed" signal), so the version-based OCC check alone cannot serialize
// two simultaneous Deploys — both would pass it and run `docker build` on
// the same tag. The row lock closes that gap: the first caller marks the
// row 'building' and commits; a concurrent caller blocks on the lock and
// then re-reads the committed row (READ COMMITTED re-evaluates after the
// lock wait), sees status 'building', and fails fast.
func GetRuntimeImageForUpdate(ctx context.Context, tx pgx.Tx, tenantID, id string) (RuntimeImageRow, error) {
	q := `SELECT ` + runtimeImageCols + ` FROM runtime_images WHERE id = $1 AND tenant_id = $2 FOR UPDATE`
	r, err := scanRuntimeImage(tx.QueryRow(ctx, q, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListRuntimeImages returns the tenant's image specs, optionally filtered.
func ListRuntimeImages(ctx context.Context, tx pgx.Tx, f RuntimeImageFilter) ([]RuntimeImageRow, error) {
	q := `SELECT ` + runtimeImageCols + ` FROM runtime_images WHERE tenant_id = $1`
	args := []any{f.TenantID}
	n := 2
	if f.Status != "" {
		q += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, f.Status)
		n++
	}
	if f.Source != "" {
		q += fmt.Sprintf(" AND source = $%d", n)
		args = append(args, f.Source)
		n++
	}
	if f.Search != "" {
		q += fmt.Sprintf(" AND (name ILIKE $%d OR slug ILIKE $%d)", n, n)
		args = append(args, "%"+f.Search+"%")
	}
	q += " ORDER BY created_at DESC"
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuntimeImageRow
	for rows.Next() {
		r, err := scanRuntimeImage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRuntimeImage applies a partial update with optimistic concurrency.
// Field pointers match UpdateRuntimeImageFields; nil = unchanged.
//
// StatusOnly marks a build-flow transition (draft→building→ready/failed)
// that must NOT bump `version`: the version is the "spec changed" signal
// (docs/09 §5) and the rebuild gate compares it to `built_version`, so a
// mere status flip must leave it untouched. Spec edits and every other
// caller leave StatusOnly false (version bumps, OCC preserved).
type UpdateRuntimeImageFields struct {
	Name               *string
	Slug               *string
	Description        *string
	AptPackages        *[]byte
	Toolchains         *[]byte
	Env                *[]byte
	DockerfileOverride *string
	Tag                *string
	Status             *string
	BuildLog           *string
	Error              *string
	BuiltVersion       *int   // set on build success to the spec version the image was built from
	StatusOnly         bool   // true = build-flow transition; do not bump version
}

// UpdateRuntimeImage updates mutable fields with optimistic concurrency.
func UpdateRuntimeImage(ctx context.Context, tx pgx.Tx, tenantID, id string, version int, f UpdateRuntimeImageFields) (RuntimeImageRow, error) {
	var sets []string
	var args []any
	n := 1
	add := func(col string, v any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, n))
		args = append(args, v)
		n++
	}
	if f.Name != nil {
		add("name", *f.Name)
	}
	if f.Slug != nil {
		add("slug", *f.Slug)
	}
	if f.Description != nil {
		add("description", *f.Description)
	}
	if f.AptPackages != nil {
		add("apt_packages", *f.AptPackages)
	}
	if f.Toolchains != nil {
		add("toolchains", *f.Toolchains)
	}
	if f.Env != nil {
		add("env", *f.Env)
	}
	if f.DockerfileOverride != nil {
		add("dockerfile_override", *f.DockerfileOverride)
	}
	if f.Tag != nil {
		add("tag", *f.Tag)
	}
	if f.Status != nil {
		add("status", *f.Status)
	}
	if f.BuildLog != nil {
		add("build_log", *f.BuildLog)
	}
	if f.Error != nil {
		add("error", *f.Error)
	}
	if f.BuiltVersion != nil {
		add("built_version", *f.BuiltVersion)
	}
	if len(sets) == 0 {
		return GetRuntimeImage(ctx, tx, tenantID, id)
	}
	// The spec version only advances on a real spec edit. Build-flow
	// transitions (StatusOnly) keep it — that is what makes `version` a
	// faithful "did the spec change" signal for the rebuild gate. The OCC
	// WHERE clause still checks the caller-supplied version either way.
	if !f.StatusOnly {
		add("version", version+1)
	}
	add("updated_at", time.Now().UTC())
	q := `UPDATE runtime_images SET ` + strings.Join(sets, ", ") +
		` WHERE id = $` + fmt.Sprintf("%d", n) + ` AND tenant_id = $` + fmt.Sprintf("%d", n+1) +
		` AND version = $` + fmt.Sprintf("%d", n+2) +
		` RETURNING ` + runtimeImageCols
	args = append(args, id, tenantID, version)
	r, err := scanRuntimeImage(tx.QueryRow(ctx, q, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// DeleteRuntimeImage removes the image spec row.
func DeleteRuntimeImage(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM runtime_images WHERE id = $1 AND tenant_id = $2`, id, tenantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReadyRuntimeImageTags returns the tags of all ready custom images for a
// tenant — the list merged with the daemon's stock images for the
// work-item runtime_image dropdown.
func ReadyRuntimeImageTags(ctx context.Context, tx pgx.Tx, tenantID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT tag FROM runtime_images WHERE tenant_id = $1 AND status = 'ready' AND tag <> '' ORDER BY name`,
		tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
