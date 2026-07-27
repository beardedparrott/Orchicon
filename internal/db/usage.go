package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/oklog/ulid/v2"
)

// UsageRecordRow is the in-memory representation of a usage_records row
// (docs/08 §5.2, docs/09 §3.7). The AI Gateway writes these as the
// source of truth; OTel metrics mirror them to ClickHouse for fast
// telemetry queries.
type UsageRecordRow struct {
	ID               string
	TenantID         string
	ProjectID        string
	TaskID           string
	ExecutionID      string
	WorkerID         string
	Provider         string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	CostUSD          float64
	CorrelationID    string
	TraceID          string
	OccurredAt       time.Time
	CreatedAt        time.Time
}

// CreateUsageRecord inserts a usage record within the given tenant-scoped
// transaction. The caller is responsible for the outbox event (docs/09 §6)
// if a streaming projection is needed; this function only writes the row.
func CreateUsageRecord(ctx context.Context, tx pgx.Tx, row UsageRecordRow) (UsageRecordRow, error) {
	if row.ID == "" {
		row.ID = ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	}
	if row.OccurredAt.IsZero() {
		row.OccurredAt = time.Now().UTC()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.TotalTokens == 0 {
		row.TotalTokens = row.PromptTokens + row.CompletionTokens
	}
	const q = `INSERT INTO usage_records
		(id, tenant_id, project_id, task_id, execution_id, worker_id,
		 provider, model, prompt_tokens, completion_tokens, total_tokens,
		 cost_usd, correlation_id, trace_id, occurred_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`
	if _, err := tx.Exec(ctx, q,
		row.ID, row.TenantID, row.ProjectID, row.TaskID, row.ExecutionID, row.WorkerID,
		row.Provider, row.Model, row.PromptTokens, row.CompletionTokens, row.TotalTokens,
		row.CostUSD, row.CorrelationID, row.TraceID, row.OccurredAt, row.CreatedAt,
	); err != nil {
		return UsageRecordRow{}, fmt.Errorf("db: create usage record: %w", err)
	}
	return row, nil
}

// ListUsageRecordsFilter scopes a usage query. TenantID is required; the
// data-access layer enforces it (AGENTS.md: no cross-tenant queries).
type ListUsageRecordsFilter struct {
	TenantID    string
	ProjectID   string // optional
	TaskID      string // optional
	ExecutionID string // optional
	Provider    string // optional
	Model       string // optional
	StartTime   time.Time
	EndTime     time.Time
	PageSize    int32
}

// ListUsageRecords returns usage records matching the filter, ordered
// most-recent first. Tenant-scoped via the TenantTx (RLS backstop).
func ListUsageRecords(ctx context.Context, tx pgx.Tx, f ListUsageRecordsFilter) ([]UsageRecordRow, error) {
	if f.TenantID == "" {
		return nil, fmt.Errorf("db: list usage records: tenant_id required")
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 100
	}
	const q = `SELECT id, tenant_id, project_id, task_id, execution_id, worker_id,
		provider, model, prompt_tokens, completion_tokens, total_tokens,
		cost_usd, correlation_id, trace_id, occurred_at, created_at
		FROM usage_records
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR task_id = $3)
		  AND ($4 = '' OR execution_id = $4)
		  AND ($5 = '' OR provider = $5)
		  AND ($6 = '' OR model = $6)
		  AND ($7::timestamptz <= 'epoch'::timestamptz OR occurred_at >= $7::timestamptz)
		  AND ($8::timestamptz <= 'epoch'::timestamptz OR occurred_at <  $8::timestamptz)
		ORDER BY occurred_at DESC
		LIMIT $9`
	rows, err := tx.Query(ctx, q,
		f.TenantID, f.ProjectID, f.TaskID, f.ExecutionID, f.Provider, f.Model,
		f.StartTime, f.EndTime, f.PageSize,
	)
	if err != nil {
		return nil, fmt.Errorf("db: list usage records: %w", err)
	}
	defer rows.Close()
	var out []UsageRecordRow
	for rows.Next() {
		r, err := scanUsageRecord(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CostSummaryRow is an aggregated cost roll-up at one drill-down level
// (docs/10 §11: Tenant → Project → Task → Execution).
type CostSummaryRow struct {
	GroupKey          string
	TotalTokens       int64
	PromptTokens      int64
	CompletionTokens  int64
	CostUSD           float64
	ExecutionCount    int32
	RecordCount       int32
}

// CostRollupLevel selects the group-by column for GetCostRollup.
type CostRollupLevel string

const (
	RollupTenant    CostRollupLevel = "tenant"
	RollupProject   CostRollupLevel = "project"
	RollupTask      CostRollupLevel = "task"
	RollupExecution CostRollupLevel = "execution"
	RollupModel     CostRollupLevel = "model"
)

// GetCostRollup aggregates usage records to the requested drill-down
// level, scoped to an optional parent (project/task/execution). The
// tenant_id filter is enforced by RLS + the explicit WHERE.
func GetCostRollup(ctx context.Context, tx pgx.Tx, tenantID string, level CostRollupLevel, projectID, taskID, executionID string, start, end time.Time) ([]CostSummaryRow, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("db: cost rollup: tenant_id required")
	}
	var groupCol string
	switch level {
	case RollupTenant:
		groupCol = "tenant_id"
	case RollupProject:
		groupCol = "project_id"
	case RollupTask:
		groupCol = "task_id"
	case RollupExecution:
		groupCol = "execution_id"
	case RollupModel:
		groupCol = "model"
	default:
		return nil, fmt.Errorf("db: cost rollup: unknown level %q", level)
	}
	q := fmt.Sprintf(`SELECT %s AS group_key,
		COALESCE(SUM(total_tokens), 0) AS total_tokens,
		COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
		COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
		COALESCE(SUM(cost_usd), 0) AS cost_usd,
		COUNT(DISTINCT execution_id) AS execution_count,
		COUNT(*) AS record_count
		FROM usage_records
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR task_id = $3)
		  AND ($4 = '' OR execution_id = $4)
		  AND ($5::timestamptz <= 'epoch'::timestamptz OR occurred_at >= $5::timestamptz)
		  AND ($6::timestamptz <= 'epoch'::timestamptz OR occurred_at <  $6::timestamptz)
		GROUP BY %s
		ORDER BY cost_usd DESC`, groupCol, groupCol)
	rows, err := tx.Query(ctx, q, tenantID, projectID, taskID, executionID, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: cost rollup: %w", err)
	}
	defer rows.Close()
	var out []CostSummaryRow
	for rows.Next() {
		var r CostSummaryRow
		if err := rows.Scan(&r.GroupKey, &r.TotalTokens, &r.PromptTokens,
			&r.CompletionTokens, &r.CostUSD, &r.ExecutionCount, &r.RecordCount,
		); err != nil {
			return nil, fmt.Errorf("db: scan cost rollup: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCostTotal returns the grand total for the window + scope.
func GetCostTotal(ctx context.Context, tx pgx.Tx, tenantID, projectID, taskID, executionID string, start, end time.Time) (CostSummaryRow, error) {
	const q = `SELECT COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cost_usd), 0),
		COUNT(DISTINCT execution_id),
		COUNT(*)
		FROM usage_records
		WHERE tenant_id = $1
		  AND ($2 = '' OR project_id = $2)
		  AND ($3 = '' OR task_id = $3)
		  AND ($4 = '' OR execution_id = $4)
		  AND ($5::timestamptz <= 'epoch'::timestamptz OR occurred_at >= $5::timestamptz)
		  AND ($6::timestamptz <= 'epoch'::timestamptz OR occurred_at <  $6::timestamptz)`
	var r CostSummaryRow
	r.GroupKey = "total"
	if err := tx.QueryRow(ctx, q, tenantID, projectID, taskID, executionID, start, end).Scan(
		&r.TotalTokens, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD,
		&r.ExecutionCount, &r.RecordCount,
	); err != nil {
		return CostSummaryRow{}, fmt.Errorf("db: cost total: %w", err)
	}
	return r, nil
}

// WorkflowCostRow is a cost roll-up grouped by workflow run.
type WorkflowCostRow struct {
	WorkflowRunID  string
	WorkflowID     string
	WorkflowName   string
	TotalCostUSD   float64
	TotalTokens    int64
	ExecutionCount int32
}

// WorkflowExecutionCostRow is a per-execution cost within a workflow run.
type WorkflowExecutionCostRow struct {
	ExecutionID     string
	WorkItemID      string
	WorkItemTitle   string
	WorkerID        string
	WorkerName      string
	WorkflowStepID  string
	CostUSD         float64
	TotalTokens     int64
	PromptTokens    int64
	CompletionTokens int64
}

// GetWorkflowCostRollup returns cost grouped by workflow run, joining
// usage_records with work_items and workflow_runs to attribute costs to
// workflows. When workflowRunID is non-empty, only that run is queried.
func GetWorkflowCostRollup(ctx context.Context, tx pgx.Tx, tenantID, workflowRunID string, start, end time.Time) ([]WorkflowCostRow, error) {
	const q = `SELECT
		wr.id AS workflow_run_id,
		w.id AS workflow_id,
		w.name AS workflow_name,
		COALESCE(SUM(ur.cost_usd), 0) AS total_cost,
		COALESCE(SUM(ur.total_tokens), 0) AS total_tokens,
		COUNT(DISTINCT ur.execution_id) AS execution_count
		FROM usage_records ur
		JOIN work_items wi ON ur.task_id = wi.id
		JOIN workflow_runs wr ON wi.workflow_run_id = wr.id
		JOIN workflows w ON wr.workflow_id = w.id
		WHERE ur.tenant_id = $1
		  AND ($2 = '' OR wi.workflow_run_id = $2)
		  AND ($3::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at >= $3::timestamptz)
		  AND ($4::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at <  $4::timestamptz)
		GROUP BY wr.id, w.id, w.name
		ORDER BY total_cost DESC`
	rows, err := tx.Query(ctx, q, tenantID, workflowRunID, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: workflow cost rollup: %w", err)
	}
	defer rows.Close()
	var out []WorkflowCostRow
	for rows.Next() {
		var r WorkflowCostRow
		if err := rows.Scan(&r.WorkflowRunID, &r.WorkflowID, &r.WorkflowName,
			&r.TotalCostUSD, &r.TotalTokens, &r.ExecutionCount,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow cost: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowExecutionCosts returns per-execution cost breakdown for a
// single workflow run, with worker names and work item titles.
func GetWorkflowExecutionCosts(ctx context.Context, tx pgx.Tx, tenantID, workflowRunID string) ([]WorkflowExecutionCostRow, error) {
	const q = `SELECT
		we.id AS execution_id,
		COALESCE(wi.id, '') AS work_item_id,
		COALESCE(wi.title, '') AS work_item_title,
		COALESCE(we.worker_id, '') AS worker_id,
		COALESCE(w.name, '') AS worker_name,
		COALESCE(we.workflow_step_id, wi.workflow_step_id, '') AS workflow_step_id,
		SUM(ur.cost_usd) AS cost_usd,
		SUM(ur.total_tokens) AS total_tokens,
		SUM(ur.prompt_tokens) AS prompt_tokens,
		SUM(ur.completion_tokens) AS completion_tokens
		FROM worker_executions we
		JOIN usage_records ur ON ur.execution_id = we.id
		LEFT JOIN work_items wi ON wi.id = we.task_id
		LEFT JOIN workers w ON w.id = we.worker_id
		WHERE we.tenant_id = $1 AND we.workflow_run_id = $2
		GROUP BY we.id, wi.id, wi.title, we.worker_id, w.name, we.workflow_step_id, wi.workflow_step_id
		ORDER BY we.started_at`
	rows, err := tx.Query(ctx, q, tenantID, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("db: workflow execution costs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowExecutionCostRow
	for rows.Next() {
		var r WorkflowExecutionCostRow
		if err := rows.Scan(&r.ExecutionID, &r.WorkItemID, &r.WorkItemTitle,
			&r.WorkerID, &r.WorkerName, &r.WorkflowStepID,
			&r.CostUSD, &r.TotalTokens, &r.PromptTokens, &r.CompletionTokens,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow execution cost: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanUsageRecord(ctx context.Context, rows pgx.Rows) (UsageRecordRow, error) {
	var r UsageRecordRow
	var occurredAt, createdAt pgtype.Timestamptz
	if err := rows.Scan(
		&r.ID, &r.TenantID, &r.ProjectID, &r.TaskID, &r.ExecutionID, &r.WorkerID,
		&r.Provider, &r.Model, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
		&r.CostUSD, &r.CorrelationID, &r.TraceID, &occurredAt, &createdAt,
	); err != nil {
		return UsageRecordRow{}, fmt.Errorf("db: scan usage record: %w", err)
	}
	if occurredAt.Valid {
		r.OccurredAt = occurredAt.Time
	}
	if createdAt.Valid {
		r.CreatedAt = createdAt.Time
	}
	return r, nil
}
