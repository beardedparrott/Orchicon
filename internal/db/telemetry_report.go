package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DurationStats is a per-group duration + failure aggregate over the Orchicon domain model
// (worker_executions / workflow_runs).
type DurationStats struct {
	GroupKey string
	// GroupName is populated only by the workflow query (workflows.name); the project query leaves it
	// empty and the service layer resolves a project display name.
	GroupName       string
	Executions      int32
	Failed          int32
	DurationMsTotal int64
}

// GetProjectNames returns project id → name for the tenant, so a report row
// can carry a display name without the frontend having to join.
func GetProjectNames(ctx context.Context, tx pgx.Tx, tenantID string) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT id, name FROM projects WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: project names: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("db: scan project name: %w", err)
		}
		out[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: project names: %w", err)
	}
	return out, nil
}

// GetProjectDurationStats aggregates worker_executions per project over an optional window.
func GetProjectDurationStats(ctx context.Context, tx pgx.Tx, tenantID, projectID, executionID, workItemID string, start, end time.Time) ([]DurationStats, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("db: project duration stats: tenant_id required")
	}
	q := `SELECT we.project_id,
		COUNT(*),
		COUNT(*) FILTER (WHERE we.ended_at IS NOT NULL AND we.status NOT IN ` + activeExecutionStatuses + ` AND we.status <> 'succeeded'),
		COALESCE(SUM(EXTRACT(EPOCH FROM (we.ended_at - we.started_at)) * 1000), 0)::bigint
		FROM worker_executions we
		WHERE we.tenant_id = $1
		  AND ($2 = '' OR we.project_id = $2)
		  AND ($3 = '' OR we.id = $3)
		  AND ($4 = '' OR we.task_id = $4)
		  AND we.started_at IS NOT NULL
		  AND ($5::timestamptz <= 'epoch'::timestamptz OR we.started_at >= $5::timestamptz)
		  AND ($6::timestamptz <= 'epoch'::timestamptz OR we.started_at <  $6::timestamptz)
		GROUP BY we.project_id`
	rows, err := tx.Query(ctx, q, tenantID, projectID, executionID, workItemID, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: project duration stats: %w", err)
	}
	defer rows.Close()
	var out []DurationStats
	for rows.Next() {
		var r DurationStats
		if err := rows.Scan(&r.GroupKey, &r.Executions, &r.Failed, &r.DurationMsTotal); err != nil {
			return nil, fmt.Errorf("db: scan project duration stats: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowDurationStats aggregates workflow_runs per workflow over an optional window.
func GetWorkflowDurationStats(ctx context.Context, tx pgx.Tx, tenantID, projectID, workItemID, executionID string, start, end time.Time) ([]DurationStats, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("db: workflow duration stats: tenant_id required")
	}
	const q = `SELECT w.id,
		w.name,
		COUNT(*),
		COUNT(*) FILTER (WHERE wr.status NOT IN ('pending','running') AND wr.status <> 'completed'),
		COALESCE(SUM(EXTRACT(EPOCH FROM (wr.ended_at - wr.started_at)) * 1000), 0)::bigint
		FROM workflow_runs wr
		JOIN workflows w ON w.id = wr.workflow_id
		WHERE wr.tenant_id = $1
		  AND ($2 = '' OR wr.project_id = $2)
		  AND ($5 = '' OR wr.work_item_id = $5)
		  AND ($6 = '' OR EXISTS (SELECT 1 FROM worker_executions we
			WHERE we.workflow_run_id = wr.id AND we.id = $6))
		  AND wr.started_at IS NOT NULL
		  AND ($3::timestamptz <= 'epoch'::timestamptz OR wr.started_at >= $3::timestamptz)
		  AND ($4::timestamptz <= 'epoch'::timestamptz OR wr.started_at <  $4::timestamptz)
		GROUP BY w.id, w.name`
	rows, err := tx.Query(ctx, q, tenantID, projectID, start, end, workItemID, executionID)
	if err != nil {
		return nil, fmt.Errorf("db: workflow duration stats: %w", err)
	}
	defer rows.Close()
	var out []DurationStats
	for rows.Next() {
		var r DurationStats
		if err := rows.Scan(&r.GroupKey, &r.GroupName, &r.Executions, &r.Failed, &r.DurationMsTotal); err != nil {
			return nil, fmt.Errorf("db: scan workflow duration stats: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
