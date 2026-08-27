package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/reconciler"
	"github.com/beardedparrott/orchicon/internal/workitem"
)

// RecurringFireReconciler scans for work items with status 'recurring' and
// a past-due next_run_at, fires the bound workflow (or sequence for parents),
// and advances next_run_at to the next occurrence. Idempotent: each item is
// only fired once per due window via optimistic version locking.
//
// The scan mirrors scheduled_run_reconciler.go:41-76 but targets the
// recurring status + next_run_at cursor instead of scheduled status +
// scheduled_start_at.
type RecurringFireReconciler struct {
	pool     *db.Pool
	log      *slog.Logger
	start    StartWorkflowFn
	sequence StartSequenceFn // optional: fires sequence parents with children
}

// NewRecurringFireReconciler creates a new RecurringFireReconciler.
func NewRecurringFireReconciler(pool *db.Pool, log *slog.Logger, start StartWorkflowFn) *RecurringFireReconciler {
	return &RecurringFireReconciler{pool: pool, log: log, start: start}
}

// SetSequenceStarter injects the sequence fire path used when a recurring
// work item has children and no bound workflow. Optional — without it a
// sequence parent is skipped with a warning.
func (r *RecurringFireReconciler) SetSequenceStarter(fn StartSequenceFn) { r.sequence = fn }

func (r *RecurringFireReconciler) Kind() string { return "recurring_fire" }

// Reconcile scans for due recurring items and fires them.
func (r *RecurringFireReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	return r.scanAndFire(ctx)
}

func (r *RecurringFireReconciler) scanAndFire(ctx context.Context) reconciler.Result {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("recurring_fire: begin tx", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	defer ttx.Rollback(ctx)

	// Scan for recurring items whose next_run_at is within the due window.
	// The 5-minute lookback matches scheduled_run_reconciler and covers
	// reconcile loop jitter. Status = 'recurring' is the idempotency
	// guard: once fired, the item is either running (workflow in flight)
	// or has a next_run_at in the future (advanced after fire).
	q := `SELECT w.id, w.tenant_id, w.workflow_id, w.project_id,
	        w.recurring_schedule, w.version,
	        EXISTS (SELECT 1 FROM work_items c
	                WHERE c.tenant_id = w.tenant_id AND c.parent_id = w.id) AS has_children
	       FROM work_items w
	     WHERE w.status = 'recurring'
	       AND w.recurring_enabled
	       AND w.next_run_at IS NOT NULL
	       AND w.next_run_at BETWEEN now() - interval '5 minutes' AND now()
	     LIMIT 100`

	rows, err := ttx.Tx.Query(ctx, q)
	if err != nil {
		r.log.Error("recurring_fire: scan query", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	defer rows.Close()

	type wiRef struct {
		id, tenantID, projectID string
		workflowID              *string
		recurringSchedule       []byte
		version                 int
		hasChildren             bool
	}
	var refs []wiRef
	for rows.Next() {
		var ref wiRef
		if err := rows.Scan(&ref.id, &ref.tenantID, &ref.workflowID,
			&ref.projectID, &ref.recurringSchedule, &ref.version, &ref.hasChildren); err != nil {
			r.log.Error("recurring_fire: scan row", "error", err)
			continue
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		r.log.Error("recurring_fire: rows iteration", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	ttx.Rollback(ctx) // release tx before firing individual workflows

	if len(refs) == 0 {
		return reconciler.Result{RequeueAfter: 0}
	}

	for _, ref := range refs {
		// Defensive guard: skip items currently in an active run state.
		// Redundant with the status='recurring' scan predicate (recurring
		// items are never running/checkpointing/recovering while due), but
		// protects against races where the status flips between scan and fire.
		if ref.hasChildren {
			if r.sequence == nil {
				r.log.Warn("recurring_fire: sequence parent fired but no sequence starter wired",
					"work_item", ref.id)
				continue
			}
			// Advance next_run_at before firing so idempotency holds even
			// if the sequence start fails or is slow.
			if err := r.advanceNextRunAt(ctx, ref.tenantID, ref.id, ref.version,
				ref.recurringSchedule); err != nil {
				r.log.Error("recurring_fire: advance next_run_at failed",
					"work_item", ref.id, "error", err)
				continue
			}
			if err := r.sequence(ctx, ref.tenantID, ref.id); err != nil {
				r.log.Error("recurring_fire: start sequence failed",
					"work_item", ref.id, "error", err)
				r.recordFire(ctx, ref.tenantID, ref.id, "failed", "", err.Error())
			} else {
				r.log.Info("recurring_fire: sequence started",
					"work_item", ref.id)
				r.recordFire(ctx, ref.tenantID, ref.id, "fired",
					r.fireWorkflowRunID(ctx, ref.tenantID, ref.id), "")
			}
			continue
		}
		if ref.workflowID == nil {
			r.log.Warn("recurring_fire: recurring leaf with no workflow skipped",
				"work_item", ref.id)
			continue
		}
		// Advance next_run_at before firing so idempotency holds even
		// if the workflow start fails or is slow.
		if err := r.advanceNextRunAt(ctx, ref.tenantID, ref.id, ref.version,
			ref.recurringSchedule); err != nil {
			r.log.Error("recurring_fire: advance next_run_at failed",
				"work_item", ref.id, "error", err)
			continue
		}
		if err := r.start(ctx, ref.tenantID, *ref.workflowID, ref.projectID, ref.id); err != nil {
			r.log.Error("recurring_fire: start workflow failed",
				"work_item", ref.id, "workflow", *ref.workflowID, "error", err)
			r.recordFire(ctx, ref.tenantID, ref.id, "failed", "", err.Error())
		} else {
			r.log.Info("recurring_fire: workflow started",
				"work_item", ref.id, "workflow", *ref.workflowID)
			r.recordFire(ctx, ref.tenantID, ref.id, "fired",
				r.fireWorkflowRunID(ctx, ref.tenantID, ref.id), "")
		}
	}

	return reconciler.Result{RequeueAfter: 0}
}

// advanceNextRunAt computes the next occurrence from the recurring_schedule
// JSONB and persists it to next_run_at. Uses optimistic version locking
// (WHERE version = expectedVersion) so concurrent scans for the same item
// see a version mismatch and skip it — this is the idempotency guard.
func (r *RecurringFireReconciler) advanceNextRunAt(ctx context.Context,
	tenantID, itemID string, expectedVersion int, scheduleJSON []byte) error {

	next := computeRecurringNextRunAt(scheduleJSON, time.Now().UTC())
	if next == nil {
		return nil // no schedule / compute failed → leave next_run_at unchanged
	}

	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return err
	}
	defer ttx.Rollback(ctx)

	if _, err := db.UpdateWorkItem(ctx, ttx.Tx, tenantID, itemID, expectedVersion,
		db.UpdateWorkItemFields{NextRunAt: next}); err != nil {
		return err // version mismatch → already fired; harmless
	}
	return ttx.Commit(ctx)
}

// recordFire writes one recurring_run_history ledger row for a fired item in
// its own short transaction (independent of the dispatch tx). Best-effort: on
// a ledger write failure the fire outcome is still consistent (next_run_at was
// already advanced) and the error is logged — it must never wedge the pass.
func (r *RecurringFireReconciler) recordFire(ctx context.Context,
	tenantID, itemID, status, workflowRunID, errMsg string) {

	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		r.log.Error("recurring_fire: record fire: begin tx",
			"work_item", itemID, "error", err)
		return
	}
	defer ttx.Rollback(ctx)

	var runID *string
	if workflowRunID != "" {
		runID = &workflowRunID
	}
	if _, err := db.CreateRecurringRunHistory(ctx, ttx.Tx, db.RecurringRunHistoryRow{
		ID:            db.NewID(),
		TenantID:      tenantID,
		WorkItemID:    itemID,
		FireAt:        time.Now().UTC(),
		Status:        status,
		WorkflowRunID: runID,
		Error:         errMsg,
	}); err != nil {
		r.log.Error("recurring_fire: record fire failed",
			"work_item", itemID, "status", status, "error", err)
		return
	}
	if err := ttx.Commit(ctx); err != nil {
		r.log.Error("recurring_fire: record fire commit",
			"work_item", itemID, "error", err)
	}
}

// fireWorkflowRunID reads back the workflow_run_id the just-started run bound
// to the item (StartWorkflowDirect sets it on the item at dispatch). For a
// sequence parent StartSequence clears the parent's workflow binding (children
// each run their own workflows) so this returns "" — the ledger still records
// the fire fact; the run graph join is per-child.
func (r *RecurringFireReconciler) fireWorkflowRunID(ctx context.Context, tenantID, itemID string) string {
	ttx, err := r.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return ""
	}
	defer ttx.Rollback(ctx)
	w, err := db.GetWorkItem(ctx, ttx.Tx, tenantID, itemID)
	if err != nil {
		return ""
	}
	return w.WorkflowRunID
}

// computeRecurringNextRunAt parses a raw recurring_schedule JSONB value and
// returns the next occurrence at-or-after now. Returns nil when the schedule
// is empty or unparseable (the caller keeps whatever next_run_at it has).
func computeRecurringNextRunAt(scheduleJSON []byte, now time.Time) *time.Time {
	if len(scheduleJSON) == 0 {
		return nil
	}
	var rs apiv1.RecurringSchedule
	if err := json.Unmarshal(scheduleJSON, &rs); err != nil {
		return nil
	}
	return workitem.ComputeNextRunAt(&rs, now)
}

// ensureRecurringNextRun returns a recomputed next_run_at for a recurring
// item whose cursor was cleared mid-run/cycle (nil next_run_at), so the
// RecurringFireReconciler always has a due date to pick the item up again.
// Returns nil when the item is not recurring or the schedule can't be
// re-computed (the caller keeps whatever it has — field-mask semantics).
func ensureRecurringNextRun(scheduleJSON []byte, nextRunAt *time.Time, now time.Time) *time.Time {
	if len(scheduleJSON) == 0 || nextRunAt != nil {
		return nil
	}
	return computeRecurringNextRunAt(scheduleJSON, now)
}
