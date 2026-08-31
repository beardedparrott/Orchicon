package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// QueryClient queries tenant-scoped telemetry from the Grafana stack:
// Tempo (traces), Loki (logs), VictoriaMetrics (metrics). It talks to the
// backends' native HTTP APIs directly — Grafana itself is only the UI.
//
// If a backend is unreachable, the corresponding query method returns a
// Degraded=true response rather than an error, so the UI degrades
// gracefully without breaking the Orchicon shell (docs/08 §8).
type QueryClient struct {
	tempoURL   string
	lokiURL    string
	vmURL      string
	httpClient *http.Client
}

// NewQueryClient constructs a query client. tempoURL/lokiURL/vmURL are the
// backend HTTP endpoints (e.g. "http://localhost:3200"). An empty URL
// disables the corresponding query (returns degraded empty).
func NewQueryClient(tempoURL, lokiURL, vmURL string) *QueryClient {
	return &QueryClient{
		tempoURL:   strings.TrimRight(tempoURL, "/"),
		lokiURL:    strings.TrimRight(lokiURL, "/"),
		vmURL:      strings.TrimRight(vmURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Available reports whether any backend is configured.
func (c *QueryClient) Available() bool {
	return c.tempoURL != "" || c.lokiURL != "" || c.vmURL != ""
}

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

// ----- QueryTraces (Tempo /api/search) -----

// QueryTraces searches Tempo for traces matching the filter. Tags map to
// span/resource attribute filters. The tenant filter is best-effort:
// control-plane spans are created before the auth middleware resolves the
// tenant, so orchicon.tenant_id is not present on every trace (docs/10 §8).
func (c *QueryClient) QueryTraces(ctx context.Context, tenantID string, f TraceFilter) (TraceResult, error) {
	if c.tempoURL == "" {
		return TraceResult{Degraded: true}, nil
	}

	end := f.End
	if end.IsZero() {
		end = time.Now()
	}
	start := f.Start
	if start.IsZero() {
		start = end.Add(-1 * time.Hour)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	q := url.Values{}
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("limit", strconv.Itoa(limit))

	tags := map[string]string{}
	if f.ProjectID != "" {
		tags["orchicon.project_id"] = f.ProjectID
	}
	if f.ExecutionID != "" {
		tags["orchicon.execution_id"] = f.ExecutionID
	}
	if f.TraceID != "" {
		tags["traceID"] = f.TraceID
	}
	if f.Service != "" {
		tags["service.name"] = f.Service
	}
	if len(tags) > 0 {
		tb, _ := json.Marshal(tags)
		q.Set("tags", string(tb))
	}

	body, status, err := c.get(ctx, c.tempoURL+"/api/search?"+q.Encode())
	if err != nil || status >= 400 {
		return TraceResult{Degraded: true}, nil
	}

	var resp struct {
		Traces []struct {
			TraceID           string `json:"traceID"`
			RootServiceName   string `json:"rootServiceName"`
			RootTraceName     string `json:"rootTraceName"`
			StartTimeUnixNano string `json:"startTimeUnixNano"`
			DurationMs        int64  `json:"durationMs"`
			SpanSets          []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"spanSets"`
		} `json:"traces"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return TraceResult{Degraded: true}, nil
	}

	out := TraceResult{}
	for _, t := range resp.Traces {
		tsNano, _ := strconv.ParseInt(t.StartTimeUnixNano, 10, 64)
		spanCount := 0
		for _, ss := range t.SpanSets {
			spanCount += len(ss.Spans)
		}
		out.Traces = append(out.Traces, TraceSummary{
			TraceID:      t.TraceID,
			RootSpanName: t.RootTraceName,
			Service:      t.RootServiceName,
			DurationUS:   t.DurationMs * 1000,
			StartTime:    time.Unix(0, tsNano),
			SpanCount:    spanCount,
			TenantID:     tenantID,
		})
	}
	return out, nil
}

// ----- QueryMetrics (VictoriaMetrics /api/v1/query_range) -----

// tokenClassSplitMetrics are the OTel counter families that now carry a
// token_class label (prompt/cache_read/cache_write/completion) per the
// cache-aware counters ADR
// (architecture-notes/otel-metrics-cache-aware-token-cost-counters.md). A
// bare label select on these names matches up to four series (one per class),
// so a total/deterministic read must re-aggregate across classes. We wrap the
// selector in sum(...) to collapse them back into a single total series, which
// preserves the pre-split total semantics while the per-class breakdown stays
// available to Grafana via sum by (token_class). This is the only safe form
// for consumers that read a single latest point (the telemetry "Metric values"
// panel) — otherwise they would report one arbitrary bucket instead of the
// total.
var tokenClassSplitMetrics = map[string]bool{
	"orchicon_tokens_consumed": true,
	"orchicon_cost_usd":        true,
}

// QueryMetrics runs one PromQL range query per metric name against
// VictoriaMetrics. Resource attributes are added as series labels by the
// collector (resource_to_telemetry_conversion), so the tenant filter is
// a label matcher on orchicon_tenant_id. Points are returned newest-first.
func (c *QueryClient) QueryMetrics(ctx context.Context, tenantID string, f MetricFilter) (MetricResult, error) {
	if c.vmURL == "" {
		return MetricResult{Degraded: true}, nil
	}

	end := f.End
	if end.IsZero() {
		end = time.Now()
	}
	start := f.Start
	if start.IsZero() {
		start = end.Add(-24 * time.Hour)
	}
	step := end.Sub(start) / 200
	if step < 30*time.Second {
		step = 30 * time.Second
	}

	out := MetricResult{}
	for _, name := range f.Names {
		if name == "" {
			continue
		}
		selector := fmt.Sprintf("%s{orchicon_tenant_id=%q}", name, tenantID)
		if f.ProjectID != "" {
			selector = fmt.Sprintf("%s{orchicon_tenant_id=%q,project=%q}", name, tenantID, f.ProjectID)
		}
		if tokenClassSplitMetrics[name] {
			selector = fmt.Sprintf("sum(%s)", selector)
		}
		q := url.Values{}
		q.Set("query", selector)
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("step", fmt.Sprintf("%ds", int(step.Seconds())))

		body, status, err := c.get(ctx, c.vmURL+"/api/v1/query_range?"+q.Encode())
		if err != nil || status >= 400 {
			out.Degraded = true
			continue
		}

		var resp struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Metric map[string]string   `json:"metric"`
					Values [][]json.RawMessage `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			out.Degraded = true
			continue
		}

		var allPts []MetricPoint
		for _, res := range resp.Data.Result {
			for _, v := range res.Values {
				if len(v) < 2 {
					continue
				}
				ts, _ := strconv.ParseInt(string(v[0]), 10, 64)
				var vs string
				if err := json.Unmarshal(v[1], &vs); err != nil {
					continue
				}
				val, err := strconv.ParseFloat(vs, 64)
				if err != nil {
					continue
				}
				allPts = append(allPts, MetricPoint{Timestamp: time.Unix(ts, 0), Value: val})
			}
		}
		if len(allPts) > 0 {
			// Newest-first to match the frontend's "latest sample" read.
			for i, j := 0, len(allPts)-1; i < j; i, j = i+1, j-1 {
				allPts[i], allPts[j] = allPts[j], allPts[i]
			}
			out.Series = append(out.Series, MetricSeriesPoint{
				Name:   name,
				Labels: map[string]string{"tenant_id": tenantID},
				Points: allPts,
			})
		}
	}
	return out, nil
}

// ----- QueryLogs (Loki /loki/api/v1/query_range) -----

// QueryLogs runs a LogQL stream-selector query against Loki. The OTLP
// endpoint maps resource attributes to stream labels (orchicon_tenant_id,
// service_name) and the OTel severity_number to a label (there is no
// `level` label unless the record carries severity_text); trace_id/span_id
// and record attributes arrive as structured metadata on each entry
// (docs/08 §5.3).
func (c *QueryClient) QueryLogs(ctx context.Context, tenantID string, f LogFilter) (LogResult, error) {
	if c.lokiURL == "" {
		return LogResult{Degraded: true}, nil
	}

	end := f.End
	if end.IsZero() {
		end = time.Now()
	}
	start := f.Start
	if start.IsZero() {
		start = end.Add(-24 * time.Hour)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	selector := fmt.Sprintf(`{orchicon_tenant_id=%q}`, tenantID)
	if sev := severityNumber(f.Severity); sev != "" {
		selector = fmt.Sprintf(`{orchicon_tenant_id=%q,severity_number=%q}`, tenantID, sev)
	}

	q := url.Values{}
	q.Set("query", selector)
	q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("direction", "backward")

	body, status, err := c.get(ctx, c.lokiURL+"/loki/api/v1/query_range?"+q.Encode())
	if err != nil || status >= 400 {
		return LogResult{Degraded: true}, nil
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string   `json:"stream"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return LogResult{Degraded: true}, nil
	}

	out := LogResult{}
	for _, res := range resp.Data.Result {
		service := res.Stream["service_name"]
		severity := severityText(res.Stream["severity_number"])
		for _, v := range res.Values {
			if len(v) < 2 {
				continue
			}
			ts, _ := strconv.ParseInt(string(v[0]), 10, 64)
			var line string
			if err := json.Unmarshal(v[1], &line); err != nil {
				continue
			}
			entry := LogEntry{
				Timestamp: time.Unix(0, ts),
				Severity:  severity,
				Body:      line,
				Service:   service,
				TenantID:  tenantID,
			}
			// Loki 3.x returns structured metadata as a third element
			// ([ts, line, {metadata}]) when present.
			if len(v) >= 3 {
				var sm map[string]json.RawMessage
				if err := json.Unmarshal(v[2], &sm); err == nil {
					entry.TraceID = rawString(sm["trace_id"])
					entry.SpanID = rawString(sm["span_id"])
					entry.CorrelationID = rawString(sm["correlation_id"])
				}
			}
			out.Logs = append(out.Logs, entry)
		}
	}
	return out, nil
}

// severityNumber maps an OTel severity text (as passed by the frontend:
// "ERROR", "INFO", ...) to the OTel severity_number value that Loki
// stores as the severity_number label. Empty when the filter is unset or
// unrecognised.
func severityNumber(severity string) string {
	switch strings.ToUpper(severity) {
	case "TRACE":
		return "1"
	case "DEBUG":
		return "5"
	case "INFO":
		return "9"
	case "WARN", "WARNING":
		return "13"
	case "ERROR":
		return "17"
	case "FATAL":
		return "21"
	default:
		return ""
	}
}

// severityText maps a Loki severity_number label back to an OTel severity
// text for display. Defaults to INFO for the unset case.
func severityText(num string) string {
	switch num {
	case "1", "2", "3", "4":
		return "TRACE"
	case "5", "6", "7", "8":
		return "DEBUG"
	case "13", "14", "15", "16":
		return "WARN"
	case "17", "18", "19", "20":
		return "ERROR"
	case "21", "22", "23", "24":
		return "FATAL"
	default:
		return "INFO"
	}
}

// ----- HTTP helpers -----

func (c *QueryClient) get(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

// rawString extracts a string from a json.RawMessage, tolerating both
// JSON strings and raw values.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
