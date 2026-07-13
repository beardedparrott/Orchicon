package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdapterRow is the data-access shape of a runtime_adapters table row
// (docs/02 §2.8, docs/04 §2, docs/09 §3.7). A registered adapter
// process offering execution capabilities. tenant_id is the primary
// isolation layer; RLS is the backstop (docs/09 §8.5).
type AdapterRow struct {
	ID                    string
	TenantID              string
	Kind                  string
	Version               string
	Endpoint              string
	Capabilities          []byte // jsonb
	Status                string
	MaxConcurrentExecutions int
	RegisteredAt          time.Time
	LastHeartbeatAt       *time.Time
}

// CreateAdapter inserts a new runtime adapter registration
// (docs/04 §2: register). Returns the row with server-generated
// timestamps.
func CreateAdapter(ctx context.Context, tx pgx.Tx, a AdapterRow) (AdapterRow, error) {
	const q = `INSERT INTO runtime_adapters
		(id, tenant_id, kind, version, endpoint, capabilities, status, max_concurrent_executions)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, tenant_id, kind, version, endpoint, capabilities, status,
			max_concurrent_executions, registered_at, last_heartbeat_at`
	row := a
	err := tx.QueryRow(ctx, q,
		a.ID, a.TenantID, a.Kind, a.Version, a.Endpoint, a.Capabilities,
		a.Status, a.MaxConcurrentExecutions,
	).Scan(
		&row.ID, &row.TenantID, &row.Kind, &row.Version, &row.Endpoint,
		&row.Capabilities, &row.Status, &row.MaxConcurrentExecutions,
		&row.RegisteredAt, &row.LastHeartbeatAt,
	)
	if err != nil {
		return AdapterRow{}, fmt.Errorf("db: create adapter: %w", err)
	}
	return row, nil
}

// GetAdapter fetches a single adapter by id within the tenant scope.
func GetAdapter(ctx context.Context, tx pgx.Tx, tenantID, id string) (AdapterRow, error) {
	const q = `SELECT id, tenant_id, kind, version, endpoint, capabilities, status,
		max_concurrent_executions, registered_at, last_heartbeat_at
		FROM runtime_adapters WHERE id = $1 AND tenant_id = $2`
	var a AdapterRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&a.ID, &a.TenantID, &a.Kind, &a.Version, &a.Endpoint,
		&a.Capabilities, &a.Status, &a.MaxConcurrentExecutions,
		&a.RegisteredAt, &a.LastHeartbeatAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdapterRow{}, ErrNotFound
	}
	if err != nil {
		return AdapterRow{}, fmt.Errorf("db: get adapter: %w", err)
	}
	return a, nil
}

// ListAdaptersFilter scopes a list query to a tenant, optionally by kind.
type ListAdaptersFilter struct {
	TenantID  string
	Kind      string
	Status    string
	PageSize  int
	AfterID   string
}

// ListAdapters returns a page of registered adapters for the tenant.
func ListAdapters(ctx context.Context, tx pgx.Tx, f ListAdaptersFilter) ([]AdapterRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, kind, version, endpoint, capabilities, status,
		max_concurrent_executions, registered_at, last_heartbeat_at
		FROM runtime_adapters
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)`
	args := []any{f.TenantID, f.AfterID}
	if f.Kind != "" {
		q += fmt.Sprintf(` AND kind = $%d`, len(args)+1)
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	q += ` ORDER BY id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list adapters: %w", err)
	}
	defer rows.Close()
	var out []AdapterRow
	for rows.Next() {
		var a AdapterRow
		if err := rows.Scan(
			&a.ID, &a.TenantID, &a.Kind, &a.Version, &a.Endpoint,
			&a.Capabilities, &a.Status, &a.MaxConcurrentExecutions,
			&a.RegisteredAt, &a.LastHeartbeatAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan adapter: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HeartbeatAdapter updates the adapter's last_heartbeat_at and health
// snapshot. Called by the adapter lease renewal path (docs/04 §2).
func HeartbeatAdapter(ctx context.Context, tx pgx.Tx, tenantID, id string, health []byte) error {
	const q = `UPDATE runtime_adapters
		SET last_heartbeat_at = now(), capabilities = COALESCE($3, capabilities)
		WHERE tenant_id = $1 AND id = $2`
	tag, err := tx.Exec(ctx, q, tenantID, id, nullableJSON(health))
	if err != nil {
		return fmt.Errorf("db: heartbeat adapter: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateAdapterStatus transitions an adapter's status (e.g.
// registered→ready→draining→expired). Uses optimistic concurrency.
func UpdateAdapterStatus(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, status string) (AdapterRow, error) {
	const q = `UPDATE runtime_adapters
		SET status = $4
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, kind, version, endpoint, capabilities, status,
			max_concurrent_executions, registered_at, last_heartbeat_at`
	var a AdapterRow
	err := tx.QueryRow(ctx, q, tenantID, id, expectedVersion, status).Scan(
		&a.ID, &a.TenantID, &a.Kind, &a.Version, &a.Endpoint,
		&a.Capabilities, &a.Status, &a.MaxConcurrentExecutions,
		&a.RegisteredAt, &a.LastHeartbeatAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdapterRow{}, ErrNotFound
	}
	if err != nil {
		return AdapterRow{}, fmt.Errorf("db: update adapter status: %w", err)
	}
	return a, nil
}

// ListReadyAdaptersByKind returns adapters matching a kind that are in
// ready/registered state with a recent heartbeat (within ttl). Used by
// the TaskReconciler for adapter selection (docs/03 §4.2).
func ListReadyAdaptersByKind(ctx context.Context, tx pgx.Tx, tenantID, kind string, heartbeatTTL time.Duration) ([]AdapterRow, error) {
	// Multiply the numeric seconds by a 1-second interval so pgx only has
	// to encode a float64 (not an interval) — pgx v5 cannot encode a Go
	// float64 directly into the interval OID.
	const q = `SELECT id, tenant_id, kind, version, endpoint, capabilities, status,
		max_concurrent_executions, registered_at, last_heartbeat_at
		FROM runtime_adapters
		WHERE tenant_id = $1 AND kind = $2
		  AND status IN ('registered', 'ready')
		  AND (last_heartbeat_at IS NULL OR last_heartbeat_at >= now() - ($3 * interval '1 second'))
		ORDER BY last_heartbeat_at DESC NULLS LAST`
	rows, err := tx.Query(ctx, q, tenantID, kind, heartbeatTTL.Seconds())
	if err != nil {
		return nil, fmt.Errorf("db: list ready adapters: %w", err)
	}
	defer rows.Close()
	var out []AdapterRow
	for rows.Next() {
		var a AdapterRow
		if err := rows.Scan(
			&a.ID, &a.TenantID, &a.Kind, &a.Version, &a.Endpoint,
			&a.Capabilities, &a.Status, &a.MaxConcurrentExecutions,
			&a.RegisteredAt, &a.LastHeartbeatAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan adapter: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountActiveExecutionsForAdapter returns the number of in-flight
// (non-terminal) executions for an adapter. Used for capacity checks
// (docs/03 §4.2: prefer adapters with free capacity).
func CountActiveExecutionsForAdapter(ctx context.Context, tx pgx.Tx, tenantID, adapterID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM worker_executions
		WHERE tenant_id = $1 AND adapter_id = $2
		  AND status NOT IN ('terminated', 'failed_to_start')`,
		tenantID, adapterID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count active executions: %w", err)
	}
	return count, nil
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
