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
// source of truth; OTel metrics mirror them to VictoriaMetrics for fast
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
	WorkerName       string // denormalised via LEFT JOIN at query time
	TaskTitle        string // denormalised via LEFT JOIN at query time
	WorkflowRunID    string // immutable; survives execution deletion
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
		 cost_usd, correlation_id, trace_id, occurred_at, created_at,
		 workflow_run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`
	if _, err := tx.Exec(ctx, q,
		row.ID, row.TenantID, row.ProjectID, row.TaskID, row.ExecutionID, row.WorkerID,
		row.Provider, row.Model, row.PromptTokens, row.CompletionTokens, row.TotalTokens,
		row.CostUSD, row.CorrelationID, row.TraceID, row.OccurredAt, row.CreatedAt,
		row.WorkflowRunID,
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
	const q = `SELECT ur.id, ur.tenant_id, ur.project_id, ur.task_id, ur.execution_id, ur.worker_id,
		ur.provider, ur.model, ur.prompt_tokens, ur.completion_tokens, ur.total_tokens,
		ur.cost_usd, ur.correlation_id, ur.trace_id, ur.occurred_at, ur.created_at,
		COALESCE(w.name, '') AS worker_name,
		COALESCE(wi.title, '') AS task_title
		FROM usage_records ur
		LEFT JOIN worker_executions we ON we.id = ur.execution_id
		LEFT JOIN workers w ON w.id = we.worker_id
		LEFT JOIN work_items wi ON wi.id = ur.task_id
		WHERE ur.tenant_id = $1
		  AND ($2 = '' OR ur.project_id = $2)
		  AND ($3 = '' OR ur.task_id = $3)
		  AND ($4 = '' OR ur.execution_id = $4)
		  AND ($5 = '' OR ur.provider = $5)
		  AND ($6 = '' OR ur.model = $6)
		  AND ($7::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at >= $7::timestamptz)
		  AND ($8::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at <  $8::timestamptz)
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
	DisplayName       string // human-readable name populated by the service layer
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

// WorkflowAggregateRow is a cost roll-up grouped by workflow (across all
// runs). Used for the top-level "By Workflow" hierarchy.
type WorkflowAggregateRow struct {
	WorkflowID     string
	WorkflowName   string
	TotalCostUSD   float64
	TotalTokens    int64
	RunCount       int32
	ExecutionCount int32
}

// WorkflowRunCostRow is a cost roll-up grouped by workflow run.
type WorkflowRunCostRow struct {
	WorkflowRunID  string
	WorkflowID     string
	TotalCostUSD   float64
	TotalTokens    int64
	ExecutionCount int32
	RunStatus      string
}

// WorkflowWorkerCostRow is a per-worker cost summary within one run.
type WorkflowWorkerCostRow struct {
	WorkerID        string
	WorkerName      string
	TotalCostUSD    float64
	TotalTokens     int64
	PromptTokens    int64
	CompletionTokens int64
	ExecutionCount  int32
}

// GetWorkflowAggregateCosts returns cost grouped by workflow (across all
// runs). Uses worker_executions.workflow_run_id (immutable, set at creation
// time) when available, falling back to work_items.workflow_run_id (mutable,
// latest-run value) for old executions where the column was not populated.
func GetWorkflowAggregateCosts(ctx context.Context, tx pgx.Tx, tenantID string, start, end time.Time) ([]WorkflowAggregateRow, error) {
	const q = `SELECT
		w.id AS workflow_id,
		w.name AS workflow_name,
		COALESCE(SUM(ur.cost_usd), 0) AS total_cost,
		COALESCE(SUM(ur.total_tokens), 0) AS total_tokens,
		COUNT(DISTINCT wr.id) AS run_count,
		COUNT(DISTINCT ur.execution_id) AS execution_count
		FROM usage_records ur
		JOIN work_items wi ON ur.task_id = wi.id
		LEFT JOIN worker_executions we ON we.id = ur.execution_id
		JOIN workflow_runs wr ON COALESCE(NULLIF(ur.workflow_run_id, ''), NULLIF(we.workflow_run_id, ''), NULLIF(wi.workflow_run_id, ''), '') = wr.id
		JOIN workflows w ON wr.workflow_id = w.id
		WHERE ur.tenant_id = $1
		  AND ($2::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at >= $2::timestamptz)
		  AND ($3::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at <  $3::timestamptz)
		GROUP BY w.id, w.name
		ORDER BY total_cost DESC`
	rows, err := tx.Query(ctx, q, tenantID, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: workflow aggregate costs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowAggregateRow
	for rows.Next() {
		var r WorkflowAggregateRow
		if err := rows.Scan(&r.WorkflowID, &r.WorkflowName,
			&r.TotalCostUSD, &r.TotalTokens, &r.RunCount, &r.ExecutionCount,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow aggregate: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowRunCosts returns cost grouped by workflow run for a given
// workflow. Uses COALESCE fallback to work_items.workflow_run_id for old
// executions where the column was not populated.
func GetWorkflowRunCosts(ctx context.Context, tx pgx.Tx, tenantID, workflowID string, start, end time.Time) ([]WorkflowRunCostRow, error) {
	const q = `SELECT
		wr.id AS workflow_run_id,
		w.id AS workflow_id,
		COALESCE(SUM(ur.cost_usd), 0) AS total_cost,
		COALESCE(SUM(ur.total_tokens), 0) AS total_tokens,
		COUNT(DISTINCT ur.execution_id) AS execution_count,
		wr.status AS run_status
		FROM usage_records ur
		JOIN work_items wi ON ur.task_id = wi.id
		LEFT JOIN worker_executions we ON we.id = ur.execution_id
		JOIN workflow_runs wr ON COALESCE(NULLIF(ur.workflow_run_id, ''), NULLIF(we.workflow_run_id, ''), NULLIF(wi.workflow_run_id, ''), '') = wr.id
		JOIN workflows w ON wr.workflow_id = w.id
		WHERE ur.tenant_id = $1 AND w.id = $2
		  AND ($3::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at >= $3::timestamptz)
		  AND ($4::timestamptz <= 'epoch'::timestamptz OR ur.occurred_at <  $4::timestamptz)
		GROUP BY wr.id, w.id, wr.status
		ORDER BY total_cost DESC`
	rows, err := tx.Query(ctx, q, tenantID, workflowID, start, end)
	if err != nil {
		return nil, fmt.Errorf("db: workflow run costs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowRunCostRow
	for rows.Next() {
		var r WorkflowRunCostRow
		if err := rows.Scan(&r.WorkflowRunID, &r.WorkflowID,
			&r.TotalCostUSD, &r.TotalTokens, &r.ExecutionCount, &r.RunStatus,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow run cost: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkflowWorkerCosts returns cost grouped by worker type within a
// single workflow run. Uses COALESCE fallback to work_items.workflow_run_id
// for old executions where the column was not populated.
func GetWorkflowWorkerCosts(ctx context.Context, tx pgx.Tx, tenantID, workflowRunID string) ([]WorkflowWorkerCostRow, error) {
	const q = `SELECT
		COALESCE(we.worker_id, ur.worker_id, '') AS worker_id,
		COALESCE(w2.name, '') AS worker_name,
		SUM(ur.cost_usd) AS cost_usd,
		SUM(ur.total_tokens) AS total_tokens,
		SUM(ur.prompt_tokens) AS prompt_tokens,
		SUM(ur.completion_tokens) AS completion_tokens,
		COUNT(DISTINCT ur.execution_id) AS execution_count
		FROM usage_records ur
		JOIN work_items wi ON ur.task_id = wi.id
		LEFT JOIN worker_executions we ON we.id = ur.execution_id
		LEFT JOIN workers w2 ON w2.id = COALESCE(we.worker_id, ur.worker_id, '')
		WHERE ur.tenant_id = $1 AND COALESCE(NULLIF(ur.workflow_run_id, ''), NULLIF(we.workflow_run_id, ''), NULLIF(wi.workflow_run_id, ''), '') = $2
		GROUP BY COALESCE(we.worker_id, ur.worker_id, ''), w2.name
		ORDER BY cost_usd DESC`
	rows, err := tx.Query(ctx, q, tenantID, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("db: workflow worker costs: %w", err)
	}
	defer rows.Close()
	var out []WorkflowWorkerCostRow
	for rows.Next() {
		var r WorkflowWorkerCostRow
		if err := rows.Scan(&r.WorkerID, &r.WorkerName,
			&r.TotalCostUSD, &r.TotalTokens, &r.PromptTokens, &r.CompletionTokens,
			&r.ExecutionCount,
		); err != nil {
			return nil, fmt.Errorf("db: scan workflow worker cost: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExecutionDisplayInfo holds human-readable names for an execution.
type ExecutionDisplayInfo struct {
	ExecutionID string
	WorkerName  string
	TaskTitle   string
}

// ResolveExecutionDisplayNames returns display info for the given
// execution IDs. Queries usage_records directly JOINed with workers and
// work_items (not via worker_executions) so names are always resolved
// even if the execution row has been cleaned up.
func ResolveExecutionDisplayNames(ctx context.Context, tx pgx.Tx, tenantID string, execIDs []string) (map[string]ExecutionDisplayInfo, error) {
	if len(execIDs) == 0 {
		return nil, nil
	}
	const q = `SELECT DISTINCT ur.execution_id,
		COALESCE(w.name, '') AS worker_name,
		COALESCE(wi.title, '') AS task_title
		FROM usage_records ur
		LEFT JOIN worker_executions we ON we.id = ur.execution_id
		LEFT JOIN workers w ON w.id = COALESCE(we.worker_id, ur.worker_id, '')
		LEFT JOIN work_items wi ON wi.id = COALESCE(we.task_id, ur.task_id, '')
		WHERE ur.tenant_id = $1 AND ur.execution_id = ANY($2)`
	rows, err := tx.Query(ctx, q, tenantID, execIDs)
	if err != nil {
		return nil, fmt.Errorf("db: resolve execution display names: %w", err)
	}
	defer rows.Close()
	out := make(map[string]ExecutionDisplayInfo, len(execIDs))
	for rows.Next() {
		var info ExecutionDisplayInfo
		if err := rows.Scan(&info.ExecutionID, &info.WorkerName, &info.TaskTitle); err != nil {
			return nil, fmt.Errorf("db: scan execution display info: %w", err)
		}
		out[info.ExecutionID] = info
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
		&r.WorkerName, &r.TaskTitle,
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
