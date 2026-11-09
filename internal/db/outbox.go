package db

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"
)

// OutboxRow is the in-memory representation of an outbox row to be persisted within the same
// transaction as a state mutation (docs/09_Database_Schema.md §6).
type OutboxRow struct {
	ID            string // ULID, also used as the NATS MsgId for dedup
	TenantID      string // scopes the row via RLS
	EventType     string // e.g. "project.created"
	AggregateType string // e.g. "project"
	AggregateID   string // the entity ULID
	AggregateVer  int    // entity version after the mutation
	Payload       []byte // JSON-encoded event envelope
	OccurredAt    time.Time
	TraceID       string
	CorrelationID string
}

// EnqueueOutbox inserts an outbox row within the given transaction.
func EnqueueOutbox(ctx context.Context, tx pgx.Tx, row OutboxRow) error {
	if row.ID == "" {
		row.ID = newULID()
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

// PollOutbox returns up to limit unpublished outbox rows ordered by occurrence time.
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

// MarkPublished sets published_at for the given outbox row IDs.
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

// CountUnpublished returns the number of outbox rows with published_at IS NULL.
func (p *Pool) CountUnpublished(ctx context.Context) (int64, error) {
	var count int64
	err := p.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count unpublished: %w", err)
	}
	return count, nil
}

// Rows with published_at IS NULL are NEVER touched: an unpublished row is an event that has not yet
// been delivered, and pruning it would silently drop a delivery guarantee.
func (p *Pool) PrunePublishedOutbox(ctx context.Context, olderThan time.Time, batchLimit int) (int64, error) {
	if batchLimit <= 0 {
		batchLimit = 10000
	}
	if batchLimit > 100000 {
		batchLimit = 100000
	}
	const q = `DELETE FROM outbox WHERE id IN (
		SELECT id FROM outbox
		WHERE published_at IS NOT NULL AND published_at < $1
		ORDER BY published_at
		LIMIT $2)`
	tag, err := p.Exec(ctx, q, olderThan, batchLimit)
	if err != nil {
		return 0, fmt.Errorf("db: prune published outbox: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountAndOldestUnpublished returns the number of unpublished outbox rows and the occurred_at of
// the oldest one (nil when there are none).
func (p *Pool) CountAndOldestUnpublished(ctx context.Context) (int64, *time.Time, error) {
	var count int64
	var oldest *time.Time
	err := p.QueryRow(ctx,
		`SELECT count(*), min(occurred_at) FROM outbox WHERE published_at IS NULL`,
	).Scan(&count, &oldest)
	if err != nil {
		return 0, nil, fmt.Errorf("db: count and oldest unpublished: %w", err)
	}
	return count, oldest, nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// entropy is a process-local ULID entropy source. crypto/rand provides
// the 124-bit uniqueness required for ULID's monotonicity guarantees.
var (
	// ulidMu serializes ULID generation.
	ulidMu  sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// newULID generates a monotonic ULID under a mutex.
func newULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// NewID generates a ULID suitable for use as a primary key.
func NewID() string {
	return newULID()
}
