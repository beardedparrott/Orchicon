package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ephemeral_sweep.go — the abandonment sweep for machine-managed transient records (Ask Orchicon
// Quick Work).

// EphemeralSweepResult counts what one sweep removed, per table.
type EphemeralSweepResult struct {
	WorkItems int
	Workers   int
	Workflows int
}

// Total is the number of records removed.
func (r EphemeralSweepResult) Total() int { return r.WorkItems + r.Workers + r.Workflows }

// EphemeralSweepBatch bounds how many records of each kind a single sweep removes.
const EphemeralSweepBatch = 200

// SweepAbandonedEphemeral hard-deletes ephemeral records for ONE tenant that were created before
// cutoff, and reports what it removed. THE WINDOW IS THE ONLY GUARD, deliberately.
func SweepAbandonedEphemeral(ctx context.Context, tx pgx.Tx, tenantID string, cutoff time.Time) (EphemeralSweepResult, error) {
	var res EphemeralSweepResult
	var errs []error

	// --- workflows first: they own the runs that reference the item ---
	wfIDs, err := ephemeralIDs(ctx, tx, "workflows", tenantID, cutoff)
	if err != nil {
		return res, err
	}
	for _, id := range wfIDs {
		if err := DeleteWorkflow(ctx, tx, tenantID, id); err != nil && err != ErrNotFound {
			errs = append(errs, fmt.Errorf("workflow %s: %w", id, err))
			continue
		}
		res.Workflows++
	}

	// --- then the items ---
	wiIDs, err := ephemeralIDs(ctx, tx, "work_items", tenantID, cutoff)
	if err != nil {
		return res, err
	}
	for _, id := range wiIDs {
		if err := HardDeleteWorkItem(ctx, tx, tenantID, id); err != nil && err != ErrNotFound {
			errs = append(errs, fmt.Errorf("work item %s: %w", id, err))
			continue
		}
		res.WorkItems++
	}

	// --- then the workers (nothing depends on them) ---
	wIDs, err := ephemeralIDs(ctx, tx, "workers", tenantID, cutoff)
	if err != nil {
		return res, err
	}
	for _, id := range wIDs {
		if err := DeleteWorker(ctx, tx, tenantID, id); err != nil && err != ErrNotFound {
			errs = append(errs, fmt.Errorf("worker %s: %w", id, err))
			continue
		}
		res.Workers++
	}

	if len(errs) > 0 {
		return res, fmt.Errorf("db: ephemeral sweep: %d record(s) failed: %w", len(errs), errs[0])
	}
	return res, nil
}

// ephemeralIDs returns the ids of ephemeral rows in `table` created before cutoff, oldest first,
// bounded by EphemeralSweepBatch.
func ephemeralIDs(ctx context.Context, tx pgx.Tx, table, tenantID string, cutoff time.Time) ([]string, error) {
	q := fmt.Sprintf(`SELECT id FROM %s
		WHERE tenant_id = $1 AND ephemeral AND created_at < $2
		ORDER BY created_at ASC LIMIT $3`, table)
	rows, err := tx.Query(ctx, q, tenantID, cutoff, EphemeralSweepBatch)
	if err != nil {
		return nil, fmt.Errorf("db: list ephemeral %s: %w", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan ephemeral %s id: %w", table, err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
