package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecoveryExecutionRow is the data-access shape of a recovery_executions
// table row (docs/06 §2, docs/09 §3.6). A specialized WorkerExecution
// whose Worker is the recovery workflow driver.
type RecoveryExecutionRow struct {
	ID                 string
	TenantID           string
	ProjectID          string
	TaskID             string
	FailedExecutionID  string
	RecoveryWorkflowID  string
	TriggerReason      string
	Level              int32
	Status             string
	CurrentStep        string
	// Strategy is the recovery strategy routed on by the engine (PR C).
	// One of: summarize_restart (default — the 6-step flow), stop,
	// human_escalation, retry_n. Empty string falls back to
	// summarize_restart for backward compat with rows written before
	// the column existed.
	Strategy           string
	ResumptionPath     string
	BudgetTokensLimit  int64
	BudgetTokensUsed   int64
	BudgetCostLimitUSD float64
	BudgetCostUsedUSD  float64
	BudgetRelaxFraction float64
	NeedsHumanApproval bool
	ContinuationPlanID  string
	ReviewerWorkerID    string
	Summary            string
	Version            int
	TriggeredAt        time.Time
	EndedAt            *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// RecoveryStepRunRow is the data-access shape of a recovery_step_runs
// table row (docs/06 §3, §9). Rich narrative fields for the timeline
// (docs/06 §11).
type RecoveryStepRunRow struct {
	ID                string
	TenantID          string
	RecoveryID        string
	StepID            string
	StepName          string
	Status            string
	Attempt           int
	Result            []byte // jsonb
	WorkerExecutionID  string
	TriggerReason     string
	AffectedRef       string
	AdapterRef        string
	Action            string
	StartedAt         *time.Time
	EndedAt           *time.Time
	Version           int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ContinuationPlanRow is the data-access shape of a continuation_plans
// table row (docs/06 §8).
type ContinuationPlanRow struct {
	ID              string
	TenantID        string
	RecoveryID      string
	Version         int
	Completed       []byte // jsonb
	InProgress      []byte // jsonb
	Remaining       []byte // jsonb
	Corrections     []byte // jsonb
	ContextSummary  string
	CheckpointRef   string
	Assumptions     []byte // jsonb
	Status          string
	ApprovedBy      string
	CreatedAt       time.Time
	DecidedAt       *time.Time
}

// CreateRecoveryExecution inserts a new recovery execution row.
//
// Strategy (PR C — recovery as typed work items) is intentionally NOT in
// the INSERT column list: the column has a DEFAULT of 'summarize_restart'
// (migration 20260715000000_recovery_strategy.sql) and the engine
// currently sets Strategy on the row only as a routing hint — the
// recover-strategy rounding happens at the top of the engine loop
// (engine.go applyStrategy), not via this INSERT. Preserving the column
// default keeps backward compat with rows written before PR C without
// requiring the engine to plumb strategy through every reader.
//
// Parameter numbering used to skip $12 (params went $1..$11, $13..$22),
// which Postgres rejected with "could not determine data type of
// parameter $12" — the engine's insert into recovery_executions has
// been silently failing on every failed execution, so recovery never
// actually started. Renumber contiguously to fix it.
func CreateRecoveryExecution(ctx context.Context, tx pgx.Tx, r RecoveryExecutionRow) (RecoveryExecutionRow, error) {
	const q = `INSERT INTO recovery_executions
		(id, tenant_id, project_id, task_id, failed_execution_id,
		 recovery_workflow_id, trigger_reason, level, status, current_step,
		 resumption_path, budget_tokens_limit, budget_tokens_used,
		 budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		 needs_human_approval, continuation_plan_id, reviewer_worker_id,
		 summary, triggered_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		 $15, $16, $17, $18, $19, $20, $21)
		RETURNING id, tenant_id, project_id, task_id, failed_execution_id,
			recovery_workflow_id, trigger_reason, level, status, current_step,
			resumption_path, budget_tokens_limit, budget_tokens_used,
			budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
			needs_human_approval, continuation_plan_id, reviewer_worker_id,
			summary, version, triggered_at, ended_at, created_at, updated_at`
	row := r
	if row.TriggeredAt.IsZero() {
		row.TriggeredAt = time.Now().UTC()
	}
	err := tx.QueryRow(ctx, q,
		row.ID, row.TenantID, row.ProjectID, row.TaskID, row.FailedExecutionID,
		row.RecoveryWorkflowID, row.TriggerReason, row.Level, row.Status,
		row.CurrentStep, row.ResumptionPath, row.BudgetTokensLimit,
		row.BudgetTokensUsed, row.BudgetCostLimitUSD, row.BudgetCostUsedUSD,
		row.BudgetRelaxFraction, row.NeedsHumanApproval,
		row.ContinuationPlanID, row.ReviewerWorkerID, row.Summary, row.TriggeredAt,
	).Scan(
		&row.ID, &row.TenantID, &row.ProjectID, &row.TaskID, &row.FailedExecutionID,
		&row.RecoveryWorkflowID, &row.TriggerReason, &row.Level, &row.Status,
		&row.CurrentStep, &row.ResumptionPath, &row.BudgetTokensLimit,
		&row.BudgetTokensUsed, &row.BudgetCostLimitUSD, &row.BudgetCostUsedUSD,
		&row.BudgetRelaxFraction, &row.NeedsHumanApproval,
		&row.ContinuationPlanID, &row.ReviewerWorkerID, &row.Summary,
		&row.Version, &row.TriggeredAt, &row.EndedAt, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return RecoveryExecutionRow{}, fmt.Errorf("db: create recovery execution: %w", err)
	}
	return row, nil
}

// GetRecoveryExecution fetches a single recovery by id within the tenant.
func GetRecoveryExecution(ctx context.Context, tx pgx.Tx, tenantID, id string) (RecoveryExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step, strategy,
		resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at
		FROM recovery_executions WHERE id = $1 AND tenant_id = $2`
	var r RecoveryExecutionRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
		&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
		&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
		&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
		&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
		&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
		&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryExecutionRow{}, fmt.Errorf("db: get recovery execution: %w", err)
	}
	return r, nil
}

// ListRecoveriesFilter scopes the recovery list query.
type ListRecoveriesFilter struct {
	TenantID  string
	ProjectID string
	TaskID    string
	Status    string
	PageSize  int
	AfterID   string
}

// ListRecoveries returns a page of recoveries, newest first.
func ListRecoveries(ctx context.Context, tx pgx.Tx, f ListRecoveriesFilter) ([]RecoveryExecutionRow, error) {
	if f.PageSize <= 0 || f.PageSize > 1000 {
		f.PageSize = 100
	}
	q := `SELECT id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step, strategy,
		resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at
		FROM recovery_executions WHERE tenant_id = $1 AND ($2 = '' OR id < $2)`
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
	q += ` ORDER BY triggered_at DESC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, f.PageSize)
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list recoveries: %w", err)
	}
	defer rows.Close()
	var out []RecoveryExecutionRow
	for rows.Next() {
		var r RecoveryExecutionRow
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
			&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
			&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
			&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
			&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
			&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
			&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan recovery: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPendingRecoveries returns recoveries in a non-terminal state
// (pending/running/blocked) for the reconciler to progress (docs/06 §9).
func ListPendingRecoveries(ctx context.Context, tx pgx.Tx, tenantID string) ([]RecoveryExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step,
		strategy, resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at
		FROM recovery_executions
		WHERE tenant_id = $1 AND status IN ('pending', 'running', 'blocked')
		ORDER BY triggered_at ASC`
	rows, err := tx.Query(ctx, q, tenantID)
	if err != nil {
		return nil, fmt.Errorf("db: list pending recoveries: %w", err)
	}
	defer rows.Close()
	var out []RecoveryExecutionRow
	for rows.Next() {
		var r RecoveryExecutionRow
		if err := rows.Scan(
			&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
			&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
			&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
			&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
			&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
			&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
			&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan recovery: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetActiveRecoveryForTask returns a non-terminal recovery for a task,
// if one exists. Used to avoid duplicate recoveries (docs/06 §9
// idempotency).
func GetActiveRecoveryForTask(ctx context.Context, tx pgx.Tx, tenantID, taskID string) (RecoveryExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step,
		resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at
		FROM recovery_executions
		WHERE tenant_id = $1 AND task_id = $2 AND status IN ('pending', 'running', 'blocked')
		ORDER BY triggered_at DESC LIMIT 1`
	var r RecoveryExecutionRow
	err := tx.QueryRow(ctx, q, tenantID, taskID).Scan(
		&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
		&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
		&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
		&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
		&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
		&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
		&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryExecutionRow{}, fmt.Errorf("db: get active recovery for task: %w", err)
	}
	return r, nil
}

// GetLatestRecoveryForTask returns the most recent recovery execution for
// a task, regardless of status. Used by the workflow RECOVER step to
// determine whether a recovery has completed (terminal) or is still
// in flight.
func GetLatestRecoveryForTask(ctx context.Context, tx pgx.Tx, tenantID, taskID string) (RecoveryExecutionRow, error) {
	const q = `SELECT id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step,
		strategy, resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at
		FROM recovery_executions
		WHERE tenant_id = $1 AND task_id = $2
		ORDER BY triggered_at DESC LIMIT 1`
	var r RecoveryExecutionRow
	err := tx.QueryRow(ctx, q, tenantID, taskID).Scan(
		&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
		&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
		&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
		&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
		&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
		&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
		&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryExecutionRow{}, fmt.Errorf("db: get latest recovery for task: %w", err)
	}
	return r, nil
}

// UpdateRecoveryExecutionFields is a partial update applied with
// optimistic concurrency (docs/09 §5).
type UpdateRecoveryExecutionFields struct {
	Status               *string
	CurrentStep          *string
	ResumptionPath       *string
	BudgetTokensUsed     *int64
	BudgetCostUsedUSD    *float64
	BudgetRelaxFraction  *float64
	BudgetTokensLimit    *int64
	BudgetCostLimitUSD   *float64
	NeedsHumanApproval   *bool
	ContinuationPlanID   *string
	ReviewerWorkerID     *string
	Summary              *string
	Level                *int32
	// Strategy is the recovery strategy routed on by the engine. PR C
	// routes per row; left nil on updates that don't change strategy.
	Strategy             *string
	EndedAt              *time.Time
}

// UpdateRecoveryExecution applies a partial update with optimistic
// concurrency.
func UpdateRecoveryExecution(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateRecoveryExecutionFields) (RecoveryExecutionRow, error) {
	q := `UPDATE recovery_executions SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.CurrentStep != nil {
		q += fmt.Sprintf(`, current_step = $%d`, setIdx)
		args = append(args, *f.CurrentStep)
		setIdx++
	}
	if f.ResumptionPath != nil {
		q += fmt.Sprintf(`, resumption_path = $%d`, setIdx)
		args = append(args, *f.ResumptionPath)
		setIdx++
	}
	if f.BudgetTokensUsed != nil {
		q += fmt.Sprintf(`, budget_tokens_used = $%d`, setIdx)
		args = append(args, *f.BudgetTokensUsed)
		setIdx++
	}
	if f.BudgetCostUsedUSD != nil {
		q += fmt.Sprintf(`, budget_cost_used_usd = $%d`, setIdx)
		args = append(args, *f.BudgetCostUsedUSD)
		setIdx++
	}
	if f.BudgetRelaxFraction != nil {
		q += fmt.Sprintf(`, budget_relax_fraction = $%d`, setIdx)
		args = append(args, *f.BudgetRelaxFraction)
		setIdx++
	}
	if f.BudgetTokensLimit != nil {
		q += fmt.Sprintf(`, budget_tokens_limit = $%d`, setIdx)
		args = append(args, *f.BudgetTokensLimit)
		setIdx++
	}
	if f.BudgetCostLimitUSD != nil {
		q += fmt.Sprintf(`, budget_cost_limit_usd = $%d`, setIdx)
		args = append(args, *f.BudgetCostLimitUSD)
		setIdx++
	}
	if f.NeedsHumanApproval != nil {
		q += fmt.Sprintf(`, needs_human_approval = $%d`, setIdx)
		args = append(args, *f.NeedsHumanApproval)
		setIdx++
	}
	if f.ContinuationPlanID != nil {
		q += fmt.Sprintf(`, continuation_plan_id = $%d`, setIdx)
		args = append(args, *f.ContinuationPlanID)
		setIdx++
	}
	if f.ReviewerWorkerID != nil {
		q += fmt.Sprintf(`, reviewer_worker_id = $%d`, setIdx)
		args = append(args, *f.ReviewerWorkerID)
		setIdx++
	}
	if f.Summary != nil {
		q += fmt.Sprintf(`, summary = $%d`, setIdx)
		args = append(args, *f.Summary)
		setIdx++
	}
	if f.Level != nil {
		q += fmt.Sprintf(`, level = $%d`, setIdx)
		args = append(args, *f.Level)
		setIdx++
	}
	if f.Strategy != nil {
		q += fmt.Sprintf(`, strategy = $%d`, setIdx)
		args = append(args, *f.Strategy)
		setIdx++
	}
	if f.EndedAt != nil {
		q += fmt.Sprintf(`, ended_at = $%d`, setIdx)
		args = append(args, *f.EndedAt)
		setIdx++
	}
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, project_id, task_id, failed_execution_id,
		recovery_workflow_id, trigger_reason, level, status, current_step, strategy,
		resumption_path, budget_tokens_limit, budget_tokens_used,
		budget_cost_limit_usd, budget_cost_used_usd, budget_relax_fraction,
		needs_human_approval, continuation_plan_id, reviewer_worker_id,
		summary, version, triggered_at, ended_at, created_at, updated_at`
	var r RecoveryExecutionRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.FailedExecutionID,
		&r.RecoveryWorkflowID, &r.TriggerReason, &r.Level, &r.Status,
		&r.CurrentStep, &r.Strategy, &r.ResumptionPath, &r.BudgetTokensLimit,
		&r.BudgetTokensUsed, &r.BudgetCostLimitUSD, &r.BudgetCostUsedUSD,
		&r.BudgetRelaxFraction, &r.NeedsHumanApproval,
		&r.ContinuationPlanID, &r.ReviewerWorkerID, &r.Summary,
		&r.Version, &r.TriggeredAt, &r.EndedAt, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryExecutionRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryExecutionRow{}, fmt.Errorf("db: update recovery execution: %w", err)
	}
	return r, nil
}

// --- RecoveryStepRun ------------------------------------------------------

// CreateRecoveryStepRun inserts a new recovery step run row.
func CreateRecoveryStepRun(ctx context.Context, tx pgx.Tx, s RecoveryStepRunRow) (RecoveryStepRunRow, error) {
	if s.Result == nil {
		s.Result = []byte("{}")
	}
	const q = `INSERT INTO recovery_step_runs
		(id, tenant_id, recovery_id, step_id, step_name, status, attempt,
		 result, worker_execution_id, trigger_reason, affected_ref,
		 adapter_ref, action, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id, tenant_id, recovery_id, step_id, step_name, status,
			attempt, result, worker_execution_id, trigger_reason, affected_ref,
			adapter_ref, action, started_at, ended_at, version, created_at,
			updated_at`
	row := s
	err := tx.QueryRow(ctx, q,
		row.ID, row.TenantID, row.RecoveryID, row.StepID, row.StepName,
		row.Status, row.Attempt, row.Result, row.WorkerExecutionID,
		row.TriggerReason, row.AffectedRef, row.AdapterRef, row.Action,
		row.StartedAt,
	).Scan(
		&row.ID, &row.TenantID, &row.RecoveryID, &row.StepID, &row.StepName,
		&row.Status, &row.Attempt, &row.Result, &row.WorkerExecutionID,
		&row.TriggerReason, &row.AffectedRef, &row.AdapterRef, &row.Action,
		&row.StartedAt, &row.EndedAt, &row.Version, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		return RecoveryStepRunRow{}, fmt.Errorf("db: create recovery step run: %w", err)
	}
	return row, nil
}

// GetRecoveryStepRun fetches a single step run by id.
func GetRecoveryStepRun(ctx context.Context, tx pgx.Tx, tenantID, id string) (RecoveryStepRunRow, error) {
	const q = `SELECT id, tenant_id, recovery_id, step_id, step_name, status,
		attempt, result, worker_execution_id, trigger_reason, affected_ref,
		adapter_ref, action, started_at, ended_at, version, created_at,
		updated_at
		FROM recovery_step_runs WHERE id = $1 AND tenant_id = $2`
	var s RecoveryStepRunRow
	err := tx.QueryRow(ctx, q, id, tenantID).Scan(
		&s.ID, &s.TenantID, &s.RecoveryID, &s.StepID, &s.StepName,
		&s.Status, &s.Attempt, &s.Result, &s.WorkerExecutionID,
		&s.TriggerReason, &s.AffectedRef, &s.AdapterRef, &s.Action,
		&s.StartedAt, &s.EndedAt, &s.Version, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryStepRunRow{}, fmt.Errorf("db: get recovery step run: %w", err)
	}
	return s, nil
}

// GetRecoveryStepRunByStep returns the step run for a (recovery_id,
// step_id) pair.
func GetRecoveryStepRunByStep(ctx context.Context, tx pgx.Tx, tenantID, recoveryID, stepID string) (RecoveryStepRunRow, error) {
	const q = `SELECT id, tenant_id, recovery_id, step_id, step_name, status,
		attempt, result, worker_execution_id, trigger_reason, affected_ref,
		adapter_ref, action, started_at, ended_at, version, created_at,
		updated_at
		FROM recovery_step_runs
		WHERE tenant_id = $1 AND recovery_id = $2 AND step_id = $3
		ORDER BY created_at ASC LIMIT 1`
	var s RecoveryStepRunRow
	err := tx.QueryRow(ctx, q, tenantID, recoveryID, stepID).Scan(
		&s.ID, &s.TenantID, &s.RecoveryID, &s.StepID, &s.StepName,
		&s.Status, &s.Attempt, &s.Result, &s.WorkerExecutionID,
		&s.TriggerReason, &s.AffectedRef, &s.AdapterRef, &s.Action,
		&s.StartedAt, &s.EndedAt, &s.Version, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryStepRunRow{}, fmt.Errorf("db: get recovery step run by step: %w", err)
	}
	return s, nil
}

// ListRecoveryStepRuns returns all step runs for a recovery (the
// timeline materialized server-side — docs/06 §11).
func ListRecoveryStepRuns(ctx context.Context, tx pgx.Tx, tenantID, recoveryID string) ([]RecoveryStepRunRow, error) {
	const q = `SELECT id, tenant_id, recovery_id, step_id, step_name, status,
		attempt, result, worker_execution_id, trigger_reason, affected_ref,
		adapter_ref, action, started_at, ended_at, version, created_at,
		updated_at
		FROM recovery_step_runs
		WHERE tenant_id = $1 AND recovery_id = $2
		ORDER BY created_at ASC`
	rows, err := tx.Query(ctx, q, tenantID, recoveryID)
	if err != nil {
		return nil, fmt.Errorf("db: list recovery step runs: %w", err)
	}
	defer rows.Close()
	var out []RecoveryStepRunRow
	for rows.Next() {
		var s RecoveryStepRunRow
		if err := rows.Scan(
			&s.ID, &s.TenantID, &s.RecoveryID, &s.StepID, &s.StepName,
			&s.Status, &s.Attempt, &s.Result, &s.WorkerExecutionID,
			&s.TriggerReason, &s.AffectedRef, &s.AdapterRef, &s.Action,
			&s.StartedAt, &s.EndedAt, &s.Version, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan recovery step run: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateRecoveryStepRunFields is a partial update with optimistic
// concurrency (docs/09 §5).
type UpdateRecoveryStepRunFields struct {
	Status            *string
	Attempt           *int
	Result            *[]byte
	WorkerExecutionID *string
	TriggerReason     *string
	AffectedRef       *string
	AdapterRef        *string
	Action            *string
	StartedAt         *time.Time
	EndedAt           *time.Time
}

// UpdateRecoveryStepRun applies a partial update with optimistic
// concurrency. Idempotent: re-running a step for the same recovery yields
// the same artifacts (docs/06 §9).
func UpdateRecoveryStepRun(ctx context.Context, tx pgx.Tx, tenantID, id string, expectedVersion int, f UpdateRecoveryStepRunFields) (RecoveryStepRunRow, error) {
	q := `UPDATE recovery_step_runs SET updated_at = now(), version = version + 1`
	args := []any{tenantID, id, expectedVersion}
	setIdx := len(args) + 1
	if f.Status != nil {
		q += fmt.Sprintf(`, status = $%d`, setIdx)
		args = append(args, *f.Status)
		setIdx++
	}
	if f.Attempt != nil {
		q += fmt.Sprintf(`, attempt = $%d`, setIdx)
		args = append(args, *f.Attempt)
		setIdx++
	}
	if f.Result != nil {
		q += fmt.Sprintf(`, result = $%d`, setIdx)
		args = append(args, *f.Result)
		setIdx++
	}
	if f.WorkerExecutionID != nil {
		q += fmt.Sprintf(`, worker_execution_id = $%d`, setIdx)
		args = append(args, *f.WorkerExecutionID)
		setIdx++
	}
	if f.TriggerReason != nil {
		q += fmt.Sprintf(`, trigger_reason = $%d`, setIdx)
		args = append(args, *f.TriggerReason)
		setIdx++
	}
	if f.AffectedRef != nil {
		q += fmt.Sprintf(`, affected_ref = $%d`, setIdx)
		args = append(args, *f.AffectedRef)
		setIdx++
	}
	if f.AdapterRef != nil {
		q += fmt.Sprintf(`, adapter_ref = $%d`, setIdx)
		args = append(args, *f.AdapterRef)
		setIdx++
	}
	if f.Action != nil {
		q += fmt.Sprintf(`, action = $%d`, setIdx)
		args = append(args, *f.Action)
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
	q += ` WHERE tenant_id = $1 AND id = $2 AND version = $3`
	q += ` RETURNING id, tenant_id, recovery_id, step_id, step_name, status,
		attempt, result, worker_execution_id, trigger_reason, affected_ref,
		adapter_ref, action, started_at, ended_at, version, created_at,
		updated_at`
	var s RecoveryStepRunRow
	err := tx.QueryRow(ctx, q, args...).Scan(
		&s.ID, &s.TenantID, &s.RecoveryID, &s.StepID, &s.StepName,
		&s.Status, &s.Attempt, &s.Result, &s.WorkerExecutionID,
		&s.TriggerReason, &s.AffectedRef, &s.AdapterRef, &s.Action,
		&s.StartedAt, &s.EndedAt, &s.Version, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryStepRunRow{}, ErrNotFound
	}
	if err != nil {
		return RecoveryStepRunRow{}, fmt.Errorf("db: update recovery step run: %w", err)
	}
	return s, nil
}

// --- ContinuationPlan -----------------------------------------------------

// CreateContinuationPlan inserts a new continuation plan row (docs/06
// §8).
func CreateContinuationPlan(ctx context.Context, tx pgx.Tx, p ContinuationPlanRow) (ContinuationPlanRow, error) {
	row := p
	if row.Completed == nil {
		row.Completed = []byte("[]")
	}
	if row.InProgress == nil {
		row.InProgress = []byte("[]")
	}
	if row.Remaining == nil {
		row.Remaining = []byte("[]")
	}
	if row.Corrections == nil {
		row.Corrections = []byte("[]")
	}
	if row.Assumptions == nil {
		row.Assumptions = []byte("[]")
	}
	const q = `INSERT INTO continuation_plans
		(id, tenant_id, recovery_id, version, completed, in_progress,
		 remaining, corrections, context_summary, checkpoint_ref,
		 assumptions, status, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, tenant_id, recovery_id, version, completed, in_progress,
			remaining, corrections, context_summary, checkpoint_ref,
			assumptions, status, approved_by, created_at, decided_at`
	err := tx.QueryRow(ctx, q,
		row.ID, row.TenantID, row.RecoveryID, row.Version, row.Completed,
		row.InProgress, row.Remaining, row.Corrections, row.ContextSummary,
		row.CheckpointRef, row.Assumptions, row.Status, row.ApprovedBy,
	).Scan(
		&row.ID, &row.TenantID, &row.RecoveryID, &row.Version, &row.Completed,
		&row.InProgress, &row.Remaining, &row.Corrections, &row.ContextSummary,
		&row.CheckpointRef, &row.Assumptions, &row.Status, &row.ApprovedBy,
		&row.CreatedAt, &row.DecidedAt,
	)
	if err != nil {
		return ContinuationPlanRow{}, fmt.Errorf("db: create continuation plan: %w", err)
	}
	return row, nil
}

// GetContinuationPlanByRecovery returns the latest plan for a recovery.
func GetContinuationPlanByRecovery(ctx context.Context, tx pgx.Tx, tenantID, recoveryID string) (ContinuationPlanRow, error) {
	const q = `SELECT id, tenant_id, recovery_id, version, completed,
		in_progress, remaining, corrections, context_summary,
		checkpoint_ref, assumptions, status, approved_by, created_at,
		decided_at
		FROM continuation_plans
		WHERE tenant_id = $1 AND recovery_id = $2
		ORDER BY version DESC LIMIT 1`
	var p ContinuationPlanRow
	err := tx.QueryRow(ctx, q, tenantID, recoveryID).Scan(
		&p.ID, &p.TenantID, &p.RecoveryID, &p.Version, &p.Completed,
		&p.InProgress, &p.Remaining, &p.Corrections, &p.ContextSummary,
		&p.CheckpointRef, &p.Assumptions, &p.Status, &p.ApprovedBy,
		&p.CreatedAt, &p.DecidedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContinuationPlanRow{}, ErrNotFound
	}
	if err != nil {
		return ContinuationPlanRow{}, fmt.Errorf("db: get continuation plan: %w", err)
	}
	return p, nil
}

// UpdateContinuationPlanStatus sets the plan status (approved/rejected)
// with the deciding actor (docs/06 §8).
func UpdateContinuationPlanStatus(ctx context.Context, tx pgx.Tx, tenantID, id, status, approvedBy string) (ContinuationPlanRow, error) {
	const q = `UPDATE continuation_plans
		SET status = $3, approved_by = $4, decided_at = now()
		WHERE tenant_id = $1 AND id = $2
		RETURNING id, tenant_id, recovery_id, version, completed,
			in_progress, remaining, corrections, context_summary,
			checkpoint_ref, assumptions, status, approved_by, created_at,
			decided_at`
	var p ContinuationPlanRow
	err := tx.QueryRow(ctx, q, tenantID, id, status, approvedBy).Scan(
		&p.ID, &p.TenantID, &p.RecoveryID, &p.Version, &p.Completed,
		&p.InProgress, &p.Remaining, &p.Corrections, &p.ContextSummary,
		&p.CheckpointRef, &p.Assumptions, &p.Status, &p.ApprovedBy,
		&p.CreatedAt, &p.DecidedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ContinuationPlanRow{}, ErrNotFound
	}
	if err != nil {
		return ContinuationPlanRow{}, fmt.Errorf("db: update continuation plan status: %w", err)
	}
	return p, nil
}
