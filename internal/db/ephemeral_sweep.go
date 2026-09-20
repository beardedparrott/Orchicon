package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ephemeral_sweep.go — the abandonment sweep for machine-managed transient
// records (Ask Orchicon Quick Work).
//
// Quick Work's contract is that its worker, workflow and work item are
// HARD-DELETED when the job ends. But the deletion is performed BY THE AGENT,
// and an agent can die: a killed process, a crashed plane, an OOM. When it
// does, its ephemeral records are left behind — invisible to every view and
// still in the table. That is precisely the "invisible record" the hard-delete
// rule exists to prevent, arriving by a different route, and it is the one
// failure mode the protocol itself cannot cover.
//
// So the sweep is the backstop, not the primary path: it deletes ephemeral
// rows that are simply too old to belong to a live job.

// EphemeralSweepResult counts what one sweep removed, per table.
type EphemeralSweepResult struct {
	WorkItems int
	Workers   int
	Workflows int
}

// Total is the number of records removed.
func (r EphemeralSweepResult) Total() int { return r.WorkItems + r.Workers + r.Workflows }

// EphemeralSweepBatch bounds how many records of each kind a single sweep
// removes.
//
// The bound matters because the sweep runs on a timer inside the control plane:
// an unbounded sweep over a table that somehow accumulated transients would
// hold a transaction open for an unbounded time and could stall the plane it is
// supposed to be tidying. The next tick continues the work, so a bound only
// makes the sweep take longer, never less complete.
const EphemeralSweepBatch = 200

// SweepAbandonedEphemeral hard-deletes ephemeral records for ONE tenant that
// were created before cutoff, and reports what it removed.
//
// THE WINDOW IS THE ONLY GUARD, deliberately. Status is not consulted, because
// there is no status that means "abandoned": the owner is gone and the record
// simply stopped being updated, so a stale `running` row and a stale `pending`
// row are the same problem. The window is chosen to be far longer than any real
// Quick Work job (which is a conversational turn, i.e. minutes), so anything
// past it is stale by a wide margin whatever its status says.
//
// DELETE ORDER IS WORKFLOWS, THEN ITEMS, THEN WORKERS, and the order is not
// cosmetic:
//
//   - workflow_runs.work_item_id references work_items. Deleting the workflow
//     first removes the runs (and their step runs) that point at the item, so
//     the item delete does not have to write NULL back into rows that are about
//     to disappear anyway;
//   - workers are independent of both (assigned_worker_ref is JSONB, not a
//     foreign key), so they are last purely because nothing depends on them.
//
// Each kind reuses the EXISTING hard-delete path — HardDeleteWorkItem,
// DeleteWorker, DeleteWorkflow — rather than re-implementing the cascades here.
// Those functions are what the operator's own delete buttons call, so a sweep
// cannot drift into deleting a different set of child rows than a manual delete
// would.
//
// Per-record failures do not abort the sweep: one un-deletable record must not
// block the removal of the others, and the failure is returned in the error so
// the caller logs it. A NotFound (someone else deleted it first) is not an
// error.
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

// ephemeralIDs returns the ids of ephemeral rows in `table` created before
// cutoff, oldest first, bounded by EphemeralSweepBatch.
//
// `table` is a package constant at every call site (never user input), which is
// why it can be interpolated; the tenant and the cutoff remain bound
// parameters. The ix_<table>_ephemeral_created partial indexes
// (created_at WHERE ephemeral) make this proportional to the handful of live
// transients rather than to the table.
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
