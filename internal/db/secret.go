package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type SecretRow struct {
	ID          string
	TenantID    string
	Name        string
	Description string
	Ciphertext  string
	KeyVersion  int16
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const secretCols = `id, tenant_id, name, description, ciphertext, key_version, created_at, updated_at`

func scanSecret(row pgx.Row, dst *SecretRow) error {
	return row.Scan(&dst.ID, &dst.TenantID, &dst.Name, &dst.Description, &dst.Ciphertext, &dst.KeyVersion, &dst.CreatedAt, &dst.UpdatedAt)
}

func CreateSecret(ctx context.Context, tx pgx.Tx, r SecretRow) (SecretRow, error) {
	const q = `INSERT INTO tenant_secrets (id, tenant_id, name, description, ciphertext, key_version) VALUES ($1,$2,$3,$4,$5,$6) RETURNING ` + secretCols
	var out SecretRow
	err := tx.QueryRow(ctx, q, r.ID, r.TenantID, r.Name, r.Description, r.Ciphertext, r.KeyVersion).Scan(&out.ID, &out.TenantID, &out.Name, &out.Description, &out.Ciphertext, &out.KeyVersion, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return SecretRow{}, fmt.Errorf("db: create secret: %w", err)
	}
	return out, nil
}

func GetSecret(ctx context.Context, tx pgx.Tx, tenantID, id string) (SecretRow, error) {
	const q = `SELECT ` + secretCols + ` FROM tenant_secrets WHERE tenant_id=$1 AND id=$2`
	var r SecretRow
	err := tx.QueryRow(ctx, q, tenantID, id).Scan(&r.ID, &r.TenantID, &r.Name, &r.Description, &r.Ciphertext, &r.KeyVersion, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretRow{}, ErrNotFound
	}
	if err != nil {
		return SecretRow{}, fmt.Errorf("db: get secret: %w", err)
	}
	return r, nil
}

func ListSecrets(ctx context.Context, tx pgx.Tx, tenantID string, search string, limit int, afterID string) ([]SecretRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT ` + secretCols + ` FROM tenant_secrets WHERE tenant_id=$1 AND ($2='' OR id > $2)`
	args := []any{tenantID, afterID}
	if search != "" {
		q += fmt.Sprintf(` AND (name ILIKE $%d OR description ILIKE $%d)`, len(args)+1, len(args)+1)
		args = append(args, "%"+search+"%")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list secrets: %w", err)
	}
	defer rows.Close()
	var out []SecretRow
	for rows.Next() {
		var r SecretRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Description, &r.Ciphertext, &r.KeyVersion, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan secret: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func UpdateSecret(ctx context.Context, tx pgx.Tx, tenantID, id string, description *string, ciphertext *string, keyVersion *int16) (SecretRow, error) {
	// build dynamic update
	q := `UPDATE tenant_secrets SET updated_at=now()`
	args := []any{tenantID, id}
	idx := len(args) + 1
	if description != nil {
		q += fmt.Sprintf(`, description=$%d`, idx)
		args = append(args, *description)
		idx++
	}
	if ciphertext != nil {
		q += fmt.Sprintf(`, ciphertext=$%d`, idx)
		args = append(args, *ciphertext)
		idx++
		if keyVersion != nil {
			q += fmt.Sprintf(`, key_version=$%d`, idx)
			args = append(args, *keyVersion)
			idx++
		}
	}
	q += ` WHERE tenant_id=$1 AND id=$2 RETURNING ` + secretCols
	var r SecretRow
	err := tx.QueryRow(ctx, q, args...).Scan(&r.ID, &r.TenantID, &r.Name, &r.Description, &r.Ciphertext, &r.KeyVersion, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretRow{}, ErrNotFound
	}
	if err != nil {
		return SecretRow{}, fmt.Errorf("db: update secret: %w", err)
	}
	return r, nil
}

func DeleteSecret(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM tenant_secrets WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return fmt.Errorf("db: delete secret: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func BatchGetSecrets(ctx context.Context, tx pgx.Tx, tenantID string, ids []string) (map[string]SecretRow, error) {
	if len(ids) == 0 {
		return map[string]SecretRow{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+secretCols+` FROM tenant_secrets WHERE tenant_id=$1 AND id = ANY($2)`, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("db: batch get secrets: %w", err)
	}
	defer rows.Close()
	m := map[string]SecretRow{}
	for rows.Next() {
		var r SecretRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Name, &r.Description, &r.Ciphertext, &r.KeyVersion, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan batch secret: %w", err)
		}
		m[r.ID] = r
	}
	return m, rows.Err()
}
