package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SigNozClient queries tenant-scoped telemetry from ClickHouse directly
// (docs/07 §3.9, docs/08 §5). It bypasses the SigNoz query-service REST
// API (which changed incompatibly in v0.132) and reads ClickHouse tables
// directly via its HTTP interface (localhost:8123 by default).
//
// If ClickHouse is unreachable, query methods return a Degraded=true
// response rather than an error, so the UI degrades gracefully without
// breaking the Orchicon shell (docs/08 §8).
type SigNozClient struct {
	chURL      string
	httpClient *http.Client
}

// NewSigNozClient constructs a client. clickhouseDSN is the ClickHouse
// HTTP endpoint with credentials, e.g. "http://signoz:signoz@localhost:8123".
// An empty DSN disables queries (returns degraded empty).
func NewSigNozClient(clickhouseDSN string) *SigNozClient {
	return &SigNozClient{
		chURL:      strings.TrimRight(clickhouseDSN, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Available reports whether ClickHouse is configured.
func (c *SigNozClient) Available() bool { return c.chURL != "" }

// ----- Types -----

type TraceResult struct {
	Traces   []TraceSummary
	NextPage string
	Degraded bool
}

type TraceSummary struct {
	TraceID       string
	RootSpanName  string
	Service       string
	DurationUS    int64
	StartTime     time.Time
	SpanCount     int
	TenantID      string
	ProjectID     string
	CorrelationID string
}

type MetricResult struct {
	Series   []MetricSeriesPoint
	Degraded bool
}

type MetricSeriesPoint struct {
	Name   string
	Labels map[string]string
	Points []MetricPoint
}

type MetricPoint struct {
	Timestamp time.Time
	Value     float64
}

type LogResult struct {
	Logs     []LogEntry
	Degraded bool
}

type LogEntry struct {
	TraceID       string
	SpanID        string
	Timestamp     time.Time
	Severity      string
	Body          string
	Service       string
	TenantID      string
	CorrelationID string
}

type TraceFilter struct {
	ProjectID     string
	ExecutionID   string
	TraceID       string
	CorrelationID string
	Service       string
	Start, End    time.Time
	Limit         int
}

type MetricFilter struct {
	Names      []string
	ProjectID  string
	Start, End time.Time
}

type LogFilter struct {
	ProjectID string
	Severity  string
	Start, End time.Time
	Limit     int
}

// ----- QueryTraces -----

func (c *SigNozClient) QueryTraces(ctx context.Context, tenantID string, f TraceFilter) (TraceResult, error) {
	if !c.Available() {
		return TraceResult{Degraded: true}, nil
	}

	where := fmt.Sprintf(`resourceTagsMap['orchicon.tenant_id']='%s'`, tenantID)
	if f.ProjectID != "" {
		where += fmt.Sprintf(` AND resourceTagsMap['orchicon.project_id']='%s'`, f.ProjectID)
	}
	if f.ExecutionID != "" {
		where += fmt.Sprintf(` AND stringTagMap['orchicon.execution_id']='%s'`, f.ExecutionID)
	}
	if f.TraceID != "" {
		where += fmt.Sprintf(` AND traceID='%s'`, f.TraceID)
	}
	if f.Service != "" {
		where += fmt.Sprintf(` AND serviceName='%s'`, f.Service)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	sql := fmt.Sprintf(`
		SELECT
			traceID,
			serviceName,
			name,
			durationNano,
			timestamp
		FROM signoz_traces.signoz_index_v2
		WHERE %s
		ORDER BY timestamp DESC
		LIMIT %d
	`, where, limit)

	rows, err := c.queryClickHouse(ctx, sql)
	if err != nil {
		return TraceResult{Degraded: true}, nil
	}

	// Group rows by traceID to build summaries.
	type spanRow struct {
		TraceID     string    `json:"traceID"`
		ServiceName string    `json:"serviceName"`
		Name        string    `json:"name"`
		DurationNano uint64    `json:"durationNano"`
		Timestamp   time.Time `json:"timestamp"`
	}
	type traceAccum struct {
		Service   string
		Name      string
		Duration  int64
		StartTime time.Time
		SpanCount int
	}
	acc := make(map[string]*traceAccum)
	var order []string

	for _, row := range rows {
		var r spanRow
		if err := json.Unmarshal(row, &r); err != nil {
			continue
		}
		tid := r.TraceID
		if _, ok := acc[tid]; !ok {
			acc[tid] = &traceAccum{
				Service:   r.ServiceName,
				Name:      r.Name,
				Duration:  int64(r.DurationNano / 1000),
				StartTime: r.Timestamp,
			}
			order = append(order, tid)
		}
		a := acc[tid]
		a.SpanCount++
		if r.Timestamp.Before(a.StartTime) {
			a.StartTime = r.Timestamp
		}
		d := int64(r.DurationNano / 1000)
		if d > a.Duration {
			a.Duration = d
		}
	}

	out := TraceResult{}
	for _, tid := range order {
		a := acc[tid]
		out.Traces = append(out.Traces, TraceSummary{
			TraceID:      tid,
			RootSpanName: a.Name,
			Service:      a.Service,
			DurationUS:   a.Duration,
			StartTime:    a.StartTime,
			SpanCount:    a.SpanCount,
			TenantID:     tenantID,
		})
	}
	return out, nil
}

// ----- QueryMetrics -----

func (c *SigNozClient) QueryMetrics(ctx context.Context, tenantID string, f MetricFilter) (MetricResult, error) {
	if !c.Available() {
		return MetricResult{Degraded: true}, nil
	}

	out := MetricResult{}
	for _, name := range f.Names {
		sql := fmt.Sprintf(`
			SELECT
				timestamp,
				value
			FROM signoz_metrics.samples_v4
			WHERE metric_name = '%s'
				AND JSONExtractString(labels, 'orchicon_tenant_id') = '%s'
			ORDER BY timestamp DESC
			LIMIT 100
		`, name, tenantID)

		rows, err := c.queryClickHouse(ctx, sql)
		if err != nil {
			out.Degraded = true
			continue
		}

		type metricRow struct {
			Timestamp time.Time `json:"timestamp"`
			Value     float64   `json:"value"`
		}

		pts := make([]MetricPoint, 0, len(rows))
		for _, row := range rows {
			var r metricRow
			if err := json.Unmarshal(row, &r); err != nil {
				continue
			}
			pts = append(pts, MetricPoint{
				Timestamp: r.Timestamp,
				Value:     r.Value,
			})
		}
		if len(pts) > 0 {
			out.Series = append(out.Series, MetricSeriesPoint{
				Name:   name,
				Labels: map[string]string{"tenant_id": tenantID},
				Points: pts,
			})
		}
	}
	return out, nil
}

// ----- QueryLogs -----

func (c *SigNozClient) QueryLogs(ctx context.Context, tenantID string, f LogFilter) (LogResult, error) {
	if !c.Available() {
		return LogResult{Degraded: true}, nil
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	where := fmt.Sprintf(`resources_string['orchicon.tenant_id']='%s'`, tenantID)
	if f.Severity != "" && f.Severity != "UNSPECIFIED" {
		where += fmt.Sprintf(` AND severity_text='%s'`, f.Severity)
	}

	sql := fmt.Sprintf(`
		SELECT
			trace_id,
			span_id,
			timestamp,
			severity_text,
			body,
			resources_string['service.name'] AS service_name
		FROM signoz_logs.logs_v2
		WHERE %s
		ORDER BY timestamp DESC
		LIMIT %d
	`, where, limit)

	rows, err := c.queryClickHouse(ctx, sql)
	if err != nil {
		return LogResult{Degraded: true}, nil
	}

	type logRow struct {
		TraceID    string `json:"trace_id"`
		SpanID     string `json:"span_id"`
		Timestamp  uint64 `json:"timestamp"`
		Severity   string `json:"severity_text"`
		Body       string `json:"body"`
		Service    string `json:"service_name"`
	}

	out := LogResult{}
	for _, row := range rows {
		var r logRow
		if err := json.Unmarshal(row, &r); err != nil {
			continue
		}
		out.Logs = append(out.Logs, LogEntry{
			TraceID:   r.TraceID,
			SpanID:    r.SpanID,
			Timestamp: time.UnixMicro(int64(r.Timestamp)),
			Severity:  r.Severity,
			Body:      r.Body,
			Service:   r.Service,
			TenantID:  tenantID,
		})
	}
	return out, nil
}

// ----- ClickHouse HTTP query -----

// queryClickHouse sends a SQL query to ClickHouse via its HTTP interface
// and returns parsed JSON rows.
func (c *SigNozClient) queryClickHouse(ctx context.Context, sql string) ([]json.RawMessage, error) {
	u := c.chURL + "/?default_format=JSONEachRow"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(sql))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("clickhouse: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Each line is a JSON object (JSONEachRow format).
	var rows []json.RawMessage
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rows = append(rows, json.RawMessage(line))
	}
	return rows, sc.Err()
}
