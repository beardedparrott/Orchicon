package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- local_credentials (identity-provider boundary only) -------------------
//
// Local-account password hashes for the embedded OpenID Provider. This is
// the ONLY table holding human password material in the plane, and it is
// deliberately walled off: the accessors here are consumed exclusively by
// the identity-provider boundary (internal/auth) — never by a control-plane
// service, RPC, or Ask Orchicon tool (AGENTS.md password standard amended by
// the local-credentials design: human passwords live only inside the IdP
// boundary, and only as one-way hashes).

// LocalCredentialRow is the data-access shape of a local_credentials row.
type LocalCredentialRow struct {
	ID           string
	TenantID     string
	IdentityID   string
	Username     string
	PasswordHash string
	Status       string
	// ForcePasswordChange marks a credential that must have its password changed before the operator
	// may proceed (the bootstrap admin seeded with the built-in default).
	ForcePasswordChange bool
	Version             int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const localCredentialCols = `id, tenant_id, identity_id, username, password_hash, status,
	force_password_change, version, created_at, updated_at`

func scanLocalCredential(row pgx.Row) (LocalCredentialRow, error) {
	var r LocalCredentialRow
	err := row.Scan(&r.ID, &r.TenantID, &r.IdentityID, &r.Username, &r.PasswordHash,
		&r.Status, &r.ForcePasswordChange, &r.Version, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LocalCredentialRow{}, ErrNotFound
	}
	if err != nil {
		return LocalCredentialRow{}, fmt.Errorf("db: scan local credential: %w", err)
	}
	return r, nil
}

// UpsertLocalCredential inserts or replaces the local credential bound to an identity within a
// tenant.
func UpsertLocalCredential(ctx context.Context, tx pgx.Tx, tenantID, identityID, username, passwordHash string, forcePasswordChange bool) (LocalCredentialRow, error) {
	const q = `INSERT INTO local_credentials (id, tenant_id, identity_id, username, password_hash, status, force_password_change)
		VALUES ($1, $2, $3, $4, $5, 'active', $6)
		ON CONFLICT (tenant_id, identity_id)
		DO UPDATE SET username = EXCLUDED.username,
			password_hash = EXCLUDED.password_hash,
			status = 'active',
			force_password_change = EXCLUDED.force_password_change,
			version = local_credentials.version + 1,
			updated_at = now()
		RETURNING ` + localCredentialCols
	return scanLocalCredential(tx.QueryRow(ctx, q, NewID(), tenantID, identityID, username, passwordHash, forcePasswordChange))
}

// GetLocalCredentialByUsername finds the active local credential for a username within a tenant.
func GetLocalCredentialByUsername(ctx context.Context, tx pgx.Tx, tenantID, username string) (LocalCredentialRow, error) {
	const q = `SELECT ` + localCredentialCols + `
		FROM local_credentials WHERE tenant_id = $1 AND username = $2`
	return scanLocalCredential(tx.QueryRow(ctx, q, tenantID, username))
}

// GetLocalCredentialByIdentity finds the local credential bound to an
// identity within a tenant.
func GetLocalCredentialByIdentity(ctx context.Context, tx pgx.Tx, tenantID, identityID string) (LocalCredentialRow, error) {
	const q = `SELECT ` + localCredentialCols + `
		FROM local_credentials WHERE tenant_id = $1 AND identity_id = $2`
	return scanLocalCredential(tx.QueryRow(ctx, q, tenantID, identityID))
}

// DeleteLocalCredential removes the local credential for an identity within a tenant (the identity
// row itself is untouched — disabling an account without deleting the identity is a status update.
func DeleteLocalCredential(ctx context.Context, tx pgx.Tx, tenantID, identityID string) error {
	const q = `DELETE FROM local_credentials WHERE tenant_id = $1 AND identity_id = $2`
	ct, err := tx.Exec(ctx, q, tenantID, identityID)
	if err != nil {
		return fmt.Errorf("db: delete local credential: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
