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

	var conditions []string
	// NOTE: tenant_id is not stored as a span attribute yet (the OTel
	// middleware creates spans before the auth middleware resolves the
	// tenant). In single-tenant dev this is harmless; for production
	// multi-tenant support, add tenant_id as a resource attribute in
	// the OTel middleware (docs/10 §8: span enrichment).
	if f.ProjectID != "" {
		conditions = append(conditions, fmt.Sprintf(`attributes_string['orchicon.project_id']='%s'`, f.ProjectID))
	}
	if f.ExecutionID != "" {
		conditions = append(conditions, fmt.Sprintf(`attributes_string['orchicon.execution_id']='%s'`, f.ExecutionID))
	}
	if f.TraceID != "" {
		conditions = append(conditions, fmt.Sprintf(`trace_id='%s'`, f.TraceID))
	}
	if f.Service != "" {
		conditions = append(conditions, fmt.Sprintf(`serviceName='%s'`, f.Service))
	}

	where := "1=1"
	if len(conditions) > 0 {
		where = strings.Join(conditions, " AND ")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	sql := fmt.Sprintf(`
		SELECT
			trace_id,
			serviceName,
			name,
			duration_nano,
			timestamp
		FROM signoz_traces.signoz_index_v3
		WHERE %s
		ORDER BY timestamp DESC
		LIMIT %d
	`, where, limit)

	rows, err := c.queryClickHouse(ctx, sql)
	if err != nil {
		return TraceResult{Degraded: true}, nil
	}

	// Group rows by trace_id to build summaries.
	// NOTE: ClickHouse returns timestamps as "2006-01-02 15:04:05.999999"
	// (space separator, no timezone) which Go's time.Time cannot parse
	// via json.Unmarshal (expects RFC3339). Use string + manual parse.
	type spanRow struct {
		TraceID      string `json:"trace_id"`
		ServiceName  string `json:"serviceName"`
		Name         string `json:"name"`
		DurationNano uint64 `json:"duration_nano"`
		TimestampStr string `json:"timestamp"`
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
		ts, _ := parseClickHouseTS(r.TimestampStr)
		tid := r.TraceID
		if _, ok := acc[tid]; !ok {
			acc[tid] = &traceAccum{
				Service:   r.ServiceName,
				Name:      r.Name,
				Duration:  int64(r.DurationNano / 1000),
				StartTime: ts,
			}
			order = append(order, tid)
		}
		a := acc[tid]
		a.SpanCount++
		if ts.Before(a.StartTime) {
			a.StartTime = ts
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
				unix_milli,
				value
			FROM signoz_metrics.samples_v4
			WHERE metric_name = '%s'
			ORDER BY unix_milli DESC
			LIMIT 100
		`, name)

		rows, err := c.queryClickHouse(ctx, sql)
		if err != nil {
			out.Degraded = true
			continue
		}

		type metricRow struct {
			UnixMilli int64   `json:"unix_milli"`
			Value     float64 `json:"value"`
		}

		pts := make([]MetricPoint, 0, len(rows))
		for _, row := range rows {
			var r metricRow
			if err := json.Unmarshal(row, &r); err != nil {
				continue
			}
			pts = append(pts, MetricPoint{
				Timestamp: time.UnixMilli(r.UnixMilli),
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
// parseClickHouseTS parses a ClickHouse DateTime64 string ("2006-01-02 15:04:05.999999")
// which Go's json.Unmarshal cannot handle natively (it expects RFC3339).
func parseClickHouseTS(s string) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05.999999", s)
}

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
