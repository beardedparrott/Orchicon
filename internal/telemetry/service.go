package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/eventbus"
	"github.com/beardedparrott/orchicon/internal/tenant"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the TelemetryService Connect handler
// (apiv1connect.TelemetryServiceHandler). It proxies tenant-scoped
// queries to the telemetry backends (Tempo/Loki/VictoriaMetrics,
// docs/07 §3.9, docs/08 §5) so users explore traces/metrics/logs without
// leaving the Orchicon shell. The tenant_id filter is injected from the
// request context so a client cannot read another tenant's telemetry
// (AGENTS.md tenant isolation).
type Service struct {
	pool       *db.Pool
	query      *QueryClient
	subscriber eventbus.Subscriber
	apiv1connect.UnimplementedTelemetryServiceHandler
}

var _ apiv1connect.TelemetryServiceHandler = (*Service)(nil)

// NewService constructs a TelemetryService handler.
func NewService(pool *db.Pool, query *QueryClient, sub eventbus.Subscriber) *Service {
	return &Service{pool: pool, query: query, subscriber: sub}
}

// QueryTraces proxies a tenant-scoped trace search to Tempo.
func (s *Service) QueryTraces(ctx context.Context, req *connect.Request[apiv1.QueryTracesRequest]) (*connect.Response[apiv1.QueryTracesResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	f := traceFilterFromQuery(req.Msg.GetQuery())
	res, _ := s.query.QueryTraces(ctx, tenantID, f)
	out := &apiv1.QueryTracesResponse{Degraded: res.Degraded, DegradedReason: res.Reason}
	for _, t := range res.Traces {
		out.Traces = append(out.Traces, &apiv1.Trace{
			TraceId:      t.TraceID,
			RootSpanName: t.RootSpanName,
			StartTime:    timestamppb.New(t.StartTime),
			DurationUs:   t.DurationUS,
			SpanCount:    int32(t.SpanCount),
			RootAttributes: map[string]string{
				"tenant_id":      t.TenantID,
				"project_id":     t.ProjectID,
				"correlation_id": t.CorrelationID,
			},
		})
	}
	return connect.NewResponse(out), nil
}

// QueryMetrics proxies a tenant-scoped metric query to VictoriaMetrics.
func (s *Service) QueryMetrics(ctx context.Context, req *connect.Request[apiv1.QueryMetricsRequest]) (*connect.Response[apiv1.QueryMetricsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	q := req.Msg.GetQuery()
	f := MetricFilter{
		ProjectID:  q.GetProjectId(),
		WorkItemID: q.GetWorkItemId(),
		Start:      tsToTime(q.GetStart()),
		End:        tsToTime(q.GetEnd()),
		Names:      req.Msg.GetMetricNames(),
	}
	res, _ := s.query.QueryMetrics(ctx, tenantID, f)
	out := &apiv1.QueryMetricsResponse{Degraded: res.Degraded, DegradedReason: res.Reason}
	for _, ser := range res.Series {
		series := &apiv1.MetricSeries{
			MetricName: ser.Name,
			Labels:     ser.Labels,
			Start:      timestamppb.New(tsToTime(q.GetStart())),
			End:        timestamppb.New(tsToTime(q.GetEnd())),
		}
		for _, p := range ser.Points {
			series.Points = append(series.Points, &apiv1.MetricPoint{
				MetricName: ser.Name,
				Timestamp:  timestamppb.New(p.Timestamp),
				Value:      p.Value,
				Labels:     ser.Labels,
			})
		}
		out.Series = append(out.Series, series)
	}
	return connect.NewResponse(out), nil
}

// QueryLogs proxies a tenant-scoped log query to Loki.
func (s *Service) QueryLogs(ctx context.Context, req *connect.Request[apiv1.QueryLogsRequest]) (*connect.Response[apiv1.QueryLogsResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	q := req.Msg.GetQuery()
	f := LogFilter{
		ProjectID:   q.GetProjectId(),
		ExecutionID: q.GetExecutionId(),
		WorkItemID:  q.GetWorkItemId(),
		Severity:    req.Msg.GetSeverity(),
		Search:      req.Msg.GetSearch(),
		Start:       tsToTime(q.GetStart()),
		End:         tsToTime(q.GetEnd()),
		Limit:       boundLimit(q.GetLimit()),
	}
	res, _ := s.query.QueryLogs(ctx, tenantID, f)
	out := &apiv1.QueryLogsResponse{Degraded: res.Degraded, DegradedReason: res.Reason}
	for _, l := range res.Logs {
		out.Logs = append(out.Logs, &apiv1.LogRecord{
			TraceId:   l.TraceID,
			SpanId:    l.SpanID,
			Timestamp: timestamppb.New(l.Timestamp),
			Severity:  l.Severity,
			Body:      l.Body,
			Service:   l.Service,
			Attributes: map[string]string{
				"tenant_id": l.TenantID,
			},
		})
	}
	return connect.NewResponse(out), nil
}

// StreamTelemetry streams live telemetry updates (usage/cost events +
// metric samples) from NATS to connected clients (docs/07 §4, docs/10
// §4). The stream fans out orchicon.events.usage.> events.
func (s *Service) StreamTelemetry(ctx context.Context, req *connect.Request[apiv1.StreamTelemetryRequest], stream *connect.ServerStream[apiv1.StreamTelemetryResponse]) error {
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
		return connect.NewError(connect.CodeInternal, fmt.Errorf("subscribe to telemetry events: %w", err))
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			evt, err := parseUsageEventProto(msg.Data)
			if err != nil {
				continue
			}
			if req.Msg.ExecutionId != "" && evt.ExecutionId != req.Msg.ExecutionId {
				continue
			}
			if req.Msg.ProjectId != "" && evt.ProjectId != req.Msg.ProjectId {
				continue
			}
			if err := stream.Send(&apiv1.StreamTelemetryResponse{
				Update:   &apiv1.StreamTelemetryResponse_Usage{Usage: evt},
				Sequence: int64(msg.Seq),
			}); err != nil {
				return err
			}
		}
	}
}

// GetDashboard returns an Orchicon-specific telemetry dashboard: a
// curated roll-up built custom on the Orchicon domain model (cost,
// executions) — raw exploration uses the Telemetry page
// (docs/10 §11). Reads Postgres (usage_records — source of truth).
func (s *Service) GetDashboard(ctx context.Context, req *connect.Request[apiv1.GetDashboardRequest]) (*connect.Response[apiv1.GetDashboardResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The default window is all-time: a zero start flows to the SQL
	// unbounded sentinel (`$n::timestamptz <= 'epoch'::timestamptz`),
	// exactly like AIGateway.GetCost, so the Overview total matches the
	// Cost Explorer's per-model sum. Only `end` defaults to now so a nil
	// end still means "up to now".
	start, end := tsToTime(req.Msg.Start), tsToTime(req.Msg.End)
	if end.IsZero() {
		end = time.Now()
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	total, err := db.GetCostTotal(ctx, ttx.Tx, tenantID, req.Msg.ProjectId, "", "", start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	modelRollup, err := db.GetCostRollup(ctx, ttx.Tx, tenantID, db.RollupModel, req.Msg.ProjectId, "", "", start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	summary := &apiv1.DashboardSummary{
		TotalTokens:     total.TotalTokens,
		TotalCostUsd:    total.CostUSD,
		TotalExecutions: total.ExecutionCount,
		WindowStart:     timestamppb.New(start),
		WindowEnd:       timestamppb.New(end),
	}
	out := &apiv1.GetDashboardResponse{Summary: summary}
	endTs := timestamppb.New(end)
	for i := range modelRollup {
		ser := &apiv1.MetricSeries{
			MetricName: "orchicon_cost_usd",
			Labels:     map[string]string{"model": modelRollup[i].GroupKey},
			End:        endTs,
		}
		ser.Points = append(ser.Points, &apiv1.MetricPoint{
			MetricName: "orchicon_cost_usd",
			Timestamp:  endTs,
			Value:      modelRollup[i].CostUSD,
			Labels:     map[string]string{"model": modelRollup[i].GroupKey},
		})
		out.Panels = append(out.Panels, ser)
	}
	return connect.NewResponse(out), nil
}

// GetTelemetryReport returns a summarised window over the Orchicon domain
// model: token usage, cost, duration and failure rate per project or per
// workflow. It reads Postgres only (usage_records + worker_executions +
// workflow_runs — the source of truth), so reports keep working when a
// telemetry store is down. That is what makes the UI's "partial" state
// truthful: graphs go degraded, the report does not.
//
// The window is explicit on purpose: a zero start means "24h before end"
// (never "all time"), so window_start/window_end in the response are always
// meaningful for the caller.
func (s *Service) GetTelemetryReport(ctx context.Context, req *connect.Request[apiv1.GetTelemetryReportRequest]) (*connect.Response[apiv1.GetTelemetryReportResponse], error) {
	tenantID, err := requireTenant(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	start, end := tsToTime(req.Msg.GetStart()), tsToTime(req.Msg.GetEnd())
	if end.IsZero() {
		end = time.Now()
	}
	if start.IsZero() {
		start = end.Add(-24 * time.Hour)
	}
	ttx, err := s.pool.BeginTenantTx(ctx, tenantID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer ttx.Rollback(ctx)

	rows := map[string]*apiv1.TelemetryReportRow{}
	order := make([]string, 0, 16)
	ensure := func(key, name string) *apiv1.TelemetryReportRow {
		if r, ok := rows[key]; ok {
			if r.GroupName == "" && name != "" {
				r.GroupName = name
			}
			return r
		}
		r := &apiv1.TelemetryReportRow{GroupKey: key, GroupName: name}
		rows[key] = r
		order = append(order, key)
		return r
	}

	projectID := req.Msg.GetProjectId()
	executionID := req.Msg.GetExecutionId()
	workItemID := req.Msg.GetWorkItemId()

	switch req.Msg.GetGroupBy() {
	case apiv1.TelemetryReportGroupBy_TELEMETRY_REPORT_GROUP_BY_WORKFLOW:
		// Cost and duration are scoped identically: a workflow row that
		// answered the project filter with project-scoped run times but
		// tenant-wide spend would be quietly wrong.
		costs, err := db.GetWorkflowAggregateCosts(ctx, ttx.Tx, tenantID, projectID, workItemID, executionID, start, end)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, c := range costs {
			r := ensure(c.WorkflowID, c.WorkflowName)
			r.TotalTokens += c.TotalTokens
			r.PromptTokens += c.PromptTokens
			r.CacheReadTokens += c.CacheReadTokens
			r.CostUsd += c.TotalCostUSD
			r.Executions += c.ExecutionCount
		}
		stats, err := db.GetWorkflowDurationStats(ctx, ttx.Tx, tenantID, projectID, workItemID, executionID, start, end)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, st := range stats {
			r := ensure(st.GroupKey, st.GroupName)
			// For workflows the run count IS the fail-rate denominator, so it
			// lands in Runs while Executions stays the usage-record count.
			r.Runs += st.Executions
			r.FailedRuns += st.Failed
			r.DurationMsTotal += st.DurationMsTotal
		}

	default: // project grouping
		costs, err := db.GetCostRollup(ctx, ttx.Tx, tenantID, db.RollupProject, projectID, workItemID, executionID, start, end)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		names, err := db.GetProjectNames(ctx, ttx.Tx, tenantID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, c := range costs {
			r := ensure(c.GroupKey, names[c.GroupKey])
			r.TotalTokens += c.TotalTokens
			r.PromptTokens += c.PromptTokens
			r.CacheReadTokens += c.CacheReadTokens
			r.CostUsd += c.CostUSD
		}
		stats, err := db.GetProjectDurationStats(ctx, ttx.Tx, tenantID, projectID, executionID, workItemID, start, end)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, st := range stats {
			r := ensure(st.GroupKey, names[st.GroupKey])
			r.Executions += st.Executions
			r.FailedRuns += st.Failed
			r.DurationMsTotal += st.DurationMsTotal
		}
	}

	totals := &apiv1.TelemetryReportTotals{}
	out := &apiv1.GetTelemetryReportResponse{
		WindowStart: timestamppb.New(start),
		WindowEnd:   timestamppb.New(end),
	}
	for _, key := range order {
		r := rows[key]
		r.DurationMsAvg = avgOrZero(r.DurationMsTotal, failRateDenominator(r.GetRuns(), r.GetExecutions()))
		totals.TotalTokens += r.TotalTokens
		totals.CostUsd += r.CostUsd
		totals.Executions += r.Executions
		totals.Runs += r.Runs
		totals.FailedRuns += r.FailedRuns
		totals.DurationMsTotal += r.DurationMsTotal
		out.Rows = append(out.Rows, r)
	}
	totals.DurationMsAvg = avgOrZero(totals.DurationMsTotal, failRateDenominator(totals.Runs, totals.Executions))
	if d := failRateDenominator(totals.Runs, totals.Executions); d > 0 {
		totals.FailureRate = float64(totals.FailedRuns) / float64(d)
	}
	out.Totals = totals

	// Outcomes and models are answered independently of the grouping: "how
	// much work finished, and how" is the same question whether the table is
	// grouped by project or by workflow. They read different tables from the
	// rows above, so a failure here fails the report rather than quietly
	// returning zeroes an operator would read as "nothing finished".
	outcomes, err := db.GetOutcomeCounts(ctx, ttx.Tx, tenantID, projectID, workItemID, start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out.Outcomes = &apiv1.TelemetryReportOutcomes{
		WorkSucceeded:      outcomes.WorkSucceeded,
		WorkSkipped:        outcomes.WorkSkipped,
		WorkCancelled:      outcomes.WorkCancelled,
		WorkFailed:         outcomes.WorkFailed,
		WorkOpen:           outcomes.WorkOpen,
		WorkflowsCompleted: outcomes.WorkflowsCompleted,
		WorkflowsFailed:    outcomes.WorkflowsFailed,
		WorkflowsAborted:   outcomes.WorkflowsAborted,
		WorkflowsRunning:   outcomes.WorkflowsRunning,
	}

	// The model leaderboard is the existing per-model cost roll-up, scoped by
	// the SAME filters as the rows so the two halves of one screen cannot
	// disagree: a model total that ignored the project filter would answer a
	// different question than the table beside it.
	models, err := db.GetCostRollup(ctx, ttx.Tx, tenantID, db.RollupModel, projectID, workItemID, executionID, start, end)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, m := range models {
		out.Models = append(out.Models, &apiv1.TelemetryReportModel{
			Model:           m.GroupKey,
			TotalTokens:     m.TotalTokens,
			PromptTokens:    m.PromptTokens,
			CacheReadTokens: m.CacheReadTokens,
			CostUsd:         m.CostUSD,
			Executions:      m.ExecutionCount,
		})
	}
	return connect.NewResponse(out), nil
}

// failRateDenominator picks how many finished units a failure rate (and the
// average duration) divides by: workflow runs when the grouping produced
// them, otherwise executions. Zero means "nothing finished" → rate 0.
func failRateDenominator(runs, executions int32) int32 {
	if runs > 0 {
		return runs
	}
	return executions
}

func avgOrZero(total int64, n int32) int64 {
	if n <= 0 {
		return 0
	}
	return total / int64(n)
}

func requireTenant(ctx context.Context) (string, error) {
	id := tenant.FromContext(ctx)
	if id == "" {
		return "", errors.New("no tenant in context")
	}
	return id, nil
}

func traceFilterFromQuery(q *apiv1.TelemetryQuery) TraceFilter {
	return TraceFilter{
		ProjectID:     q.GetProjectId(),
		ExecutionID:   q.GetExecutionId(),
		WorkItemID:    q.GetWorkItemId(),
		TraceID:       q.GetTraceId(),
		CorrelationID: q.GetCorrelationId(),
		Service:       q.GetService(),
		Start:         tsToTime(q.GetStart()),
		End:           tsToTime(q.GetEnd()),
		Limit:         boundLimit(q.GetLimit()),
	}
}

func boundLimit(n int32) int {
	if n <= 0 || n > 500 {
		return 100
	}
	return int(n)
}

func tsToTime(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// parseUsageEventProto decodes a NATS usage event payload (JSON) into the
// wire UsageEvent proto. Mirrors aigateway.parseUsageEvent.
func parseUsageEventProto(data []byte) (*apiv1.UsageEvent, error) {
	if len(data) == 0 {
		return nil, errors.New("empty usage event payload")
	}
	var raw struct {
		ID               string  `json:"id"`
		TenantID         string  `json:"tenant_id"`
		ProjectID        string  `json:"project_id"`
		TaskID           string  `json:"task_id"`
		ExecutionID      string  `json:"execution_id"`
		WorkerID         string  `json:"worker_id"`
		Provider         string  `json:"provider"`
		Model            string  `json:"model"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		TotalTokens      int64   `json:"total_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		CorrelationID    string  `json:"correlation_id"`
		OccurredAt       string  `json:"occurred_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	evt := &apiv1.UsageEvent{
		Id:               raw.ID,
		TenantId:         raw.TenantID,
		ProjectId:        raw.ProjectID,
		TaskId:           raw.TaskID,
		ExecutionId:      raw.ExecutionID,
		WorkerId:         raw.WorkerID,
		Provider:         raw.Provider,
		Model:            raw.Model,
		PromptTokens:     raw.PromptTokens,
		CompletionTokens: raw.CompletionTokens,
		TotalTokens:      raw.TotalTokens,
		CostUsd:          raw.CostUSD,
		CorrelationId:    raw.CorrelationID,
	}
	if raw.OccurredAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw.OccurredAt); err == nil {
			evt.OccurredAt = timestamppb.New(t)
		}
	}
	return evt, nil
}
