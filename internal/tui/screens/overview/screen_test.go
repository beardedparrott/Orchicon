package overview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// ---- stubbed plane services ------------------------------------------

type fakeExec struct {
	apiv1connect.UnimplementedExecutionServiceHandler
	want []*apiv1.WorkerExecution
}

func (f *fakeExec) ListExecutions(context.Context, *connect.Request[apiv1.ListExecutionsRequest]) (*connect.Response[apiv1.ListExecutionsResponse], error) {
	return connect.NewResponse(&apiv1.ListExecutionsResponse{Executions: f.want}), nil
}

type fakeWorkItems struct {
	apiv1connect.UnimplementedWorkItemServiceHandler
	want []*apiv1.WorkItem
}

func (f *fakeWorkItems) ListWorkItems(context.Context, *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{WorkItems: f.want}), nil
}

type fakeWorkers struct {
	apiv1connect.UnimplementedWorkerServiceHandler
	want []*apiv1.Worker
}

func (f *fakeWorkers) ListWorkers(context.Context, *connect.Request[apiv1.ListWorkersRequest]) (*connect.Response[apiv1.ListWorkersResponse], error) {
	return connect.NewResponse(&apiv1.ListWorkersResponse{Workers: f.want}), nil
}

type fakeImages struct {
	apiv1connect.UnimplementedRuntimeImageServiceHandler
	want []*apiv1.RuntimeImage
}

func (f *fakeImages) ListRuntimeImages(context.Context, *connect.Request[apiv1.ListRuntimeImagesRequest]) (*connect.Response[apiv1.ListRuntimeImagesResponse], error) {
	return connect.NewResponse(&apiv1.ListRuntimeImagesResponse{RuntimeImages: f.want}), nil
}

type fakeTelemetry struct {
	apiv1connect.UnimplementedTelemetryServiceHandler
	traces []*apiv1.Trace
	live   bool
}

func (f *fakeTelemetry) QueryTraces(_ context.Context, req *connect.Request[apiv1.QueryTracesRequest]) (*connect.Response[apiv1.QueryTracesResponse], error) {
	id := req.Msg.GetQuery().GetTraceId()
	out := &apiv1.QueryTracesResponse{}
	for _, t := range f.traces {
		if id == "" || t.GetTraceId() == id {
			out.Traces = append(out.Traces, t)
		}
	}
	return connect.NewResponse(out), nil
}

func (f *fakeTelemetry) StreamTelemetry(ctx context.Context, _ *connect.Request[apiv1.StreamTelemetryRequest], s *connect.ServerStream[apiv1.StreamTelemetryResponse]) error {
	if f.live {
		if err := s.Send(&apiv1.StreamTelemetryResponse{
			Update:   &apiv1.StreamTelemetryResponse_Usage{Usage: &apiv1.UsageEvent{Id: "u1", Provider: "anthropic"}},
			Sequence: 1,
		}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

type fakeGateway struct {
	apiv1connect.UnimplementedAIGatewayServiceHandler
	records []*apiv1.UsageRecord
}

func (f *fakeGateway) GetUsage(context.Context, *connect.Request[apiv1.GetUsageRequest]) (*connect.Response[apiv1.GetUsageResponse], error) {
	return connect.NewResponse(&apiv1.GetUsageResponse{Records: f.records}), nil
}

// GetCost stands in for the backend's server-side roll-up: the grand total plus
// one summary per group at the requested level, derived from the same records.
func (f *fakeGateway) GetCost(_ context.Context, req *connect.Request[apiv1.GetCostRequest]) (*connect.Response[apiv1.GetCostResponse], error) {
	total := costTotal(f.records)
	var sums []*apiv1.CostSummary
	switch req.Msg.GetRollup() {
	case apiv1.UsageRollup_USAGE_ROLLUP_MODEL:
		sums = costGroups(f.records, "model")
	case apiv1.UsageRollup_USAGE_ROLLUP_PROJECT:
		sums = costGroups(f.records, "project")
	}
	return connect.NewResponse(&apiv1.GetCostResponse{Summaries: sums, Total: total}), nil
}

// GetWorkflowCosts returns one aggregate per worker (a stand-in for the
// per-workflow view; the screen only renders the numbers it is given).
func (f *fakeGateway) GetWorkflowCosts(context.Context, *connect.Request[apiv1.GetWorkflowCostsRequest]) (*connect.Response[apiv1.GetWorkflowCostsResponse], error) {
	byName := map[string]*apiv1.WorkflowCostAggregate{}
	var order []string
	for _, r := range f.records {
		w, ok := byName[r.GetWorkerName()]
		if !ok {
			w = &apiv1.WorkflowCostAggregate{WorkflowId: "wf-" + r.GetWorkerName(), WorkflowName: r.GetWorkerName()}
			byName[r.GetWorkerName()] = w
			order = append(order, r.GetWorkerName())
		}
		w.TotalCostUsd += r.GetCostUsd()
		w.TotalTokens += r.GetTotalTokens()
		w.RunCount++
	}
	out := make([]*apiv1.WorkflowCostAggregate, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return connect.NewResponse(&apiv1.GetWorkflowCostsResponse{Workflows: out}), nil
}

// costTotal sums the window's usage into one summary.
func costTotal(records []*apiv1.UsageRecord) *apiv1.CostSummary {
	s := &apiv1.CostSummary{GroupBy: "tenant", GroupKey: "tenant"}
	for _, r := range records {
		s.TotalTokens += r.GetTotalTokens()
		s.PromptTokens += r.GetPromptTokens()
		s.CompletionTokens += r.GetCompletionTokens()
		s.CacheReadTokens += r.GetCacheReadTokens()
		s.CacheWriteTokens += r.GetCacheWriteTokens()
		s.CostUsd += r.GetCostUsd()
		s.RecordCount++
		s.ExecutionCount++
	}
	return s
}

// costGroups rolls the records up by model or project.
func costGroups(records []*apiv1.UsageRecord, level string) []*apiv1.CostSummary {
	key := func(r *apiv1.UsageRecord) string {
		if level == "model" {
			return r.GetModel()
		}
		if r.GetProjectId() == "" {
			return "(unassigned)"
		}
		return r.GetProjectId()
	}
	seen := map[string]int{}
	var out []*apiv1.CostSummary
	for _, r := range records {
		k := key(r)
		i, ok := seen[k]
		if !ok {
			out = append(out, &apiv1.CostSummary{GroupBy: level, GroupKey: k, DisplayName: k})
			i = len(out) - 1
			seen[k] = i
		}
		s := out[i]
		s.TotalTokens += r.GetTotalTokens()
		s.PromptTokens += r.GetPromptTokens()
		s.CompletionTokens += r.GetCompletionTokens()
		s.CacheReadTokens += r.GetCacheReadTokens()
		s.CacheWriteTokens += r.GetCacheWriteTokens()
		s.CostUsd += r.GetCostUsd()
		s.RecordCount++
		s.ExecutionCount++
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetCostUsd() > out[j].GetCostUsd() })
	return out
}

// plane is the disposable mocked Orchicon plane the Overview screen reads.
type plane struct {
	ex *fakeExec
	wi *fakeWorkItems
	w  *fakeWorkers
	im *fakeImages
	tl *fakeTelemetry
	gw *fakeGateway
}

func (p plane) clients(t *testing.T) *client.Clients {
	t.Helper()
	mux := http.NewServeMux()
	path, h := apiv1connect.NewExecutionServiceHandler(p.ex)
	mux.Handle(path, h)
	path, h = apiv1connect.NewWorkItemServiceHandler(p.wi)
	mux.Handle(path, h)
	path, h = apiv1connect.NewWorkerServiceHandler(p.w)
	mux.Handle(path, h)
	path, h = apiv1connect.NewRuntimeImageServiceHandler(p.im)
	mux.Handle(path, h)
	path, h = apiv1connect.NewTelemetryServiceHandler(p.tl)
	mux.Handle(path, h)
	path, h = apiv1connect.NewAIGatewayServiceHandler(p.gw)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.NewWithHTTPClient(client.Options{BaseURL: srv.URL, Token: "oc_test"}, srv.Client())
}

func newModel(t *testing.T, p plane) *Model {
	t.Helper()
	m := New(p.clients(t), subs.NewRegistry(), "")
	// Wide viewport so every pane renders at full width (the shell's own
	// 80/120-col layouts are covered by the shell tests).
	m.SetSize(600, 40)
	return m
}

// drive applies one Cmd tree (BatchMsg-aware) to the model, mirroring what
// bubbletea's loop delivers: run the cmd, feed the msg to Update, recurse.
func drive(t *testing.T, m *Model, c tea.Cmd) *Model {
	t.Helper()
	if c == nil {
		return m
	}
	msg := c()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, cc := range batch {
			m = drive(t, m, cc)
		}
		return m
	}
	if msg == nil {
		return m
	}
	ns, next := m.Update(msg)
	return drive(t, ns.(*Model), next)
}

// load runs the screen's full Load() and applies every fetched message.
func load(t *testing.T, m *Model) *Model {
	t.Helper()
	return drive(t, m, m.Load())
}

func populatedPlane() plane {
	return plane{
		ex: &fakeExec{want: []*apiv1.WorkerExecution{
			{Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING},
			{Id: "exec-2", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING},
			{Id: "exec-3", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED},
		}},
		wi: &fakeWorkItems{want: []*apiv1.WorkItem{
			{Id: "wi-1", Title: "a", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING},
			{Id: "wi-2", Title: "b", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED},
		}},
		w: &fakeWorkers{want: []*apiv1.Worker{
			{Id: "w1", Name: "alpha", Status: apiv1.WorkerStatus_WORKER_STATUS_PUBLISHED},
			{Id: "w2", Name: "beta", Status: apiv1.WorkerStatus_WORKER_STATUS_DRAFT},
		}},
		im: &fakeImages{want: []*apiv1.RuntimeImage{
			{Id: "im1", Name: "base", Status: apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY},
			{Id: "im2", Name: "bad", Status: apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_FAILED},
		}},
		tl: &fakeTelemetry{traces: []*apiv1.Trace{{
			TraceId:      "tr-1",
			RootSpanName: "gateway.anthropic.request",
			DurationUs:   1500,
			SpanCount:    2,
			Spans: []*apiv1.TraceSpan{
				{SpanId: "s1", Name: "gateway.anthropic.request", Service: "gateway", DurationUs: 1200},
				{SpanId: "s2", Name: "db.query", Service: "api", DurationUs: 200, StatusCode: 2, StatusMessage: "boom"},
			},
		}}},
		gw: &fakeGateway{records: []*apiv1.UsageRecord{
			{Id: "r1", ProjectId: "proj-a", Provider: "anthropic", Model: "claude-sonnet-4", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CostUsd: 1.5, WorkerName: "impl"},
			{Id: "r2", ProjectId: "proj-b", Provider: "openai", Model: "gpt-x", PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30, CostUsd: 0.5, TaskTitle: "fix bug"},
		}},
	}
}

func emptyPlane() plane {
	return plane{
		ex: &fakeExec{}, wi: &fakeWorkItems{}, w: &fakeWorkers{},
		im: &fakeImages{}, tl: &fakeTelemetry{}, gw: &fakeGateway{},
	}
}

// assertContains fails the test when any wanted fragment is missing.
func assertContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("view missing %q\n---\n%s", w, got)
		}
	}
}

// ---- Dashboard --------------------------------------------------------

func TestDashboardRendersAggregatesFromStubbedRPCs(t *testing.T) {
	m := load(t, newModel(t, populatedPlane()))
	m.SelectSource("dashboard")
	assertContains(t, m.View(),
		"Executions by status", "running 2", "succeeded 1",
		"Work Items by status", "pending 1",
		"Workers", "published 1",
		"Runtime Images", "ready 1", "failed 1")

	// The detail re-reads the same stubbed RPCs and renders the breakdown
	// (exec ids appear ONLY in the recent-activity detail body).
	m = drive(t, m, m.RequestDetail("dashboard", "activity"))
	assertContains(t, m.View(), "Recent activity", "exec-1")

	m = drive(t, m, m.RequestDetail("dashboard", "images"))
	assertContains(t, m.View(), "ready (usable)")

	m = drive(t, m, m.RequestDetail("dashboard", "workers"))
	assertContains(t, m.View(), "published (active)")
}

// ---- Telemetry --------------------------------------------------------

func TestTelemetryListAndSpanDetail(t *testing.T) {
	m := load(t, newModel(t, populatedPlane()))
	m.SelectSource("telemetry")
	assertContains(t, m.View(), "gateway.anthropic.request", "2 spans")

	m = drive(t, m, m.RequestDetail("telemetry", "tr-1"))
	assertContains(t, m.View(), "spans", "db.query", "boom")
}

func TestTelemetryLiveStreamDeliversEvents(t *testing.T) {
	p := populatedPlane()
	p.tl.live = true
	m := newModel(t, p)
	defer m.Close() // cancel the live stream so the fixture server can close
	m.EnsureSubscriptions()
	if m.sub == nil {
		t.Fatal("EnsureSubscriptions must create the telemetry sub")
	}
	if m.reg.Count() == 0 {
		t.Fatal("telemetry sub must be registered")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(m.sub.Events()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("StreamTelemetry delivered no events")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.sub.Status(); got != "open" {
		t.Fatalf("stream status = %q, want open", got)
	}
	// The footer reporter carries the telemetry subscription name.
	found := false
	for _, st := range m.Base.ReportStatus() {
		if st.Name == "telemetry" {
			found = true
		}
	}
	if !found {
		t.Fatal("telemetry stream status not reported (footer would show nothing)")
	}
}

// ---- Cost Explorer + Usage -------------------------------------------

// The Cost Explorer reads the SERVER-SIDE roll-up (GetCost) at the GUI's
// drill-down levels: a grand total, per-model and per-project groups, and the
// per-workflow view. It used to aggregate the first page of raw usage records
// client-side, which is why its breakdowns did not match the GUI.
func TestCostExplorerBreakdownAndTotal(t *testing.T) {
	m := load(t, newModel(t, populatedPlane()))
	m.SelectSource("cost-explorer")
	assertContains(t, m.View(),
		"TOTAL", "$2.00",
		"claude-sonnet-4", "gpt-x", // by model
		"proj-a", "proj-b") // by project

	// The total's detail carries the full breakdown numbers.
	m = drive(t, m, m.RequestDetail("cost-explorer", "total"))
	assertContains(t, m.View(),
		"Cost — total", "cost", "$2.00", "total tokens", "180",
		"prompt tokens", "120", "completion tokens", "60",
		"cache read", "cache write", "executions", "records", "window")

	// A model group's detail names the group and shows its own numbers.
	m = drive(t, m, m.RequestDetail("cost-explorer", "model:claude-sonnet-4"))
	assertContains(t, m.View(), "Cost — model claude-sonnet-4", "$1.50", "150")

	// A project group's detail likewise.
	m = drive(t, m, m.RequestDetail("cost-explorer", "project:proj-b"))
	assertContains(t, m.View(), "Cost — project proj-b", "$0.50", "30")
}

func TestUsageRecordsTableAndDetail(t *testing.T) {
	m := load(t, newModel(t, populatedPlane()))
	m.SelectSource("usage")
	assertContains(t, m.View(), "impl", "fix bug", "anthropic/claude-sonnet-4", "$1.50")

	m = drive(t, m, m.RequestDetail("usage", "r1"))
	assertContains(t, m.View(), "Usage record", "anthropic", "claude-sonnet-4")
}

// ---- Empty states -----------------------------------------------------

func TestEmptyStatesNameWhyEachPaneIsEmpty(t *testing.T) {
	m := load(t, newModel(t, emptyPlane()))
	// Two panes render at a time, so each source's empty state is checked by
	// focusing it.
	for _, tc := range []struct{ src, want string }{
		{"dashboard", "no plane data to aggregate"},
		{"telemetry", "no traces in the window"},
		{"usage", "no usage records"},
	} {
		if !m.SelectSource(tc.src) {
			t.Fatalf("source %q not selectable", tc.src)
		}
		v := m.View()
		if strings.Contains(v, "nothing here") {
			t.Fatalf("bare \"nothing here\" empty state leaked into %s:\n%s", tc.src, v)
		}
		assertContains(t, v, tc.want)
	}
}

// The operator's read of Telemetry was "just a bunch of traces". The pane now
// opens on an at-a-glance SUMMARY row (traces, spans, errored spans, slowest)
// and each trace row flags its own errored span count.
func TestTelemetrySummaryAndErrorFlagging(t *testing.T) {
	m := load(t, newModel(t, populatedPlane()))
	m.SelectSource("telemetry")
	assertContains(t, m.View(),
		"SUMMARY", // the aggregate row, first
		"1 traces · 2 spans",
		"1 errored",
		"gateway.anthropic.request", // the trace row
		"1 err")                     // …flagged because it carries an error span

	// The summary's detail reports the numbers, not a trace.
	m = drive(t, m, m.RequestDetail("telemetry", "summary"))
	assertContains(t, m.View(),
		"Telemetry — summary", "traces", "spans", "errored spans", "slowest trace")
}

// An empty window must NOT show a 0/0 summary row: the pane's empty state names
// WHY it is empty, and a summary would suppress that explanation.
func TestTelemetryEmptyWindowKeepsItsExplanation(t *testing.T) {
	m := load(t, newModel(t, emptyPlane()))
	m.SelectSource("telemetry")
	v := m.View()
	if strings.Contains(v, "SUMMARY") {
		t.Fatalf("an empty window must not render a summary row:\n%s", v)
	}
	assertContains(t, v, "no traces in the window")
}
