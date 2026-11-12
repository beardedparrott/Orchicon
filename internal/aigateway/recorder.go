package aigateway

import (
	"context"
	"log/slog"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
)

// UsageRecorder records LLM usage: Postgres (source of truth) + OTel metrics (VictoriaMetrics half
// of the dual-write).
type PricingResolver func(ctx context.Context, provider, model string) (*apiv1.ModelCost, bool)

type UsageRecorder struct {
	pool    *db.Pool
	log     *slog.Logger
	metrics *usageMetrics
	pricing PricingResolver
}

// NewUsageRecorder constructs a UsageRecorder.
func NewUsageRecorder(pool *db.Pool, log *slog.Logger) *UsageRecorder {
	return &UsageRecorder{pool: pool, log: log, metrics: newUsageMetrics(log)}
}

// SetPricingResolver injects the catalog pricing resolver.
func (u *UsageRecorder) SetPricingResolver(fn PricingResolver) { u.pricing = fn }

// UsageInput is the usage sample from an adapter telemetry event (docs/04 §6.1 step_finish carries
// tokens + cost).
type UsageInput struct {
	TenantID    string
	ProjectID   string
	TaskID      string
	ExecutionID string
	WorkerID    string
	// AdapterKind tags the OTel/adapter parity attribute and is persisted to usage_records so
	// Ask/worker usage is attributable to the adapter that drove the model call.
	AdapterKind string
	// SessionID carries the Ask Orchicon conversation id for Ask session attribution (there is no
	// execution/task/project for a chat turn).
	SessionID        string
	Provider         string
	Model            string
	PromptTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CompletionTokens int64
	ReasoningTokens  int64
	CostUSD          float64
	CorrelationID    string
	TraceID          string
	WorkflowRunID    string
}

// Record performs the dual-write: inserts a usage_records row in Postgres (in its own transaction —
// usage is recorded outside the execution's state transaction to avoid coupling telemetry.
func (u *UsageRecorder) Record(ctx context.Context, in UsageInput) (db.UsageRecordRow, error) {
	row := db.UsageRecordRow{
		TenantID:         in.TenantID,
		ProjectID:        in.ProjectID,
		TaskID:           in.TaskID,
		ExecutionID:      in.ExecutionID,
		WorkerID:         in.WorkerID,
		AdapterKind:      in.AdapterKind,
		SessionID:        in.SessionID,
		Provider:         in.Provider,
		Model:            in.Model,
		PromptTokens:     in.PromptTokens,
		CacheReadTokens:  in.CacheReadTokens,
		CacheWriteTokens: in.CacheWriteTokens,
		CompletionTokens: in.CompletionTokens,
		ReasoningTokens:  in.ReasoningTokens,
		TotalTokens:      in.PromptTokens + in.CacheReadTokens + in.CacheWriteTokens + in.CompletionTokens,
		CostUSD:          in.CostUSD,
		CorrelationID:    in.CorrelationID,
		TraceID:          in.TraceID,
		OccurredAt:       time.Now().UTC(),
		CreatedAt:        time.Now().UTC(),
		WorkflowRunID:    in.WorkflowRunID,
	}

	// Cost authority: if a pricing resolver is wired, the catalog price is authoritative — even when it
	// is $0 (a genuinely free model).
	var resolvedCost *apiv1.ModelCost
	if u.pricing != nil {
		cost, ok := u.pricing(context.WithoutCancel(ctx), in.Provider, in.Model)
		if ok && cost != nil {
			if p, ok := usageCostFromCatalog(in, cost); ok {
				resolvedCost = cost
				in.CostUSD = p
				row.CostUSD = p
				u.log.Debug("usage cost from catalog", "source", "catalog", "provider", in.Provider, "model", in.Model, "costUSD", p)
			}
		} else {
			u.log.Debug("usage cost from adapter", "source", "adapter", "provider", in.Provider, "model", in.Model, "costUSD", in.CostUSD)
		}
	}

	// Postgres write (source of truth). Own transaction — usage is
	// independent of the execution's state mutations (docs/08 §1).
	if u.pool != nil && in.TenantID != "" {
		ttx, err := u.pool.BeginTenantTx(ctx, in.TenantID)
		if err != nil {
			u.log.Warn("usage record: begin tx failed", "error", err)
		} else {
			persisted, err := db.CreateUsageRecord(ctx, ttx.Tx, row)
			// Commit the transaction so the row sticks.
			if err != nil {
				_ = ttx.Rollback(ctx)
				u.log.Warn("usage record: insert failed (telemetry-only)", "error", err)
			} else {
				if err := ttx.Commit(ctx); err != nil {
					u.log.Warn("usage record: commit failed", "error", err)
				}
				row = persisted
			}
		}
	}

	// OTel metrics (VictoriaMetrics half of the dual-write).
	u.metrics.emit(ctx, &row, resolvedCost)

	return row, nil
}
