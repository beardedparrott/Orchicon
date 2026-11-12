package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutcomeCounts is the high-level "what happened to the work" roll-up shown at the top of a report:
// how much finished, how it ended, and how much is still in flight.
type OutcomeCounts struct {
	WorkSucceeded int32
	WorkSkipped   int32
	WorkCancelled int32
	WorkFailed    int32
	WorkOpen      int32

	WorkflowsCompleted int32
	WorkflowsFailed    int32
	WorkflowsAborted   int32
	WorkflowsRunning   int32
}

// GetOutcomeCounts counts terminal and open work over the report window.
func GetOutcomeCounts(ctx context.Context, tx pgx.Tx, tenantID, projectID, workItemID string, start, end time.Time) (OutcomeCounts, error) {
	if tenantID == "" {
		return OutcomeCounts{}, fmt.Errorf("db: outcome counts: tenant_id required")
	}
	var out OutcomeCounts

	const workSQL = `SELECT
		COUNT(*) FILTER (WHERE status = 'succeeded'),
		COUNT(*) FILTER (WHERE status = 'skipped'),
		COUNT(*) FILTER (WHERE status = 'cancelled'),
		COUNT(*) FILTER (WHERE status = 'failed'),
		-- work_open is a DENYLIST, not an allowlist: terminal states, archived,
		-- the Idea Cloud and recurring templates are excluded, and any other
		-- status counts as in flight. A status added later therefore shows up
		-- in the dashboard as open work (visible, if mislabelled) rather than
		-- vanishing from it (invisible, and silently wrong).
		COUNT(*) FILTER (WHERE status NOT IN
			('succeeded','skipped','cancelled','failed','archived','idea','recurring'))
		FROM work_items
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR id = $3)
		  AND archived_at IS NULL
		  AND ($4::timestamptz <= 'epoch'::timestamptz OR updated_at >= $4::timestamptz)
		  AND ($5::timestamptz <= 'epoch'::timestamptz OR updated_at <  $5::timestamptz)`
	if err := tx.QueryRow(ctx, workSQL, tenantID, projectID, workItemID, start, end).Scan(
		&out.WorkSucceeded, &out.WorkSkipped, &out.WorkCancelled, &out.WorkFailed, &out.WorkOpen,
	); err != nil {
		return OutcomeCounts{}, fmt.Errorf("db: work item outcome counts: %w", err)
	}

	const runSQL = `SELECT
		COUNT(*) FILTER (WHERE status = 'completed'),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COUNT(*) FILTER (WHERE status = 'aborted'),
		COUNT(*) FILTER (WHERE status IN ('pending','running'))
		FROM workflow_runs
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR work_item_id = $3)
		  AND ($4::timestamptz <= 'epoch'::timestamptz OR COALESCE(started_at, created_at) >= $4::timestamptz)
		  AND ($5::timestamptz <= 'epoch'::timestamptz OR COALESCE(started_at, created_at) <  $5::timestamptz)`
	if err := tx.QueryRow(ctx, runSQL, tenantID, projectID, workItemID, start, end).Scan(
		&out.WorkflowsCompleted, &out.WorkflowsFailed, &out.WorkflowsAborted, &out.WorkflowsRunning,
	); err != nil {
		return OutcomeCounts{}, fmt.Errorf("db: workflow run outcome counts: %w", err)
	}

	return out, nil
}
