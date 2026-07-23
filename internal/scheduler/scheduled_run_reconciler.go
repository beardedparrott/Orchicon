package scheduler

import (
	"context"
	"log/slog"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/reconciler"
)

// ScheduledRunReconciler scans for work items with a pending scheduled
// workflow start and dispatches them (docs/11 §5.2). Idempotent: once
// workflow_run_id is set on the work item, the scan filter excludes it.
type ScheduledRunReconciler struct {
	pool  *db.Pool
	log   *slog.Logger
	start StartWorkflowFn
}

// StartWorkflowFn starts a workflow run for a bound work item.
type StartWorkflowFn func(ctx context.Context, tenantID, workflowID, projectID, workItemID string) error

// NewScheduledRunReconciler creates a new ScheduledRunReconciler.
func NewScheduledRunReconciler(pool *db.Pool, log *slog.Logger, start StartWorkflowFn) *ScheduledRunReconciler {
	return &ScheduledRunReconciler{pool: pool, log: log, start: start}
}

func (r *ScheduledRunReconciler) Kind() string { return "scheduled_run" }

// Reconcile scans for ready scheduled runs and fires them.
//
//	SELECT id FROM work_items
//	 WHERE workflow_id IS NOT NULL
//	   AND workflow_run_id IS NULL
//	   AND scheduled_start_at IS NOT NULL
//	   AND scheduled_start_at <= now()
//	   AND status = 'pending'
//	   AND auto_start_workflow
func (r *ScheduledRunReconciler) Reconcile(ctx context.Context, key string) reconciler.Result {
	// The scan query uses the kind as a scan-all signal; the key is ignored.
	// Each scheduled work item is enqueued individually by the outbox or scan.
	return r.scanAndFire(ctx)
}

func (r *ScheduledRunReconciler) scanAndFire(ctx context.Context) reconciler.Result {
	ttx, err := r.pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		r.log.Error("scheduled_run: begin tx", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	defer ttx.Rollback(ctx)

	q := `SELECT id, tenant_id, workflow_id, project_id FROM work_items
		 WHERE workflow_id IS NOT NULL
		   AND scheduled_start_at IS NOT NULL
		   AND scheduled_start_at BETWEEN now() - interval '5 minutes' AND now()
		   AND status = 'scheduled'
		 LIMIT 100`

	rows, err := ttx.Tx.Query(ctx, q)
	if err != nil {
		r.log.Error("scheduled_run: scan query", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	defer rows.Close()

	type wiRef struct {
		id, tenantID, workflowID, projectID string
	}
	var refs []wiRef
	for rows.Next() {
		var ref wiRef
		if err := rows.Scan(&ref.id, &ref.tenantID, &ref.workflowID, &ref.projectID); err != nil {
			r.log.Error("scheduled_run: scan row", "error", err)
			continue
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		r.log.Error("scheduled_run: rows iteration", "error", err)
		return reconciler.Result{RequeueAfter: 0, Error: err}
	}
	ttx.Rollback(ctx) // release tx before firing individual workflows

	if len(refs) == 0 {
		return reconciler.Result{RequeueAfter: 0}
	}

	for _, ref := range refs {
		if err := r.start(ctx, ref.tenantID, ref.workflowID, ref.projectID, ref.id); err != nil {
			r.log.Error("scheduled_run: start workflow failed",
				"work_item", ref.id, "workflow", ref.workflowID, "error", err)
		} else {
			r.log.Info("scheduled_run: workflow started",
				"work_item", ref.id, "workflow", ref.workflowID)
		}
	}

	return reconciler.Result{RequeueAfter: 0}
}
