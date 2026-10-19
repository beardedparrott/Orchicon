package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutcomeCounts is the high-level "what happened to the work" roll-up shown
// at the top of a report: how much finished, how it ended, and how much is
// still in flight.
//
// The two halves come from different tables with genuinely different status
// vocabularies, and they are deliberately NOT normalised into one another:
//
//   - work_items uses the WorkItemStatus enum (succeeded / failed / cancelled /
//     skipped, plus the non-terminal in-flight states).
//   - workflow_runs uses {'pending','running','completed','failed','aborted'}
//     (internal/db/workflow.go). Its success state is 'completed', NOT
//     'succeeded', and 'aborted' is a terminal outcome of its own — folding it
//     into Failed would report cancelled work as a failure.
//
// Archived work items are excluded, matching every other active read in this
// package: an archived item keeps its terminal status and its archived_at, so
// counting it here would put work in the dashboard that the board, tree and
// list all refuse to show.
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
//
// Window rule, stated once because the two halves differ and a reviewer
// deserves to know why:
//
//   - work_items are scoped on updated_at. Work items have no started_at, and
//     updated_at is exactly when the item reached the status being counted.
//
// work_open deliberately excludes three things that are NOT units of work in
// flight, all verified against a live tnt_dev: status = 'archived' (archiving
// sets the status as well as archived_at, so 337 of them would otherwise land
// in "open"), 'idea' (the Idea Cloud, excluded from the Work Items views), and
// 'recurring' (a schedule waiting for its next fire, not work in progress).
//   - workflow_runs are scoped on COALESCE(started_at, created_at). A run that
//     never started still reached the window (it exists, and it is pending),
//     and a run's outcome is only meaningful once it has run. This is
//     intentionally looser than GetWorkflowDurationStats, which requires
//     started_at because a duration cannot be measured without one.
//
// A zero start is the "unbounded" sentinel, exactly like the other report
// queries (start <= epoch means "no lower bound").
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
