package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExecutionRow is the data-access shape of a worker_executions table row
// (docs/02 §2.7, docs/09 §3.3). A concrete invocation of a Worker
// against a Task on an adapter. Created by the TaskReconciler at
// dispatch; owns the adapter session.
type ExecutionRow struct {
	ID              string
	TenantID        string
	ProjectID       string
	TaskID          string
	WorkerID        string
	WorkerVersion   int
	AdapterID       *string
	Status          string
	HealthState     string
	StartedAt       *time.Time
	EndedAt         *time.Time
	TokenUsage      int64
	CostUSD         float64
	CheckpointRef   *string
	RecoveryID      *string
	WorkflowRunID   string
	WorkflowStepID  string
	Version         int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateExecution inserts a new worker execution row
// (docs/03 §4: createWorkerExecution). The caller controls the
// transaction so the outbox row can be enqueued atomically.
func CreateExecution(ctx context.Context, tx pgx.Tx, e ExecutionRow) (ExecutionRow, error) {
	const q = `INSERT INTO worker_executions
		(id, tenant_id, project_id, task_id, worker_id, worker_version,
		 adapter_id, status, health_state, started_at,
		 workflow_run_id, workflow_step_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, tenant_id, project_id, task_id, worker_id, worker_version,
			adapter_id, status, health_state, started_at, ended_at,
			token_usage, cost_usd, checkpoint_ref, recovery_id,
			workflow_run_id, workflow_step_id, version,
			created_at, updated_at`
	row := e
	err := tx.QueryRow(ctx, q,
		e.ID, e.TenantID, e.ProjectID, e.TaskID, e.WorkerID, e.WorkerVersion,
		e.AdapterID, e.Status, e.HealthState, e.StartedAt,
		e.WorkflowRunID, e.WorkflowStepID,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.TaskID, &row.WorkerID,
		&row.WorkerVersion, &row.AdapterID, &row.Status, &row.HealthState,
		&row.StartedAt, &row.EndedAt, &row.TokenUsage, &row.CostUSD,
		&row.CheckpointRef, &row.RecoveryID,
		&row.WorkflowRunID, &row.WorkflowStepID,
		&row.Version,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return ExecutionRow{}, fmt.Errorf("db: create execution: %w", err)
	}
	return row, nil
}

// GetExecution fetches a single execution by id within the tenant scope.
func GetExecution(ctx context.Context, tx pgx.Tx, tenantID, id string) (ExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, worker_id, worker_version,
		adapter_id, status, health_state, started_at, ended_at,
		token_usage, cost_usd, checkpoint_ref, recovery_id,
		workflow_run_id, workflow_step_id, version,
		created_at, updated_at
		FROM worker_executions WHERE id = $1 AND tenant_id = $2`
	var e ExecutionRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
		&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
		&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
		&e.CheckpointRef, &e.RecoveryID,
		&e.WorkflowRunID, &e.WorkflowStepID,
		&e.Version,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return ExecutionRow{}, fmt.Errorf("db: get execution: %w", err)
	}
	return e, nil
}

// ListExecutionsFilter scopes a list query to a tenant, optionally
// filtered by project/task/status.
type ListExecutionsFilter struct {
	TenantID  string
	ProjectID string
	TaskID    string
	Status    string
	PageSize  int
	AfterID   string
}

// ListExecutions returns a page of executions for the tenant.
func ListExecutions(ctx context.Context, tx pgx.Tx, f ListExecutionsFilter) ([]ExecutionRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, project_id, task_id, worker_id, worker_version,
		adapter_id, status, health_state, started_at, ended_at,
		token_usage, cost_usd, checkpoint_ref, recovery_id,
		workflow_run_id, workflow_step_id, version,
		created_at, updated_at
		FROM worker_executions
		WHERE tenant_id = $1 AND ($2 = '' OR id > $2)`
	args := []any{f.TenantID, f.AfterID}
	if f.ProjectID != "" {
		q += fmt.Sprintf(` AND project_id = $%d`, len(args)+1)
		args = append(args, f.ProjectID)
	}
	if f.TaskID != "" {
		q += fmt.Sprintf(` AND task_id = $%d`, len(args)+1)
		args = append(args, f.TaskID)
	}
	if f.Status != "" {
		q += fmt.Sprintf(` AND status = $%d`, len(args)+1)
		args = append(args, f.Status)
	}
	q += ` ORDER BY id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list executions: %w", err)
	}
	defer rows.Close()
	var out []ExecutionRow
	for rows.Next() {
		var e ExecutionRow
		if err := rows.Scan(
			&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
			&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
			&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
			&e.CheckpointRef, &e.RecoveryID,
			&e.WorkflowRunID, &e.WorkflowStepID,
			&e.Version,
			&e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan execution: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateExecutionFields is a partial update applied with optimistic
// concurrency (docs/09 §5).
type UpdateExecutionFields struct {
	Status        *string
	HealthState   *string
	AdapterID     *string
	StartedAt     *time.Time
	EndedAt       *time.Time
	TokenUsage    *int64
	CostUSD       *float64
	CheckpointRef *string
	RecoveryID    *string
}

// UpdateExecution applies a partial update with optimistic concurrency.
// The tenant_id is injected into the WHERE clause. Returns ErrNotFound
// if no row matches the id+tenant+version.
func UpdateExecution(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateExecutionFields) (ExecutionRow, error) {
	q := `UPDATE worker_executions SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.HealthState != nil {
		q += fmt.Sprintf(`, health_state = $%d`, setIdx)
		args = append(args, *f.HealthState)
		setIdx++
	}
	if f.AdapterID != nil {
		q += fmt.Sprintf(`, adapter_id = $%d`, setIdx)
		args = append(args, *f.AdapterID)
		setIdx++
	}
	if f.StartedAt != nil {
		q += fmt.Sprintf(`, started_at = $%d`, setIdx)
		args = append(args, *f.StartedAt)
		setIdx++
	}
	if f.EndedAt != nil {
		q += fmt.Sprintf(`, ended_at = $%d`, setIdx)
		args = append(args, *f.EndedAt)
		setIdx++
	}
	if f.TokenUsage != nil {
		q += fmt.Sprintf(`, token_usage = $%d`, setIdx)
		args = append(args, *f.TokenUsage)
		setIdx++
	}
	if f.CostUSD != nil {
		q += fmt.Sprintf(`, cost_usd = $%d`, setIdx)
		args = append(args, *f.CostUSD)
		setIdx++
	}
	if f.CheckpointRef != nil {
		q += fmt.Sprintf(`, checkpoint_ref = $%d`, setIdx)
		args = append(args, *f.CheckpointRef)
		setIdx++
	}
	if f.RecoveryID != nil {
		q += fmt.Sprintf(`, recovery_id = $%d`, setIdx)
		args = append(args, *f.RecoveryID)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, project_id, task_id, worker_id, worker_version,
		adapter_id, status, health_state, started_at, ended_at,
		token_usage, cost_usd, checkpoint_ref, recovery_id, version,
		created_at, updated_at`
	var e ExecutionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
		&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
		&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
		&e.CheckpointRef, &e.RecoveryID, &e.Version,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return ExecutionRow{}, fmt.Errorf("db: update execution: %w", err)
	}
	return e, nil
}

// ListDispatchingExecutions returns executions in "dispatching" state
// (docs/03 §6). Used by the TaskReconciler to track in-flight dispatches.
func ListDispatchingExecutions(ctx context.Context, tx pgx.Tx, tenantID string) ([]ExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, worker_id, worker_version,
		adapter_id, status, health_state, started_at, ended_at,
		token_usage, cost_usd, checkpoint_ref, recovery_id, version,
		created_at, updated_at
		FROM worker_executions
		WHERE tenant_id = $1 AND status = 'dispatching'
		ORDER BY created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list dispatching executions: %w", err)
	}
	defer rows.Close()
	var out []ExecutionRow
	for rows.Next() {
		var e ExecutionRow
		if err := rows.Scan(
			&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
			&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
			&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
			&e.CheckpointRef, &e.RecoveryID, &e.Version,
			&e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan execution: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListReadyTasks returns work items in "ready" status for a tenant,
// ordered by priority (docs/03 §3: scheduling input). The TaskReconciler
// processes these for dispatch.
func ListReadyTasks(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkItemRow, error) {
	const q = `SELECT id, tenant_id, project_id, parent_id, kind, title, description,
		acceptance_criteria, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, results, prompt_context, version, created_at, updated_at
		FROM work_items
		WHERE tenant_id = $1 AND status = 'ready'
		ORDER BY priority DESC, created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list ready tasks: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
			&w.Description, &w.AcceptanceCriteria, &w.Status, &w.AssignedWorkerRef,
			&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
			&w.Priority, &w.Budgets, &w.ContextWindow, &w.Results,
			&w.PromptContext, &w.Version, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan work item: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CheckDependenciesSatisfied returns true if all dependency edges pointing
// TO the given work item have their source (from_id) in a terminal-success
// state (succeeded). A task is only dispatched when its dependencies are
// satisfied (docs/02 §4 invariant #1, docs/03 §4).
func CheckDependenciesSatisfied(ctx context.Context, tx pgx.Tx, tenantID, workItemID string) (bool, error) {
	// A work item is ready to dispatch if:
	// 1. It has no blocking dependencies (no from_id edges where type in blocks/depends_on), OR
	// 2. All blocking dependencies point to items in succeeded state.
	const q = `WITH blocking_deps AS (
		SELECT from_id FROM work_item_dependencies
		WHERE tenant_id = $1 AND to_id = $2
		  AND type IN ('blocks', 'depends_on')
	)
	SELECT NOT EXISTS(SELECT 1 FROM blocking_deps)
		OR NOT EXISTS(
			SELECT 1 FROM blocking_deps bd
			JOIN work_items wi ON wi.id = bd.from_id
			WHERE wi.status != 'succeeded'
		)`
	var satisfied bool
	err := tx.QueryRow(ctx, q, tenantID, workItemID).Scan(&satisfied)
	if err != nil {
		return false, fmt.Errorf("db: check dependencies satisfied: %w", err)
	}
	return satisfied, nil
}
