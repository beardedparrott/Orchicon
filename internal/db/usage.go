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
		  AND ($7::timestamptz = 'epoch' OR occurred_at >= $7::timestamptz)
		  AND ($8::timestamptz = 'epoch' OR occurred_at <  $8::timestamptz)
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
		  AND ($5::timestamptz = 'epoch' OR occurred_at >= $5::timestamptz)
		  AND ($6::timestamptz = 'epoch' OR occurred_at <  $6::timestamptz)
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
		  AND ($5::timestamptz = 'epoch' OR occurred_at >= $5::timestamptz)
		  AND ($6::timestamptz = 'epoch' OR occurred_at <  $6::timestamptz)`
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
