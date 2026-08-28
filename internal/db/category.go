package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var validTargetTypes = map[string]bool{"worker": true, "workflow": true, "conversation": true}
var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)

type CategoryRow struct {
	ID          string
	TenantID    string
	TargetType  string
	Name        string
	Description string
	Slug        string
	SortOrder   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type AssignmentRow struct {
	TenantID   string
	TargetType string
	EntityID   string
	CategoryID string
	CreatedAt  time.Time
}

func Slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevUnderscore := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevUnderscore = false
		} else {
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "category"
	}
	if len(out) > 63 {
		out = out[:63]
		out = strings.TrimRight(out, "_")
	}
	return out
}

func ValidateTargetType(t string) error {
	if !validTargetTypes[t] {
		return fmt.Errorf("target_type must be one of worker, workflow, conversation")
	}
	return nil
}

func CreateCategory(ctx context.Context, tx pgx.Tx, tenantID, targetType, name, description string) (CategoryRow, error) {
	if err := ValidateTargetType(targetType); err != nil { return CategoryRow{}, err }
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 64 { return CategoryRow{}, fmt.Errorf("name must be 1-64 chars") }
	if len(description) > 256 { return CategoryRow{}, fmt.Errorf("description must be at most 256 chars") }
	slug := Slugify(name)
	// append short suffix on slug collision is handled by caller via retry; here compute base slug then rely on UNIQUE error
	var maxOrder int
	_ = tx.QueryRow(ctx, `SELECT COALESCE(MAX(sort_order), -1) FROM categories WHERE tenant_id=$1 AND target_type=$2`, tenantID, targetType).Scan(&maxOrder)
	sortOrder := maxOrder + 1
	id := NewID()
	// try insert; on slug collision append suffix
	for attempt := 0; attempt < 3; attempt++ {
		trySlug := slug
		if attempt > 0 {
			trySlug = fmt.Sprintf("%s_%s", slug, strings.ToLower(NewID()[:6]))
		}
		const q = `INSERT INTO categories (id, tenant_id, target_type, name, description, slug, sort_order) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, tenant_id, target_type, name, description, slug, sort_order, created_at, updated_at`
		var r CategoryRow
		err := tx.QueryRow(ctx, q, id, tenantID, targetType, name, description, trySlug, sortOrder).Scan(&r.ID, &r.TenantID, &r.TargetType, &r.Name, &r.Description, &r.Slug, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt)
		if err == nil { return r, nil }
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "slug") {
			continue
		}
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "name") {
			return CategoryRow{}, fmt.Errorf("category name already exists for this target_type")
		}
		return CategoryRow{}, fmt.Errorf("db: create category: %w", err)
	}
	return CategoryRow{}, fmt.Errorf("db: create category: slug collision after retries")
}

func ListCategories(ctx context.Context, tx pgx.Tx, tenantID, targetType string) ([]CategoryRow, error) {
	if err := ValidateTargetType(targetType); err != nil { return nil, err }
	rows, err := tx.Query(ctx, `SELECT id, tenant_id, target_type, name, description, slug, sort_order, created_at, updated_at FROM categories WHERE tenant_id=$1 AND target_type=$2 ORDER BY sort_order ASC, id ASC`, tenantID, targetType)
	if err != nil { return nil, fmt.Errorf("db: list categories: %w", err) }
	defer rows.Close()
	var out []CategoryRow
	for rows.Next() {
		var r CategoryRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.TargetType, &r.Name, &r.Description, &r.Slug, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt); err != nil { return nil, err }
		out = append(out, r)
	}
	return out, rows.Err()
}

func GetCategory(ctx context.Context, tx pgx.Tx, tenantID, id string) (CategoryRow, error) {
	var r CategoryRow
	err := tx.QueryRow(ctx, `SELECT id, tenant_id, target_type, name, description, slug, sort_order, created_at, updated_at FROM categories WHERE tenant_id=$1 AND id=$2`, tenantID, id).Scan(&r.ID, &r.TenantID, &r.TargetType, &r.Name, &r.Description, &r.Slug, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) { return CategoryRow{}, ErrNotFound }
	if err != nil { return CategoryRow{}, fmt.Errorf("db: get category: %w", err) }
	return r, nil
}

func UpdateCategory(ctx context.Context, tx pgx.Tx, tenantID, id string, name *string, description *string) (CategoryRow, error) {
	cat, err := GetCategory(ctx, tx, tenantID, id)
	if err != nil { return CategoryRow{}, err }
	if name != nil {
		n := strings.TrimSpace(*name)
		if len(n) == 0 || len(n) > 64 { return CategoryRow{}, fmt.Errorf("name must be 1-64 chars") }
		if n == "Uncategorized" || strings.EqualFold(n, "uncategorized") { return CategoryRow{}, fmt.Errorf("Uncategorized is reserved") }
		cat.Name = n
		cat.Slug = Slugify(n)
	}
	if description != nil {
		d := strings.TrimSpace(*description)
		if len(d) > 256 { return CategoryRow{}, fmt.Errorf("description must be at most 256 chars") }
		cat.Description = d
	}
	// try update with slug collision handling
	for attempt := 0; attempt < 3; attempt++ {
		trySlug := cat.Slug
		if attempt > 0 {
			trySlug = fmt.Sprintf("%s_%s", cat.Slug, strings.ToLower(NewID()[:6]))
		}
		const q = `UPDATE categories SET name=$3, description=$4, slug=$5, updated_at=now() WHERE tenant_id=$1 AND id=$2 RETURNING id, tenant_id, target_type, name, description, slug, sort_order, created_at, updated_at`
		var r CategoryRow
		err = tx.QueryRow(ctx, q, tenantID, id, cat.Name, cat.Description, trySlug).Scan(&r.ID, &r.TenantID, &r.TargetType, &r.Name, &r.Description, &r.Slug, &r.SortOrder, &r.CreatedAt, &r.UpdatedAt)
		if err == nil { return r, nil }
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "name") { return CategoryRow{}, fmt.Errorf("category name already exists for this target_type") }
		if strings.Contains(err.Error(), "duplicate key") && strings.Contains(err.Error(), "slug") { continue }
		return CategoryRow{}, fmt.Errorf("db: update category: %w", err)
	}
	return CategoryRow{}, fmt.Errorf("db: update category: slug collision")
}

func DeleteCategory(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM categories WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil { return fmt.Errorf("db: delete category: %w", err) }
	if tag.RowsAffected() == 0 { return ErrNotFound }
	return nil
}

func AssignToCategory(ctx context.Context, tx pgx.Tx, tenantID, targetType, entityID, categoryID string) error {
	if err := ValidateTargetType(targetType); err != nil { return err }
	if entityID == "" || categoryID == "" { return fmt.Errorf("entity_id and category_id required") }
	cat, err := GetCategory(ctx, tx, tenantID, categoryID)
	if err != nil { return err }
	if cat.TargetType != targetType { return fmt.Errorf("category target_type mismatch") }
	_, err = tx.Exec(ctx, `INSERT INTO category_assignments (tenant_id, target_type, entity_id, category_id) VALUES ($1,$2,$3,$4) ON CONFLICT (tenant_id, target_type, entity_id) DO UPDATE SET category_id=EXCLUDED.category_id`, tenantID, targetType, entityID, categoryID)
	if err != nil { return fmt.Errorf("db: assign to category: %w", err) }
	return nil
}

func UnassignFromCategory(ctx context.Context, tx pgx.Tx, tenantID, targetType, entityID string) error {
	if err := ValidateTargetType(targetType); err != nil { return err }
	_, err := tx.Exec(ctx, `DELETE FROM category_assignments WHERE tenant_id=$1 AND target_type=$2 AND entity_id=$3`, tenantID, targetType, entityID)
	return err
}

func ListAssignments(ctx context.Context, tx pgx.Tx, tenantID, targetType string) ([]AssignmentRow, error) {
	if err := ValidateTargetType(targetType); err != nil { return nil, err }
	rows, err := tx.Query(ctx, `SELECT tenant_id, target_type, entity_id, category_id, created_at FROM category_assignments WHERE tenant_id=$1 AND target_type=$2`, tenantID, targetType)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []AssignmentRow
	for rows.Next() {
		var r AssignmentRow
		if err := rows.Scan(&r.TenantID, &r.TargetType, &r.EntityID, &r.CategoryID, &r.CreatedAt); err != nil { return nil, err }
		out = append(out, r)
	}
	return out, rows.Err()
}

func ReorderCategories(ctx context.Context, tx pgx.Tx, tenantID, targetType string, orderedIDs []string) error {
	if err := ValidateTargetType(targetType); err != nil { return err }
	// lock rows
	rows, err := tx.Query(ctx, `SELECT id FROM categories WHERE tenant_id=$1 AND target_type=$2 ORDER BY sort_order, id FOR UPDATE`, tenantID, targetType)
	if err != nil { return fmt.Errorf("db: reorder lock: %w", err) }
	var existing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil { rows.Close(); return err }
		existing = append(existing, id)
	}
	rows.Close()
	if rows.Err() != nil { return rows.Err() }
	if len(existing) != len(orderedIDs) { return fmt.Errorf("ordered_ids must be permutation of existing categories") }
	set := make(map[string]bool, len(existing))
	for _, id := range existing { set[id] = true }
	for _, id := range orderedIDs { if !set[id] { return fmt.Errorf("ordered_ids contains unknown category") } }
	for idx, id := range orderedIDs {
		if _, err := tx.Exec(ctx, `UPDATE categories SET sort_order=$3, updated_at=now() WHERE tenant_id=$1 AND id=$2`, tenantID, id, idx); err != nil { return err }
	}
	return nil
}
