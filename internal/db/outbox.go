package db

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// OutboxRow is the in-memory representation of an outbox row to be
// persisted within the same transaction as a state mutation
// (docs/09_Database_Schema.md §6). The relay polls unpublished rows and
// publishes them to NATS, then marks published_at.
type OutboxRow struct {
	ID              string    // ULID, also used as the NATS MsgId for dedup
	TenantID        string    // scopes the row via RLS
	EventType       string    // e.g. "project.created"
	AggregateType   string    // e.g. "project"
	AggregateID     string    // the entity ULID
	AggregateVer    int       // entity version after the mutation
	Payload         []byte    // JSON-encoded event envelope
	OccurredAt      time.Time
	TraceID         string
	CorrelationID   string
}

// EnqueueOutbox inserts an outbox row within the given transaction. It
// must be called in the same transaction as the state mutation so the
// outbox entry and the state change commit atomically (AGENTS.md
// invariant #3). The row is tenant-scoped via the TenantTx's
// app.tenant_id session variable and enforced again by RLS.
func EnqueueOutbox(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if row.ID == "" {
		row.ID = ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	}
	if row.OccurredAt.IsZero() {
		row.OccurredAt = time.Now().UTC()
	}
	const q = `INSERT INTO outbox
		(id, tenant_id, event_id, aggregate_type, aggregate_id, aggregate_version,
		 event_type, payload, occurred_at, trace_id, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	if _, err := tx.Exec(ctx, q,
		row.ID, row.TenantID, row.ID, row.AggregateType, row.AggregateID,
		row.AggregateVer, row.EventType, row.Payload, row.OccurredAt,
		nullableStr(row.TraceID), nullableStr(row.CorrelationID),
	); err != nil {
		return fmt.Errorf("db: enqueue outbox: %w", err)
	}
	return nil
}

// UnpublishedRow is a relay-polled outbox row awaiting NATS publication.
type UnpublishedRow struct {
	ID            string
	TenantID      string
	EventID       string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	OccurredAt    time.Time
}

// PollOutbox returns up to limit unpublished outbox rows ordered by
// occurrence time. The relay publishes them to NATS and then calls
// MarkPublished. This read is against the primary (v0.1 has no
// read replicas — docs/09 §2) and is not tenant-scoped at the relay
// level: the relay uses a BYPASSRLS-free role but queries all tenants
// because it publishes on behalf of every tenant. RLS still guards
// writes; the relay only reads and updates published_at.
func (p *Pool) PollOutbox(ctx context.Context, limit int) ([]UnpublishedRow, error) {
	const q = `SELECT id, tenant_id, event_id, aggregate_type, aggregate_id,
		event_type, payload, occurred_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY occurred_at
		LIMIT $1`
	rows, err := p.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("db: poll outbox: %w", err)
	}
	defer rows.Close()
	var out []UnpublishedRow
	for rows.Next() {
		var r UnpublishedRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.EventID, &r.AggregateType,
			&r.AggregateID, &r.EventType, &r.Payload, &r.OccurredAt); err != nil {
			return nil, fmt.Errorf("db: scan outbox: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkPublished sets published_at for the given outbox row IDs. Called by
// the relay after a successful NATS publish. Idempotent: re-marking a
// published row is a no-op.
func (p *Pool) MarkPublished(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`
	if _, err := p.Exec(ctx, q, ids); err != nil {
		return fmt.Errorf("db: mark published: %w", err)
	}
	return nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// entropy is a process-local ULID entropy source. crypto/rand provides
// the 124-bit uniqueness required for ULID's monotonicity guarantees.
var entropy = ulid.Monotonic(rand.Reader, 0)
