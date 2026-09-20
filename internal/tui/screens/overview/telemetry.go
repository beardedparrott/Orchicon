package overview

// telemetry.go — the Telemetry pane's three signals.
//
// The operator's ask: the pane was "just a bunch of traces". Telemetry has three
// signals (traces, metrics, logs), each is a SECTION of the pane capped at the
// TEN most recent, and selecting a row shows that item's details in the detail
// pane. The same shape is used for Cost Explorer (top ten per group), so the two
// panes read consistently.
//
// Row IDs are prefixed so the detail dispatch can tell the signals apart:
//
//	summary                      the at-a-glance aggregate
//	<trace id>                   a trace (raw id, as before)
//	metric:<series name>         a metric series
//	log:<n>                      one log record (indexed within the fetched page)

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// signalTopN caps each section (traces / metrics / logs) at the most recent N.
const signalTopN = 10

// sectionHeader marks a section row so it is visually distinct and inert.
func sectionHeader(label string, n int) kit2.Item {
	meta := "—"
	if n > 0 {
		meta = fmt.Sprintf("%d", n)
	}
	return kit2.Item{ID: "", Title: strings.ToUpper(label), Meta: meta}
}

// fetchTelemetry builds the three signal sections, each capped at signalTopN and
// ordered most-recent-first, plus a leading SUMMARY row.
func (m *Model) fetchTelemetry(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	traces, totSpans, errSpans, degraded, err := m.tracePage(ctx, pageToken)
	if err != nil {
		return nil, "", err
	}

	var items []kit2.Item

	// SUMMARY first: the pane opens on the numbers, not the noise. Suppressed
	// when there is nothing at all so the pane's empty state can explain why.
	if len(traces) > 0 {
		meta := fmt.Sprintf("%d traces · %d spans", len(traces), totSpans)
		if errSpans > 0 {
			meta += fmt.Sprintf(" · %d errored", errSpans)
		}
		if degraded {
			meta += " · degraded"
		}
		items = append(items, kit2.Item{ID: "summary", Title: "SUMMARY", Meta: meta})
	}

	if len(traces) > 0 {
		items = append(items, sectionHeader("Traces", len(traces)))
		sorted := append([]*apiv1.Trace{}, traces...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].GetStartTime().AsTime().After(sorted[j].GetStartTime().AsTime())
		})
		for i, tr := range sorted {
			if i >= signalTopN {
				break
			}
			title := tr.GetRootSpanName()
			if title == "" {
				title = tr.GetTraceId()
			}
			errN := 0
			for _, sp := range tr.GetSpans() {
				if sp.GetStatusCode() == 2 {
					errN++
				}
			}
			meta := fmt.Sprintf("%s · %d spans · %s",
				fmtDuration(tr.GetDurationUs()), tr.GetSpanCount(),
				screenkit.FmtTime(tr.GetStartTime()))
			if errN > 0 {
				meta += fmt.Sprintf(" · %d err", errN)
			}
			items = append(items, kit2.Item{ID: tr.GetTraceId(), Title: title, Meta: meta})
		}
	}

	// METRICS: the named series over the window.
	if series, err := m.metricSeries(ctx); err == nil && len(series) > 0 {
		sorted := append([]*apiv1.MetricSeries{}, series...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].GetStart().AsTime().After(sorted[j].GetStart().AsTime())
		})
		items = append(items, sectionHeader("Metrics", len(sorted)))
		for i, s := range sorted {
			if i >= signalTopN {
				break
			}
			items = append(items, kit2.Item{
				ID:    "metric:" + s.GetMetricName(),
				Title: s.GetMetricName(),
				Meta: fmt.Sprintf("%d points · %s", len(s.GetPoints()),
					lastPointValue(s)),
			})
		}
	}

	// LOGS: the most recent records.
	if logs, err := m.logRecords(ctx); err == nil && len(logs) > 0 {
		sorted := append([]*apiv1.LogRecord{}, logs...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].GetTimestamp().AsTime().After(sorted[j].GetTimestamp().AsTime())
		})
		items = append(items, sectionHeader("Logs", len(sorted)))
		for i, l := range sorted {
			if i >= signalTopN {
				break
			}
			sev := strings.ToUpper(l.GetSeverity())
			title := firstLineOf(l.GetBody())
			if title == "" {
				title = "(empty)"
			}
			items = append(items, kit2.Item{
				ID:    fmt.Sprintf("log:%d", i),
				Title: title,
				Meta:  fmt.Sprintf("%s · %s · %s", sev, l.GetService(), screenkit.FmtTime(l.GetTimestamp())),
			})
		}
	}

	return items, "", nil
}

// tracePage fetches the trace page and its at-a-glance counters.
func (m *Model) tracePage(ctx context.Context, pageToken string) ([]*apiv1.Trace, int, int, bool, error) {
	resp, err := m.cl.Telemetry.QueryTraces(ctx, connect.NewRequest(&apiv1.QueryTracesRequest{
		Query: &apiv1.TelemetryQuery{Limit: 100, PageToken: pageToken},
	}))
	if err != nil {
		return nil, 0, 0, false, err
	}
	total, errN := 0, 0
	for _, tr := range resp.Msg.GetTraces() {
		total += int(tr.GetSpanCount())
		for _, sp := range tr.GetSpans() {
			if sp.GetStatusCode() == 2 {
				errN++
			}
		}
	}
	return resp.Msg.GetTraces(), total, errN, resp.Msg.GetDegraded(), nil
}

// metricSeries fetches the metric series over the reporting window.
func (m *Model) metricSeries(ctx context.Context) ([]*apiv1.MetricSeries, error) {
	start, end := m.costWindows()
	resp, err := m.cl.Telemetry.QueryMetrics(ctx, connect.NewRequest(&apiv1.QueryMetricsRequest{
		Query: &apiv1.TelemetryQuery{Start: tsOf(start), End: tsOf(end), Limit: 100},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSeries(), nil
}

// logRecords fetches the most recent log records.
func (m *Model) logRecords(ctx context.Context) ([]*apiv1.LogRecord, error) {
	start, end := m.costWindows()
	resp, err := m.cl.Telemetry.QueryLogs(ctx, connect.NewRequest(&apiv1.QueryLogsRequest{
		Query: &apiv1.TelemetryQuery{Start: tsOf(start), End: tsOf(end), Limit: 100},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetLogs(), nil
}

// lastPointValue describes a series' most recent sample (its headline number).
func lastPointValue(s *apiv1.MetricSeries) string {
	pts := s.GetPoints()
	if len(pts) == 0 {
		return "no points"
	}
	last := pts[0]
	for _, p := range pts {
		if p.GetTimestamp().AsTime().After(last.GetTimestamp().AsTime()) {
			last = p
		}
	}
	return fmt.Sprintf("last %g", last.GetValue())
}

// firstLineOf returns the first non-empty line of s (log bodies are multi-line).
func firstLineOf(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// metricDetail renders one metric series: identities, window, and its points.
func (m *Model) metricDetail(ctx context.Context, name string) (string, []kit2.Field, string, error) {
	series, err := m.metricSeries(ctx)
	if err != nil {
		return "", nil, "", err
	}
	for _, s := range series {
		if s.GetMetricName() != name {
			continue
		}
		fields := []kit2.Field{
			{Key: "metric", Value: s.GetMetricName()},
			{Key: "points", Value: screenkit.FmtInt(len(s.GetPoints()))},
			{Key: "window", Value: screenkit.FmtTime(s.GetStart()) + " → " + screenkit.FmtTime(s.GetEnd())},
		}
		for _, k := range sortedKeys(s.GetLabels()) {
			fields = append(fields, kit2.Field{Key: "label " + k, Value: s.GetLabels()[k]})
		}
		var b strings.Builder
		b.WriteString(theme.ListTitle.Render("POINTS") + "\n")
		pts := append([]*apiv1.MetricPoint{}, s.GetPoints()...)
		sort.SliceStable(pts, func(i, j int) bool {
			return pts[i].GetTimestamp().AsTime().After(pts[j].GetTimestamp().AsTime())
		})
		for i, p := range pts {
			if i >= 50 {
				b.WriteString("  …\n")
				break
			}
			b.WriteString(fmt.Sprintf("  %s  %g\n", screenkit.FmtTime(p.GetTimestamp()), p.GetValue()))
		}
		return "Metric " + s.GetMetricName(), fields, strings.TrimRight(b.String(), "\n"), nil
	}
	return "Metric " + name, []kit2.Field{{Key: "metric", Value: name}}, "", nil
}

// logDetail renders one log record (matched by the index the row carried).
func (m *Model) logDetail(ctx context.Context, id string) (string, []kit2.Field, string, error) {
	logs, err := m.logRecords(ctx)
	if err != nil {
		return "", nil, "", err
	}
	sort.SliceStable(logs, func(i, j int) bool {
		return logs[i].GetTimestamp().AsTime().After(logs[j].GetTimestamp().AsTime())
	})
	idx := -1
	if _, err := fmt.Sscanf(id, "log:%d", &idx); err != nil || idx < 0 || idx >= len(logs) {
		return "Log", []kit2.Field{{Key: "id", Value: id}}, "", nil
	}
	l := logs[idx]
	fields := []kit2.Field{
		{Key: "time", Value: screenkit.FmtTime(l.GetTimestamp())},
		{Key: "severity", Value: strings.ToUpper(l.GetSeverity())},
		{Key: "service", Value: l.GetService()},
		{Key: "trace", Value: l.GetTraceId()},
		{Key: "span", Value: l.GetSpanId()},
	}
	for _, k := range sortedKeys(l.GetAttributes()) {
		fields = append(fields, kit2.Field{Key: "attr " + k, Value: l.GetAttributes()[k]})
	}
	return strings.ToUpper(l.GetSeverity()) + " " + l.GetService(), fields, l.GetBody(), nil
}

// sortedKeys gives map iteration a deterministic order — see the generic
// helper in screen.go.
