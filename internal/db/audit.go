package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditEventRow is the data-access shape of an audit_events row
// (docs/09 §3.x, db/migrations/..._audit_events.sql). It mirrors the
// policy_decisions write pattern: the caller owns the transaction, this
// helper inserts, the caller commits — so the audit row exists iff the
// mutation it records committed (transactional-outbox, AGENTS.md
// invariant #3).
type AuditEventRow struct {
	ID              string    // ULID
	TenantID        string    // scopes the row via RLS
	ActorIdentityID string    // nullable (FK identities ON DELETE SET NULL)
	ActorType       string    // "user" | "system"
	AuthMethod      string    // "oidc"|"apikey"|"local"|"signup"|"dev"|"refresh"|"system"; "" legacy-only
	Action          string    // e.g. "work_item.created"
	TargetType      string    // e.g. "work_item"
	TargetID        string    // entity ULID (tenant id for tenant-level ops)
	Before          []byte    // JSON snapshot, "{}" for creates
	After           []byte    // JSON snapshot, "{}" for deletes
	TraceID         string    // OTel trace id, "" when telemetry is off
	OccurredAt      time.Time
}

// CreateAuditEvent inserts an audit row within the given transaction. It
// must be called in the same transaction as the mutation so the audit row
// and the state change commit atomically. The row is tenant-scoped via the
// TenantTx's app.tenant_id session variable and enforced again by RLS.
func CreateAuditEvent(ctx context.Context, tx pgx.Tx, r AuditEventRow) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.ActorType == "" {
		r.ActorType = "system"
	}
	if r.Before == nil {
		r.Before = []byte("{}")
	}
	if r.After == nil {
		r.After = []byte("{}")
	}
	if r.OccurredAt.IsZero() {
		r.OccurredAt = time.Now().UTC()
	}
	const q = `INSERT INTO audit_events
		(id, tenant_id, actor_identity_id, actor_type, auth_method, action,
		 target_type, target_id, before, after, trace_id, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11, $12)`
	if _, err := tx.Exec(ctx, q,
		r.ID, r.TenantID, nullableStr(r.ActorIdentityID), r.ActorType,
		r.AuthMethod, r.Action, r.TargetType, r.TargetID,
		json.RawMessage(r.Before), json.RawMessage(r.After),
		r.TraceID, r.OccurredAt,
	); err != nil {
		return fmt.Errorf("db: create audit event: %w", err)
	}
	return nil
}

// AuditEventScanRow is the read shape of an audit_events row.
type AuditEventScanRow struct {
	ID              string
	TenantID        string
	ActorIdentityID string
	ActorType       string
	AuthMethod      string
	Action          string
	TargetType      string
	TargetID        string
	Before          []byte
	After           []byte
	TraceID         string
	OccurredAt      time.Time
}

// ListAuditEvents returns a page of audit rows for the tenant, newest
// first, keyset-paginated on (occurred_at, id). Every filter is optional;
// the empty-string filter value selects all rows. RLS is the backstop for
// tenant isolation even though the query also scopes on tenant_id.
func (p *Pool) ListAuditEvents(ctx context.Context, tenantID, action, actorID, targetType, targetID, pageToken string, pageSize int) ([]AuditEventScanRow, error) {
	const q = `SELECT id, tenant_id, actor_identity_id, actor_type, auth_method, action,
		target_type, target_id, before, after, trace_id, occurred_at
		FROM audit_events
		WHERE tenant_id = $1
			AND ($2 = '' OR action = $2)
			AND ($3 = '' OR actor_identity_id = $3)
			AND ($4 = '' OR target_type = $4)
			AND ($5 = '' OR target_id = $5)
			AND ($6 = '' OR (occurred_at, id) < (
				SELECT occurred_at, id FROM audit_events
				WHERE tenant_id = $1 AND id = $6))
		ORDER BY occurred_at DESC, id DESC
		LIMIT $7`
	rows, err := p.Query(ctx, q, tenantID, action, actorID, targetType, targetID, pageToken, pageSize)
	if err != nil {
		return nil, fmt.Errorf("db: list audit events: %w", err)
	}
	defer rows.Close()
	var out []AuditEventScanRow
	for rows.Next() {
		var r AuditEventScanRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.ActorIdentityID, &r.ActorType,
			&r.AuthMethod, &r.Action, &r.TargetType, &r.TargetID,
			&r.Before, &r.After, &r.TraceID, &r.OccurredAt); err != nil {
			return nil, fmt.Errorf("db: scan audit event: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
