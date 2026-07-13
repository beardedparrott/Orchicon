package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SigNozClient proxies tenant-scoped telemetry queries to the SigNoz
// query-service API (docs/07 §3.9, docs/08 §5). SigNoz reads telemetry
// from ClickHouse and exposes a REST query API. The proxy injects the
// tenant_id attribute filter so a client cannot read another tenant's
// telemetry (AGENTS.md tenant isolation).
//
// If the SigNoz backend is unreachable, query methods return a
// Degraded=true response rather than an error, so the UI degrades
// gracefully ("backend unavailable") without breaking the Orchicon
// shell (docs/08 §8: telemetry loss never blocks control flow).
type SigNozClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewSigNozClient constructs a proxy client. baseURL is the SigNoz
// query-service root (e.g. http://localhost:3301 for the dev stack).
// An empty baseURL disables proxying (queries return degraded empty).
func NewSigNozClient(baseURL string) *SigNozClient {
	return &SigNozClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Available reports whether a SigNoz backend is configured.
func (c *SigNozClient) Available() bool { return c.baseURL != "" }

// TraceResult is the projected trace query result.
type TraceResult struct {
	Traces      []TraceSummary
	NextPage    string
	Degraded    bool
}

// TraceSummary is a lightweight trace reference (the full span tree is
// fetched on demand). Joined by trace_id across the OTel pipeline
// (docs/08 §5.1).
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

// QueryTraces proxies a tenant-scoped trace search to SigNoz. The
// tenant_id filter is injected from the resolved tenant so the client
// cannot escape tenant isolation. Returns Degraded=true if the backend
// is unreachable.
func (c *SigNozClient) QueryTraces(ctx context.Context, tenantID string, f TraceFilter) (TraceResult, error) {
	if !c.Available() {
		return TraceResult{Degraded: true}, nil
	}
	// SigNoz's trace search API: /api/v1/traces with a ClickHouse SQL
	// expression. We build a minimal query that filters by the
	// orchicon tenant_id resource attribute and the optional scopes.
	q := buildTraceSQL(tenantID, f)
	params := url.Values{}
	params.Set("start", strconvFormat(f.Start))
	params.Set("end", strconvFormat(f.End))
	params.Set("limit", fmt.Sprintf("%d", f.Limit))
	params.Set("query", q)

	body, err := c.get(ctx, "/api/v1/traces?"+params.Encode())
	if err != nil {
		return TraceResult{Degraded: true}, nil
	}
	var resp struct {
		Status string                 `json:"status"`
		Data   []struct {
			TraceID    string `json:"traceID"`
			ServiceName string `json:"serviceName"`
			RootServiceName string `json:"rootServiceName"`
			RootName   string `json:"rootName"`
			Duration   int64  `json:"duration"`
			Timestamp  int64  `json:"timestamp"`
			NumSpans   int    `json:"numSpans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return TraceResult{Degraded: true}, nil
	}
	out := TraceResult{}
	for _, t := range resp.Data {
		out.Traces = append(out.Traces, TraceSummary{
			TraceID:      t.TraceID,
			RootSpanName: t.RootName,
			Service:      t.RootServiceName,
			DurationUS:   t.Duration,
			StartTime:    time.UnixMicro(t.Timestamp),
			SpanCount:    t.NumSpans,
			TenantID:     tenantID,
		})
	}
	return out, nil
}

// MetricResult is the projected metric query result.
type MetricResult struct {
	Series   []MetricSeriesPoint
	Degraded bool
}

// MetricSeriesPoint is a flattened metric series sample.
type MetricSeriesPoint struct {
	Name      string
	Labels    map[string]string
	Points    []MetricPoint
}

// MetricPoint is one sample.
type MetricPoint struct {
	Timestamp time.Time
	Value     float64
}

// QueryMetrics proxies a tenant-scoped metric query to SigNoz
// (docs/08 §5.2). Returns Degraded=true if the backend is unreachable.
func (c *SigNozClient) QueryMetrics(ctx context.Context, tenantID string, f MetricFilter) (MetricResult, error) {
	if !c.Available() {
		return MetricResult{Degraded: true}, nil
	}
	// SigNoz metric query API: /api/v1/metrics with a PromQL/ClickHouse
	// expression. We query each requested metric name with the tenant
	// label filter.
	out := MetricResult{}
	for _, name := range f.Names {
		q := fmt.Sprintf(`%s{orchicon_tenant_id="%s"}`, name, tenantID)
		params := url.Values{}
		params.Set("start", strconvFormat(f.Start))
		params.Set("end", strconvFormat(f.End))
		params.Set("query", q)
		body, err := c.get(ctx, "/api/v1/metrics?"+params.Encode())
		if err != nil {
			out.Degraded = true
			continue
		}
		var resp struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Values [][]any            `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			out.Degraded = true
			continue
		}
		for _, r := range resp.Data.Result {
			pts := make([]MetricPoint, 0, len(r.Values))
			for _, v := range r.Values {
				if len(v) < 2 {
					continue
				}
				ts, _ := v[0].(float64)
				val, _ := v[1].(string)
				var fval float64
				_, _ = fmt.Sscanf(val, "%f", &fval)
				pts = append(pts, MetricPoint{
					Timestamp: time.Unix(int64(ts), 0),
					Value:     fval,
				})
			}
			out.Series = append(out.Series, MetricSeriesPoint{
				Name:   name,
				Labels: r.Metric,
				Points: pts,
			})
		}
	}
	return out, nil
}

// LogResult is the projected log query result.
type LogResult struct {
	Logs     []LogEntry
	Degraded bool
}

// LogEntry is a projected OTel log record (docs/08 §5.3).
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

// QueryLogs proxies a tenant-scoped log query to SigNoz
// (docs/08 §5.3). Returns Degraded=true if the backend is unreachable.
func (c *SigNozClient) QueryLogs(ctx context.Context, tenantID string, f LogFilter) (LogResult, error) {
	if !c.Available() {
		return LogResult{Degraded: true}, nil
	}
	q := fmt.Sprintf(`{"query":"SELECT trace_id, span_id, timestamp, severity_text, body, service_name, attributes FROM signoz_logs WHERE attributes['orchicon.tenant_id']='%s' ORDER BY timestamp DESC LIMIT %d"}`, tenantID, f.Limit)
	body, err := c.post(ctx, "/api/v1/logs", q)
	if err != nil {
		return LogResult{Degraded: true}, nil
	}
	var resp struct {
		Status string `json:"status"`
		Data   []struct {
			TraceID  string `json:"trace_id"`
			SpanID   string `json:"span_id"`
			Timestamp int64  `json:"timestamp"`
			Severity string `json:"severity_text"`
			Body     string `json:"body"`
			Service  string `json:"service_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return LogResult{Degraded: true}, nil
	}
	out := LogResult{}
	for _, l := range resp.Data {
		out.Logs = append(out.Logs, LogEntry{
			TraceID:   l.TraceID,
			SpanID:    l.SpanID,
			Timestamp: time.UnixMicro(l.Timestamp),
			Severity:  l.Severity,
			Body:      l.Body,
			Service:   l.Service,
			TenantID:  tenantID,
		})
	}
	return out, nil
}

// TraceFilter scopes a trace query.
type TraceFilter struct {
	ProjectID     string
	ExecutionID   string
	TraceID       string
	CorrelationID string
	Service       string
	Start, End    time.Time
	Limit         int
}

// MetricFilter scopes a metric query.
type MetricFilter struct {
	Names      []string
	ProjectID  string
	Start, End time.Time
}

// LogFilter scopes a log query.
type LogFilter struct {
	ProjectID string
	Severity  string
	Start, End time.Time
	Limit     int
}

// buildTraceSQL constructs a ClickHouse trace search expression scoped
// to the tenant. The tenant_id attribute is always injected; client
// scopes are ANDed in. This is the tenant-isolation enforcement point
// for the proxy (AGENTS.md tenant isolation).
func buildTraceSQL(tenantID string, f TraceFilter) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf(`resource_attributes['orchicon.tenant_id']='%s'`, tenantID))
	if f.ProjectID != "" {
		b.WriteString(fmt.Sprintf(` AND resource_attributes['orchicon.project_id']='%s'`, f.ProjectID))
	}
	if f.ExecutionID != "" {
		b.WriteString(fmt.Sprintf(` AND span_attributes['orchicon.execution_id']='%s'`, f.ExecutionID))
	}
	if f.TraceID != "" {
		b.WriteString(fmt.Sprintf(` AND trace_id='%s'`, f.TraceID))
	}
	if f.CorrelationID != "" {
		b.WriteString(fmt.Sprintf(` AND span_attributes['orchicon.correlation_id']='%s'`, f.CorrelationID))
	}
	if f.Service != "" {
		b.WriteString(fmt.Sprintf(` AND service_name='%s'`, f.Service))
	}
	return b.String()
}

func (c *SigNozClient) get(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, "")
}

func (c *SigNozClient) post(ctx context.Context, path, body string) ([]byte, error) {
	return c.do(ctx, http.MethodPost, path, body)
}

func (c *SigNozClient) do(ctx context.Context, method, path, body string) ([]byte, error) {
	u := c.baseURL + path
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("signoz: build request: %w", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signoz: request: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("signoz: status %d", resp.StatusCode)
	}
	return out, nil
}

// strconvFormat formats a time as a Unix-second string for SigNoz APIs.
func strconvFormat(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d", t.Unix())
}
