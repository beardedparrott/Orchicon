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
	ID             string
	TenantID       string
	ProjectID      string
	TaskID         string
	WorkerID       string
	WorkerVersion  int
	AdapterID      *string
	Status         string
	HealthState    string
	StartedAt      *time.Time
	EndedAt        *time.Time
	TokenUsage     int64
	CostUSD        float64
	CheckpointRef  *string
	RecoveryID     *string
	WorktreeStatus *string // worktree provisioning state (pending/ready/skipped/failed/pruned; NULL = no worktree)
	WorktreePath   *string // isolated working tree the execution ran in
	WorktreeBranch *string // deterministic branch created for the run
	PrURL          *string // PR URL for the run's branch, mirrored from the parent workflow run at dispatch
	PrState        *string // PR state (open/merged/draft/none), mirrored from the parent workflow run at dispatch
	WorkflowRunID  string
	WorkflowStepID string
	WorkflowName   string
	WorkerName     string // worker display name (LEFT JOINed from workers at query time)
	TaskName       string // work item title (denormalised via LEFT JOIN at query time)
	ErrorMessage   string
	Output         string
	Conversation   []byte // jsonb: follow-up conversation array
	IsFollowUp     bool
	Iteration      int // loop number: 0 = first dispatch, 1+ = loop_decision re-ask/re-entry
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CreateExecution inserts a new worker execution row
// (docs/03 §4: createWorkerExecution). The caller controls the
// transaction so the outbox row can be enqueued atomically.
func CreateExecution(ctx context.Context, tx pgx.Tx, e ExecutionRow) (ExecutionRow, error) {
	const q = `INSERT INTO worker_executions
		(id, tenant_id, project_id, task_id, worker_id, worker_version,
		 adapter_id, status, health_state, started_at,
		 worktree_status, worktree_path, worktree_branch, pr_url, pr_state,
		 workflow_run_id, workflow_step_id, conversation, is_follow_up, iteration)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
		RETURNING id, tenant_id, project_id, task_id, worker_id, worker_version,
			adapter_id, status, health_state, started_at, ended_at,
			token_usage, cost_usd, checkpoint_ref, recovery_id,
			worktree_status, worktree_path, worktree_branch, pr_url, pr_state,
			workflow_run_id, workflow_step_id, error_message, output, conversation, is_follow_up, iteration, version,
			created_at, updated_at`
	conv := e.Conversation
	if conv == nil {
		conv = []byte("[]")
	}
	row := e
	err := tx.QueryRow(ctx, q,
		e.ID, e.TenantID, e.ProjectID, e.TaskID, e.WorkerID, e.WorkerVersion,
		e.AdapterID, e.Status, e.HealthState, e.StartedAt,
		e.WorktreeStatus, e.WorktreePath, e.WorktreeBranch, e.PrURL, e.PrState,
		e.WorkflowRunID, e.WorkflowStepID, conv, e.IsFollowUp, e.Iteration,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.TaskID, &row.WorkerID,
		&row.WorkerVersion, &row.AdapterID, &row.Status, &row.HealthState,
		&row.StartedAt, &row.EndedAt, &row.TokenUsage, &row.CostUSD,
		&row.CheckpointRef, &row.RecoveryID,
		&row.WorktreeStatus, &row.WorktreePath, &row.WorktreeBranch, &row.PrURL, &row.PrState,
		&row.WorkflowRunID, &row.WorkflowStepID,
		&row.ErrorMessage, &row.Output, &row.Conversation, &row.IsFollowUp, &row.Iteration,
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
	const q = `SELECT we.id, we.tenant_id, we.project_id, we.task_id, we.worker_id, we.worker_version,
		we.adapter_id, we.status, we.health_state, we.started_at, we.ended_at,
		we.token_usage, we.cost_usd, we.checkpoint_ref, we.recovery_id,
		we.worktree_status, we.worktree_path, we.worktree_branch, we.pr_url, we.pr_state,
		we.workflow_run_id, we.workflow_step_id, COALESCE(w.name, '') AS workflow_name, COALESCE(wkr.name, '') AS worker_name, we.error_message, we.output, we.conversation, we.is_follow_up, we.iteration, we.version,
		we.created_at, we.updated_at
		FROM worker_executions we
		LEFT JOIN workflow_runs wr ON wr.id = we.workflow_run_id
		LEFT JOIN workflows w ON w.id = wr.workflow_id
		LEFT JOIN workers wkr ON wkr.id = we.worker_id AND wkr.tenant_id = we.tenant_id
		WHERE we.id = $1 AND we.tenant_id = $2`
	var e ExecutionRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
		&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
		&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
		&e.CheckpointRef, &e.RecoveryID,
		&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
		&e.WorkflowRunID, &e.WorkflowStepID, &e.WorkflowName, &e.WorkerName,
		&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
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
// filtered by project/task/status/workflow_run_id, with free-text
// search and sort.
type ListExecutionsFilter struct {
	TenantID        string
	ProjectID       string
	TaskID          string
	Status          string
	WorkflowRunID   string
	Search          string
	SortBy          string
	SortOrder       string
	PageSize        int
	AfterID         string
	ExcludeFollowUp bool
}

// ListExecutions returns a page of executions for the tenant.
func ListExecutions(ctx context.Context, tx pgx.Tx, f ListExecutionsFilter) ([]ExecutionRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	if f.SortBy == "" {
		f.SortBy = "created_at"
	}
	if f.SortOrder == "" {
		f.SortOrder = "desc"
	}
	q := `SELECT we.id, we.tenant_id, we.project_id, we.task_id, we.worker_id, we.worker_version,
		we.adapter_id, we.status, we.health_state, we.started_at, we.ended_at,
		we.token_usage, we.cost_usd, we.checkpoint_ref, we.recovery_id,
		we.worktree_status, we.worktree_path, we.worktree_branch, we.pr_url, we.pr_state,
		we.workflow_run_id, we.workflow_step_id, COALESCE(w.name, '') AS workflow_name, COALESCE(wkr.name, '') AS worker_name, we.error_message, we.output, we.conversation, we.is_follow_up, we.iteration, we.version,
		we.created_at, we.updated_at
		FROM worker_executions we
		LEFT JOIN workflow_runs wr ON wr.id = we.workflow_run_id
		LEFT JOIN workflows w ON w.id = wr.workflow_id
		LEFT JOIN workers wkr ON wkr.id = we.worker_id AND wkr.tenant_id = we.tenant_id
		WHERE we.tenant_id = $1`
	args := []any{f.TenantID}
	argIdx := 2
	if f.AfterID != "" {
		q += fmt.Sprintf(` AND ($%d = '' OR we.id > $%[1]d)`, argIdx)
		args = append(args, f.AfterID)
		argIdx++
	}
	if f.ProjectID != "" {
		q += fmt.Sprintf(` AND we.project_id = $%d`, argIdx)
		args = append(args, f.ProjectID)
		argIdx++
	}
	if f.TaskID != "" {
		q += fmt.Sprintf(` AND we.task_id = $%d`, argIdx)
		args = append(args, f.TaskID)
		argIdx++
	}
	if f.Status != "" {
		q += fmt.Sprintf(` AND we.status = $%d`, argIdx)
		args = append(args, f.Status)
		argIdx++
	}
	if f.WorkflowRunID != "" {
		q += fmt.Sprintf(` AND we.workflow_run_id = $%d`, argIdx)
		args = append(args, f.WorkflowRunID)
		argIdx++
	}
	if f.Search != "" {
		q += fmt.Sprintf(` AND (we.worker_id ILIKE '%%' || $%d || '%%' OR we.task_id ILIKE '%%' || $%[1]d || '%%' OR COALESCE(w.name, '') ILIKE '%%' || $%[1]d || '%%')`, argIdx)
		args = append(args, f.Search)
		argIdx++
	}
	if f.ExcludeFollowUp {
		q += fmt.Sprintf(` AND we.is_follow_up = $%d`, argIdx)
		args = append(args, false)
		argIdx++
	}
	// Validate sort column to prevent SQL injection.
	sortColumn := "we.created_at"
	switch f.SortBy {
	case "status":
		sortColumn = "we.status"
	case "worker_id":
		sortColumn = "we.worker_id"
	case "created_at":
		sortColumn = "we.created_at"
	}
	sortDir := "DESC"
	if f.SortOrder == "asc" {
		sortDir = "ASC"
	}
	q += fmt.Sprintf(` ORDER BY %s %s`, sortColumn, sortDir)
	q += fmt.Sprintf(` LIMIT $%d`, argIdx)
	args = append(args, f.PageSize)
	argIdx++
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
			&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
			&e.WorkflowRunID, &e.WorkflowStepID, &e.WorkflowName, &e.WorkerName,
			&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
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
	Status         *string
	HealthState    *string
	AdapterID      *string
	StartedAt      *time.Time
	EndedAt        *time.Time
	TokenUsage     *int64
	CostUSD        *float64
	CheckpointRef  *string
	RecoveryID     *string
	WorktreeStatus *string
	WorktreePath   *string
	WorktreeBranch *string
	ErrorMessage   *string
	Output         *string
	Conversation   *[]byte
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
	if f.WorktreeStatus != nil {
		q += fmt.Sprintf(`, worktree_status = $%d`, setIdx)
		args = append(args, *f.WorktreeStatus)
		setIdx++
	}
	if f.WorktreePath != nil {
		q += fmt.Sprintf(`, worktree_path = $%d`, setIdx)
		args = append(args, *f.WorktreePath)
		setIdx++
	}
	if f.WorktreeBranch != nil {
		q += fmt.Sprintf(`, worktree_branch = $%d`, setIdx)
		args = append(args, *f.WorktreeBranch)
		setIdx++
	}
	if f.ErrorMessage != nil {
		q += fmt.Sprintf(`, error_message = $%d`, setIdx)
		args = append(args, *f.ErrorMessage)
		setIdx++
	}
	if f.Output != nil {
		q += fmt.Sprintf(`, output = $%d`, setIdx)
		args = append(args, *f.Output)
		setIdx++
	}
	if f.Conversation != nil {
		q += fmt.Sprintf(`, conversation = $%d`, setIdx)
		args = append(args, *f.Conversation)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, project_id, task_id, worker_id, worker_version,
		adapter_id, status, health_state, started_at, ended_at,
		token_usage, cost_usd, checkpoint_ref, recovery_id,
		worktree_status, worktree_path, worktree_branch, pr_url, pr_state,
		error_message, output, conversation, is_follow_up, iteration, version,
		created_at, updated_at`
	var e ExecutionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
		&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
		&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
		&e.CheckpointRef, &e.RecoveryID,
		&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
		&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
		&e.Version,
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

// activeExecutionStatuses is the SQL form of the non-terminal execution
// set the concurrency guard counts: dispatching / running / healthy /
// stalled / unhealthy / terminating. Any other status (succeeded, failed,
// failed_to_start, terminated) has released its dispatch slot. Every
// per-project dispatch-limit COUNT query MUST use this constant so the
// "active execution" definition stays in one place.
const activeExecutionStatuses = "('dispatching','running','healthy','stalled','unhealthy','terminating')"

// CountActiveExecutionsForProject returns the number of currently active
// (non-terminal) executions for a project. It is the per-project admission
// measure for the max-concurrent-runs dispatch guard: when the count is at
// or above a project's effective limit, the TaskReconciler holds the next
// dispatch until a running execution releases its slot.
func CountActiveExecutionsForProject(ctx context.Context, tx pgx.Tx, tenantID, projectID string) (int, error) {
	var count int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM worker_executions
		WHERE tenant_id = $1 AND project_id = $2
		  AND status IN `+activeExecutionStatuses,
		tenantID, projectID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count active executions for project: %w", err)
	}
	return count, nil
}

// ListDispatchingExecutions returns executions in "dispatching" state
// (docs/03 §6). Used by the TaskReconciler to track in-flight dispatches.
func ListDispatchingExecutions(ctx context.Context, tx pgx.Tx, tenantID string) ([]ExecutionRow, error) {
	const q = `SELECT we.id, we.tenant_id, we.project_id, we.task_id, we.worker_id, we.worker_version,
		we.adapter_id, we.status, we.health_state, we.started_at, we.ended_at,
		we.token_usage, we.cost_usd, we.checkpoint_ref, we.recovery_id,
		we.worktree_status, we.worktree_path, we.worktree_branch, we.pr_url, we.pr_state,
		we.workflow_run_id, we.workflow_step_id, COALESCE(w.name, '') AS workflow_name, COALESCE(wkr.name, '') AS worker_name, we.error_message, we.output, we.conversation, we.is_follow_up, we.iteration, we.version,
		we.created_at, we.updated_at
		FROM worker_executions we
		LEFT JOIN workflow_runs wr ON wr.id = we.workflow_run_id
		LEFT JOIN workflows w ON w.id = wr.workflow_id
		LEFT JOIN workers wkr ON wkr.id = we.worker_id AND wkr.tenant_id = we.tenant_id
		WHERE we.tenant_id = $1 AND we.status = 'dispatching'
		ORDER BY we.created_at ASC`
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
			&e.CheckpointRef, &e.RecoveryID,
			&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
			&e.WorkflowRunID, &e.WorkflowStepID, &e.WorkflowName, &e.WorkerName,
			&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
			&e.Version,
			&e.CreatedAt, &e.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan execution: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListRunningExecutions returns executions in a non-terminal, started
// state (running/healthy/dispatching) — the set the execution-liveness
// reaper checks after a control-plane restart to find executions whose
// process is gone (docs/03 §6 liveness).
func ListRunningExecutions(ctx context.Context, tx pgx.Tx, tenantID string) ([]ExecutionRow, error) {
	const q = `SELECT we.id, we.tenant_id, we.project_id, we.task_id, we.worker_id, we.worker_version,
		we.adapter_id, we.status, we.health_state, we.started_at, we.ended_at,
		we.token_usage, we.cost_usd, we.checkpoint_ref, we.recovery_id,
		we.worktree_status, we.worktree_path, we.worktree_branch, we.pr_url, we.pr_state,
		we.workflow_run_id, we.workflow_step_id, COALESCE(w.name, '') AS workflow_name, COALESCE(wkr.name, '') AS worker_name, we.error_message, we.output, we.conversation, we.is_follow_up, we.iteration, we.version,
		we.created_at, we.updated_at
		FROM worker_executions we
		LEFT JOIN workflow_runs wr ON wr.id = we.workflow_run_id
		LEFT JOIN workflows w ON w.id = wr.workflow_id
		LEFT JOIN workers wkr ON wkr.id = we.worker_id AND wkr.tenant_id = we.tenant_id
		WHERE we.tenant_id = $1 AND we.status IN ('running', 'healthy', 'dispatching')
		ORDER BY we.created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list running executions: %w", err)
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
			&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
			&e.WorkflowRunID, &e.WorkflowStepID, &e.WorkflowName, &e.WorkerName,
			&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
			&e.Version,
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
		acceptance_criteria, acceptance_review, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, sort_order, results, prompt_context, context_files, version, created_at, updated_at
		FROM work_items
		WHERE tenant_id = $1 AND status = 'ready' AND assigned_worker_ref IS NOT NULL
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
			&w.Description, &w.AcceptanceCriteria, &w.AcceptanceReview, &w.Status, &w.AssignedWorkerRef,
			&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
			&w.Priority, &w.Budgets, &w.ContextWindow, &w.SortOrder, &w.Results,
			&w.PromptContext, &w.ContextFiles, &w.Version, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan work item: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteExecution hard-deletes a worker execution row by id.
func DeleteExecution(ctx context.Context, tx pgx.Tx, tenantID, id string) error {
	const q = `DELETE FROM worker_executions WHERE id = $1 AND tenant_id = $2`
	tag, err := tx.Exec(ctx, q, id, tenantID)
	if err != nil {
		return fmt.Errorf("db: delete execution: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListBlockedTasks returns work items in "blocked" status for a tenant,
// ordered by priority — the TaskReconciler scan complement to
// ListReadyTasks. Blocked tasks are re-evaluated on every pass so a newly
// satisfied dependency gate flips them back to ready (and dispatches in
// the same pass) without any new notifier wiring.
func ListBlockedTasks(ctx context.Context, tx pgx.Tx, tenantID string) ([]WorkItemRow, error) {
	const q = `SELECT id, tenant_id, project_id, parent_id, kind, title, description,
		acceptance_criteria, acceptance_review, status, assigned_worker_ref, workflow_id,
		workflow_run_id, workflow_step_id,
		priority, budgets, context_window, sort_order, results, prompt_context, context_files, version, created_at, updated_at
		FROM work_items
		WHERE tenant_id = $1 AND status = 'blocked' AND assigned_worker_ref IS NOT NULL
		ORDER BY priority DESC, created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list blocked tasks: %w", err)
	}
	defer rows.Close()
	var out []WorkItemRow
	for rows.Next() {
		var w WorkItemRow
		if err := rows.Scan(
			&w.ID, &w.TenantID, &w.ProjectID, &w.ParentID, &w.Kind, &w.Title,
			&w.Description, &w.AcceptanceCriteria, &w.AcceptanceReview, &w.Status, &w.AssignedWorkerRef,
			&w.WorkflowID, &w.WorkflowRunID, &w.WorkflowStepID,
			&w.Priority, &w.Budgets, &w.ContextWindow, &w.SortOrder, &w.Results,
			&w.PromptContext, &w.ContextFiles, &w.Version, &w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan blocked work item: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// terminalSuccessStatuses is the SQL form of
// domain.WorkItemIsTerminalSuccess: a work item is terminal-success for
// dependency/arming purposes exactly when its status is one of these.
// Every SQL dependency gate MUST use this constant (and stay in lockstep
// with the Go predicate) so skip-status and depends_on share one
// terminal-success definition. failed/cancelled are NOT listed — they
// keep dependents blocked.
const terminalSuccessStatuses = "('succeeded','skipped')"

// CheckDependenciesSatisfied returns true if all dependency edges pointing
// TO the given work item have their source (from_id) in a terminal-success
// state (succeeded or skipped). A task is only dispatched when its
// dependencies are satisfied (docs/02 §4 invariant #1, docs/03 §4).
func CheckDependenciesSatisfied(ctx context.Context, tx pgx.Tx, tenantID, workItemID string) (bool, error) {
	// A work item is ready to dispatch if:
	// 1. It has no blocking dependencies (no from_id edges where type in blocks/depends_on), OR
	// 2. All blocking dependencies point to items in a terminal-success state.
	const q = `WITH blocking_deps AS (
		SELECT from_id FROM work_item_dependencies
		WHERE tenant_id = $1 AND to_id = $2
		  AND type IN ('blocks', 'depends_on')
	)
	SELECT NOT EXISTS(SELECT 1 FROM blocking_deps)
		OR NOT EXISTS(
			SELECT 1 FROM blocking_deps bd
			JOIN work_items wi ON wi.id = bd.from_id
			WHERE wi.status NOT IN ` + terminalSuccessStatuses + `
		)`
	var satisfied bool
	err := tx.QueryRow(ctx, q, tenantID, workItemID).Scan(&satisfied)
	if err != nil {
		return false, fmt.Errorf("db: check dependencies satisfied: %w", err)
	}
	return satisfied, nil
}

// GetLatestExecutionForTask returns the most recent execution for a
// task within the tenant scope. Returns ErrNotFound if none exist.
func GetLatestExecutionForTask(ctx context.Context, tx pgx.Tx, tenantID, taskID string) (ExecutionRow, error) {
	const q = `SELECT we.id, we.tenant_id, we.project_id, we.task_id, we.worker_id, we.worker_version,
		we.adapter_id, we.status, we.health_state, we.started_at, we.ended_at,
		we.token_usage, we.cost_usd, we.checkpoint_ref, we.recovery_id,
		we.worktree_status, we.worktree_path, we.worktree_branch, we.pr_url, we.pr_state,
		we.workflow_run_id, we.workflow_step_id, COALESCE(w.name, '') AS workflow_name, COALESCE(wkr.name, '') AS worker_name, we.error_message, we.output, we.conversation, we.is_follow_up, we.iteration, we.version,
		we.created_at, we.updated_at
		FROM worker_executions we
		LEFT JOIN workflow_runs wr ON wr.id = we.workflow_run_id
		LEFT JOIN workflows w ON w.id = wr.workflow_id
		LEFT JOIN workers wkr ON wkr.id = we.worker_id AND wkr.tenant_id = we.tenant_id
		WHERE we.task_id = $1 AND we.tenant_id = $2
		ORDER BY we.created_at DESC LIMIT 1`
	var e ExecutionRow
	err := tx.QueryRow(ctx, q, taskID, tenantID).Scan(
		&e.ID, &e.TenantID, &e.ProjectID, &e.TaskID, &e.WorkerID,
		&e.WorkerVersion, &e.AdapterID, &e.Status, &e.HealthState,
		&e.StartedAt, &e.EndedAt, &e.TokenUsage, &e.CostUSD,
		&e.CheckpointRef, &e.RecoveryID,
		&e.WorktreeStatus, &e.WorktreePath, &e.WorktreeBranch, &e.PrURL, &e.PrState,
		&e.WorkflowRunID, &e.WorkflowStepID, &e.WorkflowName, &e.WorkerName,
		&e.ErrorMessage, &e.Output, &e.Conversation, &e.IsFollowUp, &e.Iteration,
		&e.Version,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return ExecutionRow{}, fmt.Errorf("db: get latest execution for task: %w", err)
	}
	return e, nil
}
