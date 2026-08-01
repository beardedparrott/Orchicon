// Package aigateway implements the AIGatewayService Connect handler
// (docs/07_API_Specification.md §3.10, docs/01 §2: AI Gateway embedded
// in the control plane binary for v0.1) and the usage recording path.
//
// Usage is dual-written (docs/08 §1, §5.2):
//   - Postgres usage_records table is the source of truth.
//   - OTel metrics (orchicon_tokens_consumed, orchicon_cost_usd) flow
//     from the producer to the OTel collector to the Grafana stack for
//     fast telemetry queries.
//
// Cost attribution rolls up Tenant → Project → Task → Execution
// (docs/10 §11 cost explorer).
package aigateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/telemetry"
	"github.com/beardedparrott/orchicon/internal/tenant"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/attribute"
)

// Service implements the AIGatewayService Connect handler
// (apiv1connect.AIGatewayServiceHandler).
type Service struct {
	pool        *db.Pool
	log         *slog.Logger
	subscriber  eventbus.Subscriber
	metrics     *usageMetrics
	providers   []*apiv1.AIProvider
	discoverer  *ModelDiscoverer
	mcpDiscoverer *MCPDiscoverer
	apiv1connect.UnimplementedAIGatewayServiceHandler
}

var _ apiv1connect.AIGatewayServiceHandler = (*Service)(nil)

// NewService constructs an AIGatewayService handler. The providers list
// reflects the LLM providers known to the gateway (docs/01 §2). In v0.1
// the OpenCode runtime executes the actual LLM calls; the gateway
// records usage + cost from adapter telemetry.
// If discoverer is nil, ListOpenCodeModels returns Unimplemented.
// If mcpDiscoverer is nil, ListOpenCodeMCPs returns Unimplemented.
func NewService(pool *db.Pool, log *slog.Logger, sub eventbus.Subscriber, discoverer *ModelDiscoverer, mcpDiscoverer *MCPDiscoverer) *Service {
	return &Service{
		pool:          pool,
		log:           log,
		subscriber:    sub,
		metrics:       newUsageMetrics(log),
		providers:     defaultProviders(),
		discoverer:    discoverer,
		mcpDiscoverer: mcpDiscoverer,
	}
}

// ListProviders returns the LLM providers known to the gateway
// (docs/07 §3.10). Providers are not tenant-scoped in v0.1.
func (s *Service) ListProviders(ctx context.Context, req *connect.Request[apiv1.ListProvidersRequest]) (*connect.Response[apiv1.ListProvidersResponse], error) {
	return connect.NewResponse(&apiv1.ListProvidersResponse{Providers: s.providers}), nil
}

// ListOpenCodeModels enumerates all models available via the `opencode`
// CLI by shelling out to `opencode models --verbose` (docs/04 §6).
func (s *Service) ListOpenCodeModels(ctx context.Context, req *connect.Request[apiv1.ListOpenCodeModelsRequest]) (*connect.Response[apiv1.ListOpenCodeModelsResponse], error) {
	if s.discoverer == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("model discovery is not configured (set ORCHICON_OPENCODE_BIN or install opencode on PATH)"))
	}
	provider := ""
	if req.Msg.Provider != nil {
		provider = *req.Msg.Provider
	}
	models, err := s.discoverer.ListModels(ctx, provider)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list opencode models: %w", err))
	}
	return connect.NewResponse(&apiv1.ListOpenCodeModelsResponse{Models: models}), nil
}

// ListOpenCodeMCPs enumerates MCP servers configured in opencode
// by shelling out to `opencode mcp list`.
func (s *Service) ListOpenCodeMCPs(ctx context.Context, req *connect.Request[apiv1.ListOpenCodeMCPsRequest]) (*connect.Response[apiv1.ListOpenCodeMCPsResponse], error) {
	if s.mcpDiscoverer == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("MCP discovery is not configured"))
	}
	servers, err := s.mcpDiscoverer.ListMCPs(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list opencode MCPs: %w", err))
	}
	return connect.NewResponse(&apiv1.ListOpenCodeMCPsResponse{Servers: servers}), nil
}

// GetUsage returns usage records matching the tenant-scoped filter. The
// tenant_id is injected from the request context (AGENTS.md tenant
// isolation); a client-supplied tenant_id hint is ignored.
func (s *Service) GetUsage(ctx context.Context, req *connect.Request[apiv1.GetUsageRequest]) (*connect.Response[apiv1.GetUsageResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pageSize := req.Msg.PageSize
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 100
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	records, err := db.ListUsageRecords(ctx, ttx.Tx, db.ListUsageRecordsFilter{
		TenantID:    tenantID,
		ProjectID:   req.Msg.ProjectId,
		TaskID:      req.Msg.TaskId,
		ExecutionID: req.Msg.ExecutionId,
		Provider:    req.Msg.Provider,
		Model:       req.Msg.Model,
		StartTime:   tsToTime(req.Msg.Start),
		EndTime:     tsToTime(req.Msg.End),
		PageSize:    pageSize,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*apiv1.UsageRecord, 0, len(records))
	for i := range records {
		out = append(out, usageRowToProto(&records[i]))
	}
	return connect.NewResponse(&apiv1.GetUsageResponse{Records: out}), nil
}

// GetCost returns a cost roll-up at the requested drill-down level
// (docs/10 §11: Tenant → Project → Task → Execution).
func (s *Service) GetCost(ctx context.Context, req *connect.Request[apiv1.GetCostRequest]) (*connect.Response[apiv1.GetCostResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	level := rollupToLevel(req.Msg.Rollup)
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	start, end := tsToTime(req.Msg.Start), tsToTime(req.Msg.End)
	rows, err := db.GetCostRollup(ctx, ttx.Tx, tenantID, level, req.Msg.ProjectId, req.Msg.TaskId, req.Msg.ExecutionId, start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summaries := make([]*apiv1.CostSummary, 0, len(rows))
	for i := range rows {
		summaries = append(summaries, costRowToProto(&rows[i], string(level), "", start, end))
	}
	// Enrich execution-level summaries with human-readable names.
	if level == db.RollupExecution {
		execIDs := make([]string, 0, len(rows))
		for i := range rows {
			if rows[i].GroupKey != "" {
				execIDs = append(execIDs, rows[i].GroupKey)
			}
		}
		if names, err := db.ResolveExecutionDisplayNames(ctx, ttx.Tx, tenantID, execIDs); err == nil {
			for i := range summaries {
				if info, ok := names[summaries[i].GroupKey]; ok {
					if info.WorkerName != "" {
						summaries[i].DisplayName = info.WorkerName
						if info.TaskTitle != "" {
							summaries[i].DisplayName += " · " + info.TaskTitle
						}
					} else if info.TaskTitle != "" {
						summaries[i].DisplayName = info.TaskTitle
					}
				}
			}
		} else {
			s.log.Warn("resolve execution display names", "error", err)
		}
	}
	totalRow, err := db.GetCostTotal(ctx, ttx.Tx, tenantID, req.Msg.ProjectId, req.Msg.TaskId, req.Msg.ExecutionId, start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetCostResponse{
		Summaries: summaries,
		Total:     costRowToProto(&totalRow, "total", "", start, end),
	}), nil
}

// GetWorkflowCosts returns cost broken down by workflow (aggregated across
// all runs), with per-run and per-worker detail (Cost Explorer "By Workflow").
func (s *Service) GetWorkflowCosts(ctx context.Context, req *connect.Request[apiv1.GetWorkflowCostsRequest]) (*connect.Response[apiv1.GetWorkflowCostsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)
	start, end := tsToTime(req.Msg.Start), tsToTime(req.Msg.End)

	// Top level: cost grouped by workflow (all runs aggregated).
	aggregates, err := db.GetWorkflowAggregateCosts(ctx, ttx.Tx, tenantID, start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*apiv1.WorkflowCostAggregate, 0, len(aggregates))
	for i := range aggregates {
		wf := &apiv1.WorkflowCostAggregate{
			WorkflowId:     aggregates[i].WorkflowID,
			WorkflowName:   aggregates[i].WorkflowName,
			TotalCostUsd:   aggregates[i].TotalCostUSD,
			TotalTokens:    aggregates[i].TotalTokens,
			RunCount:       aggregates[i].RunCount,
			ExecutionCount: aggregates[i].ExecutionCount,
		}
		// Mid level: individual runs for this workflow.
		runs, err := db.GetWorkflowRunCosts(ctx, ttx.Tx, tenantID, aggregates[i].WorkflowID, start, end)
		if err != nil {
			s.log.Warn("workflow run costs", "workflow_id", aggregates[i].WorkflowID, "error", err)
		} else {
			for j := range runs {
				run := &apiv1.WorkflowRunCost{
					WorkflowRunId:  runs[j].WorkflowRunID,
					TotalCostUsd:   runs[j].TotalCostUSD,
					TotalTokens:    runs[j].TotalTokens,
					ExecutionCount: runs[j].ExecutionCount,
					RunStatus:      runs[j].RunStatus,
				}
				// Leaf level: per-worker cost summary within this run.
				workers, err := db.GetWorkflowWorkerCosts(ctx, ttx.Tx, tenantID, runs[j].WorkflowRunID)
				if err != nil {
					s.log.Warn("workflow worker costs", "workflow_run_id", runs[j].WorkflowRunID, "error", err)
				} else {
					for k := range workers {
						run.Workers = append(run.Workers, &apiv1.WorkflowWorkerCost{
							WorkerId:        workers[k].WorkerID,
							WorkerName:      workers[k].WorkerName,
							TotalCostUsd:    workers[k].TotalCostUSD,
							TotalTokens:     workers[k].TotalTokens,
							PromptTokens:    workers[k].PromptTokens,
							CompletionTokens: workers[k].CompletionTokens,
							ExecutionCount:  workers[k].ExecutionCount,
						})
					}
				}
				wf.Runs = append(wf.Runs, run)
			}
		}
		out = append(out, wf)
	}
	return connect.NewResponse(&apiv1.GetWorkflowCostsResponse{Workflows: out}), nil
}

// StreamUsageEvents is the server-stream RPC that fans out live usage/cost
// events from NATS to connected clients (docs/07 §4, docs/08 §5.2).
func (s *Service) StreamUsageEvents(ctx context.Context, req *connect.Request[apiv1.StreamUsageEventsRequest], stream *connect.ServerStream[apiv1.StreamUsageEventsResponse]) error {
	if s.subscriber == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("event streaming is unavailable (NATS subscriber not connected)"))
	}
	filter := "orchicon.events.usage.>"
	var fromSeq uint64
	if req.Msg.FromSequence != nil && *req.Msg.FromSequence > 0 {
		fromSeq = uint64(*req.Msg.FromSequence)
	}
	ch, err := s.subscriber.Subscribe(ctx, filter, fromSeq)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to usage events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			evt, err := parseUsageEvent(msg.Data)
			if err != nil {
				s.log.Warn("failed to parse usage event", "subject", msg.Subject, "error", err)
				continue
			}
			if req.Msg.ExecutionId != "" && evt.ExecutionId != req.Msg.ExecutionId {
				continue
			}
			if req.Msg.ProjectId != "" && evt.ProjectId != req.Msg.ProjectId {
				continue
			}
			if err := stream.Send(&apiv1.StreamUsageEventsResponse{
				Event:    evt,
				Sequence: int64(msg.Seq),
			}); err != nil {
				return err
			}
		}
	}
}

// requireTenant resolves the tenant from the request context. The
// middleware stores it; the data-access layer sets app.tenant_id per
// transaction (RLS backstop — docs/09 §8.5).
func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func rollupToLevel(r apiv1.UsageRollup) db.CostRollupLevel {
	switch r {
	case apiv1.UsageRollup_USAGE_ROLLUP_TENANT:
		return db.RollupTenant
	case apiv1.UsageRollup_USAGE_ROLLUP_PROJECT:
		return db.RollupProject
	case apiv1.UsageRollup_USAGE_ROLLUP_TASK:
		return db.RollupTask
	case apiv1.UsageRollup_USAGE_ROLLUP_EXECUTION:
		return db.RollupExecution
	case apiv1.UsageRollup_USAGE_ROLLUP_MODEL:
		return db.RollupModel
	default:
		return db.RollupTenant
	}
}

// usageMetrics holds the OTel instruments for the dual-write: token and
// cost counters that mirror each usage_records Postgres row to
// VictoriaMetrics via the OTel collector (docs/08 §5.2).
type usageMetrics struct {
	log       *slog.Logger
	tokens    otelmetric.Int64Counter
	cost      otelmetric.Float64Counter
	executions otelmetric.Int64Counter
	initOnce  sync.Once
}

func newUsageMetrics(log *slog.Logger) *usageMetrics {
	return &usageMetrics{log: log}
}

func (m *usageMetrics) ensure() {
	m.initOnce.Do(func() {
		// NOTE: no units on these instruments — the Prometheus exporter
		// appends "_<unit>" to metric names (orchicon_tokens_consumed →
		// orchicon_tokens_consumed_tokens), which would break the
		// canonical names the frontend and TelemetryService query.
		if t, err := telemetry.Meter().Int64Counter(
			"orchicon_tokens_consumed",
			otelmetric.WithDescription("Total tokens consumed per LLM call (docs/08 §5.2)"),
		); err == nil {
			m.tokens = t
		}
		if c, err := telemetry.Meter().Float64Counter(
			"orchicon_cost_usd",
			otelmetric.WithDescription("USD cost per LLM call (docs/08 §5.2)"),
		); err == nil {
			m.cost = c
		}
		if e, err := telemetry.Meter().Int64Counter(
			"orchicon_executions_total",
			otelmetric.WithDescription("Total worker executions by outcome"),
		); err == nil {
			m.executions = e
		}
	})
}

// emit records a usage sample to the OTel metrics pipeline (the
// VictoriaMetrics half of the dual-write). Best-effort: a metric error is
// logged and never blocks the control flow (docs/08 §8 invariant #5).
func (m *usageMetrics) emit(ctx context.Context, r *db.UsageRecordRow) {
	if m == nil {
		return
	}
	m.ensure()
	attrs := []attribute.KeyValue{
		attribute.String("tenant", r.TenantID),
		attribute.String("project", r.ProjectID),
		attribute.String("worker", r.WorkerID),
		attribute.String("provider", r.Provider),
		attribute.String("model", r.Model),
	}
	if m.tokens != nil {
		m.tokens.Add(ctx, r.TotalTokens, otelmetric.WithAttributes(attrs...))
	}
	if m.cost != nil {
		m.cost.Add(ctx, r.CostUSD, otelmetric.WithAttributes(attrs...))
	}
}
