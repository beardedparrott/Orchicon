package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecurringRunHistoryRow is the data-access shape of a recurring_run_history
// row — the per-fire ledger for recurring work items (feature 4.2). Status is
// the fire dispatch outcome ('fired' | 'failed'); WorkflowRunID is set after a
// successful dispatch (leaf run / sequence parent run) and NULL when the fire
// failed before a run existed. Started/end + execution ids/outputs are NOT
// duplicated here: they live in the run graph (workflow_runs, worker_executions)
// and are joined at read time (ListRecurringRunHistory).
type RecurringRunHistoryRow struct {
	ID            string
	TenantID      string
	WorkItemID    string
	FireAt        time.Time
	Status        string
	WorkflowRunID *string // NULL when no run was produced (failed-before-run fire)
	Error         string
}

// CreateRecurringRunHistory inserts a per-fire ledger row within the given
// tenant transaction.
func CreateRecurringRunHistory(ctx context.Context, tx pgx.Tx, row RecurringRunHistoryRow) (RecurringRunHistoryRow, error) {
	const q = `INSERT INTO recurring_run_history
		(id, tenant_id, work_item_id, fire_at, status, workflow_run_id, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, tenant_id, work_item_id, fire_at, status, workflow_run_id, error`
	var out RecurringRunHistoryRow
	err := tx.QueryRow(ctx, q,
		row.ID, row.TenantID, row.WorkItemID, row.FireAt, row.Status, row.WorkflowRunID, row.Error,
	).Scan(&out.ID, &out.TenantID, &out.WorkItemID, &out.FireAt, &out.Status, &out.WorkflowRunID, &out.Error)
	if err != nil {
		return RecurringRunHistoryRow{}, fmt.Errorf("db: create recurring run history: %w", err)
	}
	return out, nil
}

// RecurringRunExecution is one worker execution produced by a fire's run.
type RecurringRunExecution struct {
	ID        string
	Status    string
	StepID    string
	StartedAt *time.Time
	EndedAt   *time.Time
	Output    string
}

// RecurringRunHistoryEntry is a fire's ledger row joined to its run graph
// (run status/start/end + the run's worker executions).
type RecurringRunHistoryEntry struct {
	ID            string
	FireAt        time.Time
	Status        string // 'fired' | 'failed'
	WorkflowRunID string // "" when the fire produced no run
	RunStatus     string // bound run's status; "" when no run
	RunStartedAt  *time.Time
	RunEndedAt    *time.Time
	Error         string
	Executions    []RecurringRunExecution
}

// ListRecurringRunHistory returns the per-fire ledger for a recurring work
// item, newest first, each joined to its workflow run (status, started_at,
// ended_at) and that run's worker executions (execution ids, outputs). A fire
// that failed before a run exists yields one entry with WorkflowRunID == ""
// (RunStatus/Executions empty) whose Error carries the dispatch error.
func ListRecurringRunHistory(ctx context.Context, tx pgx.Tx, tenantID, workItemID string) ([]RecurringRunHistoryEntry, error) {
	const q = `SELECT h.id, h.fire_at, h.status, COALESCE(h.workflow_run_id, ''), COALESCE(h.error, ''),
	             COALESCE(r.status, ''), r.started_at, r.ended_at
	       FROM recurring_run_history h
	       LEFT JOIN workflow_runs r ON r.id = h.workflow_run_id
	      WHERE h.tenant_id = $1 AND h.work_item_id = $2
	      ORDER BY h.fire_at DESC`
	rows, err := tx.Query(ctx, q, tenantID, workItemID)
	if err != nil {
		return nil, fmt.Errorf("db: list recurring run history: %w", err)
	}
	defer rows.Close()

	var entries []RecurringRunHistoryEntry
	var runIDs []string
	for rows.Next() {
		var e RecurringRunHistoryEntry
		if err := rows.Scan(&e.ID, &e.FireAt, &e.Status, &e.WorkflowRunID, &e.Error,
			&e.RunStatus, &e.RunStartedAt, &e.RunEndedAt); err != nil {
			return nil, fmt.Errorf("db: scan recurring run history: %w", err)
		}
		entries = append(entries, e)
		if e.WorkflowRunID != "" {
			runIDs = append(runIDs, e.WorkflowRunID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: rows recurring run history: %w", err)
	}
	if len(runIDs) == 0 {
		return entries, nil
	}

	// Gather the executions for the fires' runs in one pass, then key them
	// by run id so each fire's entry carries its run's execution ids + outputs.
	const eq = `SELECT workflow_run_id, id, status, COALESCE(workflow_step_id, ''), started_at, ended_at, COALESCE(output, '')
	             FROM worker_executions
	            WHERE tenant_id = $1 AND workflow_run_id = ANY($2)
	            ORDER BY created_at ASC`
	erows, err := tx.Query(ctx, eq, tenantID, runIDs)
	if err != nil {
		return nil, fmt.Errorf("db: list recurring run executions: %w", err)
	}
	defer erows.Close()

	byRun := make(map[string][]RecurringRunExecution)
	for erows.Next() {
		var runID string
		var x RecurringRunExecution
		if err := erows.Scan(&runID, &x.ID, &x.Status, &x.StepID, &x.StartedAt, &x.EndedAt, &x.Output); err != nil {
			return nil, fmt.Errorf("db: scan recurring run execution: %w", err)
		}
		byRun[runID] = append(byRun[runID], x)
	}
	if err := erows.Err(); err != nil {
		return nil, fmt.Errorf("db: rows recurring run executions: %w", err)
	}
	for i := range entries {
		if entries[i].WorkflowRunID != "" {
			entries[i].Executions = byRun[entries[i].WorkflowRunID]
		}
	}
	return entries, nil
}
