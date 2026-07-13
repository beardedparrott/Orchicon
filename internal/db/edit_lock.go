package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// EditLockRow is the data-access shape of an edit_locks table row
// (docs/07 §3.3). Prevents concurrent edits in the visual editor; other
// users see "currently being edited by [user]" and can view read-only.
// The lock expires automatically on TTL.
type EditLockRow struct {
	ResourceID   string
	TenantID     string
	ResourceType string
	HeldBy       string
	AcquiredAt   time.Time
	ExpiresAt    time.Time
}

// DefaultEditLockTTL is the duration after which an edit lock expires
// automatically. The frontend is expected to heartbeat-renew before this
// expires (docs/07 §3.3).
const DefaultEditLockTTL = 5 * time.Minute

// AcquireEditLock attempts to acquire an exclusive edit lock on a
// resource (e.g. a Worker). If an unexpired lock is held by another
// actor, it returns the existing lock with acquired=false (via
// ErrLockHeld). If the existing lock is expired, it is replaced. If the
// same actor already holds the lock, it is renewed (TTL extended).
func AcquireEditLock(ctx context.Context, tx pgx.Tx, tenantID, resourceID, resourceType, actor string, ttl time.Duration) (EditLockRow, bool, error) {
	// Delete expired locks first (housekeeping).
	if _, err := tx.Exec(ctx,
		`DELETE FROM edit_locks WHERE resource_id = $1 AND resource_type = $2 AND expires_at < now()`,
		resourceID, resourceType,
	); err != nil {
		return EditLockRow{}, false, fmt.Errorf("db: cleanup expired edit lock: %w", err)
	}

	// Upsert: if no lock exists (or the same actor holds it), acquire.
	// If a different actor holds an unexpired lock, the conflict on
	// (resource_id, resource_type) means ON CONFLICT fires — we return
	// the existing lock via the DO UPDATE that only succeeds if held_by
	// matches the actor.
	expires := time.Now().UTC().Add(ttl)
	const q = `INSERT INTO edit_locks (resource_id, tenant_id, resource_type, held_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (resource_id, resource_type)
		DO UPDATE SET expires_at = $5, acquired_at = now()
		WHERE edit_locks.held_by = $4
		RETURNING resource_id, tenant_id, resource_type, held_by, acquired_at, expires_at`
	var l EditLockRow
	err := tx.QueryRow(ctx, q, resourceID, tenantID, resourceType, actor, expires).Scan(
		&l.ResourceID, &l.TenantID, &l.ResourceType, &l.HeldBy,
		&l.AcquiredAt, &l.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT's WHERE (held_by = actor) did not match — another
		// actor holds the lock. Fetch the existing lock to return it.
		existing, getErr := GetEditLock(ctx, tx, tenantID, resourceID, resourceType)
		if getErr != nil {
			return EditLockRow{}, false, fmt.Errorf("db: acquire edit lock (get existing): %w", getErr)
		}
		return existing, false, nil
	}
	if err != nil {
		return EditLockRow{}, false, fmt.Errorf("db: acquire edit lock: %w", err)
	}
	return l, true, nil
}

// ReleaseEditLock releases a held edit lock. Only the actor that holds
// the lock may release it. Returns ErrNotFound if no lock exists or the
// actor does not match.
func ReleaseEditLock(ctx context.Context, tx pgx.Tx, tenantID, resourceID, resourceType, actor string) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM edit_locks WHERE resource_id = $1 AND resource_type = $2 AND tenant_id = $3 AND held_by = $4`,
		resourceID, resourceType, tenantID, actor,
	)
	if err != nil {
		return fmt.Errorf("db: release edit lock: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetEditLock returns the current edit lock state for a resource, if
// any unexpired lock exists. Expired locks are treated as "no lock."
func GetEditLock(ctx context.Context, tx pgx.Tx, tenantID, resourceID, resourceType string) (EditLockRow, error) {
	const q = `SELECT resource_id, tenant_id, resource_type, held_by, acquired_at, expires_at
		FROM edit_locks
		WHERE resource_id = $1 AND resource_type = $2 AND tenant_id = $3 AND expires_at >= now()`
	var l EditLockRow
	err := tx.QueryRow(ctx, q, resourceID, resourceType, tenantID).Scan(
		&l.ResourceID, &l.TenantID, &l.ResourceType, &l.HeldBy,
		&l.AcquiredAt, &l.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EditLockRow{}, ErrNotFound
	}
	if err != nil {
		return EditLockRow{}, fmt.Errorf("db: get edit lock: %w", err)
	}
	return l, nil
}
