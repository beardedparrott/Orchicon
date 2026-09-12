// Package overview implements the Overview screen on the kit2 primitives:
// the plane's aggregate state in three GUI-parity sources — Dashboard
// (executions/work items by status, worker + runtime-image health, recent
// activity), Telemetry (traces list + span detail + the live
// StreamTelemetry), Cost Explorer (usage/cost aggregation by provider +
// model + total) — plus the raw Usage records table (`/usage`). Read-only:
// the GUI's Overview surfaces expose no mutations
// (frontend/src/lib/nav-config.ts NAV_GROUPS[0]).
package overview

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Overview screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamTelemetryResponse]
	reconnected bool
}

// New builds the screen. Sources mirror the GUI Overview domain:
// Dashboard, Telemetry, Cost Explorer (+ the raw Usage records table that
// covers the GUI's /usage route, which lives in the same nav tree).
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "overview"
	m.AddSource("dashboard", "Dashboard", m.fetchDashboard)
	m.AddSource("telemetry", "Telemetry", m.fetchTraces)
	m.AddSource("cost-explorer", "Cost Explorer", m.fetchCost)
	m.AddSource("usage", "Usage Records", m.fetchUsage)
	m.SetDetail(m.detail)
	// Empty states name WHY the pane is empty (never a bare "nothing here").
	m.Base.SetSourceEmpty("dashboard", "no plane data to aggregate — the plane returned no executions, work items, workers, or runtime images")
	m.Base.SetSourceEmpty("telemetry", "no traces in the window — the telemetry backend returned no spans (it may not be configured/reachable)")
	m.Base.SetSourceEmpty("cost-explorer", "no usage records — the AI gateway recorded no LLM usage/cost in this window")
	m.Base.SetSourceEmpty("usage", "no usage records — the AI gateway recorded no LLM calls in this window")
	m.Base.SetStatuses([]kit2.StatusMsg{
		{Name: "telemetry", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "overview" }

// EnsureSubscriptions starts the telemetry live stream once (idempotent;
// the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.Telemetry(m.cl, m.tenantID)
	}
}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("telemetry"), m.reg.WaitEventPoke("telemetry"))
}

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case subs.EventPokeMsg:
		// A live telemetry event landed: refresh the panes that consume
		// usage/trace data so the view stays current (the pokes are
		// overflow-dropping, so a burst collapses to one refresh).
		cmd := m.reg.WaitEventPoke("telemetry")
		if msg.Name != "telemetry" {
			return m, cmd
		}
		cmds := []tea.Cmd{cmd}
		for _, src := range []string{"telemetry", "cost-explorer", "usage"} {
			if c := m.Base.Refresh(src); c != nil {
				cmds = append(cmds, c)
			}
		}
		return m, tea.Batch(cmds...)

	case subs.StatusMsg:
		m.Base.SetStatus(msg.Name, string(msg.Status))
		cmd := m.reg.WaitStatus("telemetry")
		if msg.Status == "open" && m.reconnected {
			// reconnect gap: refetch (invalidate-on-reconnect)
			return m, tea.Batch(cmd, m.Load())
		}
		if msg.Status != "open" {
			m.reconnected = true
		}
		return m, cmd

	case tea.KeyMsg:
		if msg.String() == "r" {
			return m, m.Load()
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.Base.View())
	b.WriteString("\n")
	b.WriteString(theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh · live telemetry stream feeds cost/usage"))
	return m.Base.Frame(b.String())
}

// ---- Dashboard --------------------------------------------------------

// dashOrder fixes the dashboard row order (deterministic nav + tests).
var dashOrder = []string{"executions", "work-items", "workers", "images", "activity"}

// section is one dashboard aggregate row (list item + detail pane).
type section struct {
	title  string
	meta   string
	fields []kit2.Field
	body   string
}

// fetchDashboard aggregates the plane's state from the four read RPCs the
// dashboard consumes (executions, work items, workers, runtime images).
func (m *Model) fetchDashboard(ctx context.Context, _ string) ([]kit2.Item, string, error) {
	d, anyData, err := m.dashboard(ctx)
	if err != nil {
		return nil, "", err
	}
	if !anyData {
		// No plane data at all: surface the pane's empty state (which names
		// why) instead of five all-zero rows.
		return nil, "", nil
	}
	items := make([]kit2.Item, 0, len(dashOrder))
	for _, id := range dashOrder {
		sec, ok := d[id]
		if !ok {
			continue
		}
		items = append(items, kit2.Item{ID: id, Title: sec.title, Meta: sec.meta})
	}
	return items, "", nil
}

func (m *Model) dashboard(ctx context.Context) (map[string]section, bool, error) {
	exResp, err := m.cl.Executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{
		TenantId: m.tenantID, PageSize: 200,
	}))
	if err != nil {
		return nil, false, err
	}
	wiResp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		TenantId: m.tenantID, PageSize: 200,
	}))
	if err != nil {
		return nil, false, err
	}
	wResp, err := m.cl.Workers.ListWorkers(ctx, connect.NewRequest(&apiv1.ListWorkersRequest{PageSize: 200}))
	if err != nil {
		return nil, false, err
	}
	imResp, err := m.cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 200}))
	if err != nil {
		return nil, false, err
	}

	exBy := map[string]int{}
	for _, e := range exResp.Msg.GetExecutions() {
		exBy[enumWord(e.GetStatus())]++
	}
	wiBy := map[string]int{}
	for _, w := range wiResp.Msg.GetWorkItems() {
		wiBy[enumWord(w.GetStatus())]++
	}
	wBy := map[string]int{}
	for _, w := range wResp.Msg.GetWorkers() {
		wBy[enumWord(w.GetStatus())]++
	}
	imBy := map[string]int{}
	for _, im := range imResp.Msg.GetRuntimeImages() {
		imBy[enumWord(im.GetStatus())]++
	}

	out := map[string]section{}
	out["executions"] = countSection("Executions by status", exBy, len(exResp.Msg.GetExecutions()))
	out["work-items"] = countSection("Work Items by status", wiBy, len(wiResp.Msg.GetWorkItems()))

	workers := countSection("Workers", wBy, len(wResp.Msg.GetWorkers()))
	workers.fields = append(workers.fields, kit2.Field{
		Key:   "health",
		Value: fmt.Sprintf("%d published (active) · %d not published", wBy["published"], len(wResp.Msg.GetWorkers())-wBy["published"]),
	})
	out["workers"] = workers

	images := countSection("Runtime Images", imBy, len(imResp.Msg.GetRuntimeImages()))
	images.fields = append(images.fields, kit2.Field{
		Key:   "health",
		Value: fmt.Sprintf("%d ready (usable) · %d failed", imBy["ready"], imBy["failed"]),
	})
	out["images"] = images

	var recent strings.Builder
	for i, e := range exResp.Msg.GetExecutions() {
		if i >= 5 {
			break
		}
		recent.WriteString(enumWord(e.GetStatus()) + "  " + e.GetId() + "  " + screenkit.FmtTime(e.GetStartedAt()) + "\n")
	}
	out["activity"] = section{
		title: "Recent activity",
		meta:  fmt.Sprintf("%d execution(s) · newest first", len(exResp.Msg.GetExecutions())),
		fields: []kit2.Field{
			{Key: "executions", Value: screenkit.FmtInt(len(exResp.Msg.GetExecutions()))},
			{Key: "work items", Value: screenkit.FmtInt(len(wiResp.Msg.GetWorkItems()))},
			{Key: "workers", Value: screenkit.FmtInt(len(wResp.Msg.GetWorkers()))},
			{Key: "runtime images", Value: screenkit.FmtInt(len(imResp.Msg.GetRuntimeImages()))},
		},
		body: strings.TrimRight(recent.String(), "\n"),
	}
	anyData := len(exResp.Msg.GetExecutions())+len(wiResp.Msg.GetWorkItems())+
		len(wResp.Msg.GetWorkers())+len(imResp.Msg.GetRuntimeImages()) > 0
	return out, anyData, nil
}

// countSection renders a status histogram (sorted, deterministic).
func countSection(title string, by map[string]int, total int) section {
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := []kit2.Field{{Key: "total", Value: screenkit.FmtInt(total)}}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, kit2.Field{Key: k, Value: screenkit.FmtInt(by[k])})
		parts = append(parts, k+" "+strconv.Itoa(by[k]))
	}
	meta := strconv.Itoa(total) + " total"
	if len(parts) > 0 {
		meta += " · " + strings.Join(parts, " · ")
	}
	return section{title: title, meta: meta, fields: fields}
}

// ---- Telemetry --------------------------------------------------------

func (m *Model) fetchTraces(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Telemetry.QueryTraces(ctx, connect.NewRequest(&apiv1.QueryTracesRequest{
		Query: &apiv1.TelemetryQuery{Limit: 100, PageToken: pageToken},
	}))
	if err != nil {
		return nil, "", err
	}
	traces := resp.Msg.GetTraces()
	if len(traces) == 0 {
		// No summary row for an empty window: the pane's empty state names WHY
		// (backend unreachable / not configured). A "0 traces" summary would
		// suppress that explanation and say nothing useful.
		return nil, resp.Msg.GetNextPageToken(), nil
	}

	// A summary row FIRST, so the pane opens on the at-a-glance numbers rather
	// than "just a bunch of traces" (the operator's read of this screen): how
	// many traces/spans, how many errored, and the slowest.
	totalSpans, errSpans := 0, 0
	for _, tr := range traces {
		totalSpans += int(tr.GetSpanCount())
		for _, sp := range tr.GetSpans() {
			if sp.GetStatusCode() == 2 {
				errSpans++
			}
		}
	}

	items := make([]kit2.Item, 0, len(traces)+1)
	meta := fmt.Sprintf("%d traces · %d spans", len(traces), totalSpans)
	if errSpans > 0 {
		meta += fmt.Sprintf(" · %d errored", errSpans)
	}
	if resp.Msg.GetDegraded() {
		meta += " · degraded"
	}
	items = append(items, kit2.Item{ID: "summary", Title: "SUMMARY", Meta: meta})

	for _, tr := range traces {
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
		tmeta := fmt.Sprintf("%s · %d spans", fmtDuration(tr.GetDurationUs()), tr.GetSpanCount())
		if errN > 0 {
			tmeta += fmt.Sprintf(" · %d err", errN)
		}
		items = append(items, kit2.Item{ID: tr.GetTraceId(), Title: title, Meta: tmeta})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// traceDetail renders one trace: root fields + the span tree as body text.
func (m *Model) traceDetail(ctx context.Context, id string) (string, []kit2.Field, string, error) {
	// The summary row reports the aggregate the pane is really asking about.
	if id == "summary" {
		resp, err := m.cl.Telemetry.QueryTraces(ctx, connect.NewRequest(&apiv1.QueryTracesRequest{
			Query: &apiv1.TelemetryQuery{Limit: 100},
		}))
		if err != nil {
			return "", nil, "", err
		}
		traces := resp.Msg.GetTraces()
		totalSpans, errSpans := 0, 0
		var slowest *apiv1.Trace
		for _, tr := range traces {
			totalSpans += int(tr.GetSpanCount())
			for _, sp := range tr.GetSpans() {
				if sp.GetStatusCode() == 2 {
					errSpans++
				}
			}
			if slowest == nil || tr.GetDurationUs() > slowest.GetDurationUs() {
				slowest = tr
			}
		}
		fields := []kit2.Field{
			{Key: "traces", Value: screenkit.FmtInt(len(traces))},
			{Key: "spans", Value: screenkit.FmtInt(totalSpans)},
			{Key: "errored spans", Value: screenkit.FmtInt(errSpans)},
			{Key: "degraded", Value: screenkit.FmtBool(resp.Msg.GetDegraded())},
		}
		if slowest != nil {
			fields = append(fields,
				kit2.Field{Key: "slowest trace", Value: slowest.GetRootSpanName()},
				kit2.Field{Key: "slowest", Value: fmtDuration(slowest.GetDurationUs())},
			)
		}
		return "Telemetry — summary", fields, "Select a trace to see its spans.", nil
	}

	resp, err := m.cl.Telemetry.QueryTraces(ctx, connect.NewRequest(&apiv1.QueryTracesRequest{
		Query: &apiv1.TelemetryQuery{TraceId: id, Limit: 1},
	}))
	if err != nil {
		return "", nil, "", err
	}
	var tr *apiv1.Trace
	for _, t := range resp.Msg.GetTraces() {
		if t.GetTraceId() == id {
			tr = t
			break
		}
	}
	if tr == nil && len(resp.Msg.GetTraces()) > 0 {
		tr = resp.Msg.GetTraces()[0]
	}
	if tr == nil {
		return "Trace " + id, []kit2.Field{
			{Key: "trace", Value: id},
			{Key: "note", Value: "trace not returned by the telemetry backend"},
		}, "", nil
	}
	fields := []kit2.Field{
		{Key: "trace", Value: tr.GetTraceId()},
		{Key: "root span", Value: tr.GetRootSpanName()},
		{Key: "duration", Value: fmtDuration(tr.GetDurationUs())},
		{Key: "spans", Value: screenkit.FmtInt(int(tr.GetSpanCount()))},
		{Key: "started", Value: screenkit.FmtTime(tr.GetStartTime())},
	}
	var b strings.Builder
	b.WriteString("spans\n")
	for _, sp := range tr.GetSpans() {
		line := fmt.Sprintf("  %s  %-10s %-28s %s", sp.GetSpanId(), sp.GetService(), sp.GetName(), fmtDuration(sp.GetDurationUs()))
		if sp.GetStatusCode() == 2 {
			line += "  [error: " + sp.GetStatusMessage() + "]"
		}
		b.WriteString(line + "\n")
	}
	return "Trace " + tr.GetRootSpanName(), fields, strings.TrimRight(b.String(), "\n"), nil
}

// ---- Cost Explorer + Usage -------------------------------------------

// costWindow is the lookback the Cost Explorer reports over.
const costWindow = 30 * 24 * time.Hour

// fetchCost rolls the usage up SERVER-SIDE at the GUI's drill-down levels.
//
// The TUI used to aggregate the first page of raw usage records client-side,
// which is why its breakdowns did not match the GUI — and it silently missed
// everything past the first page of records. GetCost returns the grand total,
// per-model and per-project groups, and the next level as children.
func (m *Model) fetchCost(ctx context.Context, _ string) ([]kit2.Item, string, error) {
	items := []kit2.Item{}

	total, err := m.getCostTotal(ctx)
	if err != nil {
		return nil, "", err
	}
	if total != nil {
		items = append(items, kit2.Item{
			ID:    "total",
			Title: "TOTAL",
			Meta: fmtCost(total.GetCostUsd()) + " · " + fmtTokens(total.GetTotalTokens()) + " tok · " +
				screenkit.FmtInt(int(total.GetExecutionCount())) + " exec",
		})
	}

	for _, lvl := range []struct {
		rollup apiv1.UsageRollup
		label  string
	}{
		{apiv1.UsageRollup_USAGE_ROLLUP_MODEL, "model"},
		{apiv1.UsageRollup_USAGE_ROLLUP_PROJECT, "project"},
	} {
		sums, err := m.getCostSummaries(ctx, lvl.rollup)
		if err != nil {
			return nil, "", err
		}
		for _, s := range sums {
			name := s.GetDisplayName()
			if name == "" {
				name = s.GetGroupKey()
			}
			items = append(items, kit2.Item{
				ID:    lvl.label + ":" + s.GetGroupKey(),
				Title: name,
				Meta: lvl.label + " · " + fmtCost(s.GetCostUsd()) + " · " +
					fmtTokens(s.GetTotalTokens()) + " tok",
			})
		}
	}

	// Per-workflow cost (the GUI's workflow cost view; one row per template,
	// aggregating every run).
	if wfs, err := m.getWorkflowCosts(ctx); err == nil {
		for _, w := range wfs {
			items = append(items, kit2.Item{
				ID:    "workflow:" + w.GetWorkflowId(),
				Title: w.GetWorkflowName(),
				Meta: "workflow · " + fmtCost(w.GetTotalCostUsd()) + " · " +
					screenkit.FmtInt(int(w.GetRunCount())) + " runs",
			})
		}
	}
	return items, "", nil
}

// costWindows is the reporting window (start, end).
func (m *Model) costWindows() (time.Time, time.Time) {
	now := time.Now()
	return now.Add(-costWindow), now
}

// getCostSummaries returns the roll-up at one drill-down level.
func (m *Model) getCostSummaries(ctx context.Context, lvl apiv1.UsageRollup) ([]*apiv1.CostSummary, error) {
	start, end := m.costWindows()
	resp, err := m.cl.AIGateway.GetCost(ctx, connect.NewRequest(&apiv1.GetCostRequest{
		TenantId: m.tenantID,
		Rollup:   lvl,
		Start:    timestamppb.New(start),
		End:      timestamppb.New(end),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSummaries(), nil
}

// getCostTotal returns the grand total for the window.
func (m *Model) getCostTotal(ctx context.Context) (*apiv1.CostSummary, error) {
	start, end := m.costWindows()
	resp, err := m.cl.AIGateway.GetCost(ctx, connect.NewRequest(&apiv1.GetCostRequest{
		TenantId: m.tenantID,
		Rollup:   apiv1.UsageRollup_USAGE_ROLLUP_TENANT,
		Start:    timestamppb.New(start),
		End:      timestamppb.New(end),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetTotal(), nil
}

// getWorkflowCosts returns per-workflow aggregates for the window.
func (m *Model) getWorkflowCosts(ctx context.Context) ([]*apiv1.WorkflowCostAggregate, error) {
	start, end := m.costWindows()
	resp, err := m.cl.AIGateway.GetWorkflowCosts(ctx, connect.NewRequest(&apiv1.GetWorkflowCostsRequest{
		Start: timestamppb.New(start),
		End:   timestamppb.New(end),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetWorkflows(), nil
}

// costSummaryFields renders a roll-up's numbers — the GUI's cost breakdown
// (cost, token split, cache buckets, executions, records, window).
func costSummaryFields(s *apiv1.CostSummary, label string) []kit2.Field {
	if s == nil {
		return []kit2.Field{{Key: label, Value: "no usage in the window"}}
	}
	return []kit2.Field{
		{Key: label + " key", Value: s.GetGroupKey()},
		{Key: "cost", Value: fmtCost(s.GetCostUsd())},
		{Key: "total tokens", Value: screenkit.FmtInt64(s.GetTotalTokens())},
		{Key: "prompt tokens", Value: screenkit.FmtInt64(s.GetPromptTokens())},
		{Key: "completion tokens", Value: screenkit.FmtInt64(s.GetCompletionTokens())},
		{Key: "cache read", Value: screenkit.FmtInt64(s.GetCacheReadTokens())},
		{Key: "cache write", Value: screenkit.FmtInt64(s.GetCacheWriteTokens())},
		{Key: "executions", Value: screenkit.FmtInt(int(s.GetExecutionCount()))},
		{Key: "records", Value: screenkit.FmtInt(int(s.GetRecordCount()))},
		{Key: "window", Value: screenkit.FmtTime(s.GetWindowStart()) + " → " + screenkit.FmtTime(s.GetWindowEnd())},
	}
}

// costChildrenBody lists the next drill-down level the backend attached.
func costChildrenBody(s *apiv1.CostSummary) string {
	if s == nil || len(s.GetChildren()) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render("BREAKDOWN") + "\n")
	for _, c := range s.GetChildren() {
		name := c.GetDisplayName()
		if name == "" {
			name = c.GetGroupKey()
		}
		b.WriteString(fmt.Sprintf("  %-28s %10s  %10s tok  %d exec\n",
			truncateName(name, 28), fmtCost(c.GetCostUsd()), fmtTokens(c.GetTotalTokens()), c.GetExecutionCount()))
	}
	return strings.TrimRight(b.String(), "\n")
}

// costWorkflowLines lists per-workflow cost under the total.
func (m *Model) costWorkflowLines(ctx context.Context) string {
	wfs, err := m.getWorkflowCosts(ctx)
	if err != nil || len(wfs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + theme.ListTitle.Render("BY WORKFLOW") + "\n")
	for _, w := range wfs {
		b.WriteString(fmt.Sprintf("  %-28s %10s  %d runs\n",
			truncateName(w.GetWorkflowName(), 28), fmtCost(w.GetTotalCostUsd()), w.GetRunCount()))
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncateName clips a display name to n runes.
func truncateName(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// costDetail renders one roll-up row: the full CostSummary breakdown plus the
// next drill-down level the backend attached.
func (m *Model) costDetail(ctx context.Context, id string) (string, []kit2.Field, string, error) {
	switch {
	case id == "total":
		t, err := m.getCostTotal(ctx)
		if err != nil {
			return "", nil, "", err
		}
		return "Cost — total", costSummaryFields(t, "total"),
			joinBody(costChildrenBody(t), m.costWorkflowLines(ctx)), nil

	case strings.HasPrefix(id, "model:"), strings.HasPrefix(id, "project:"):
		lvlName, key, _ := strings.Cut(id, ":")
		lvl := apiv1.UsageRollup_USAGE_ROLLUP_MODEL
		if lvlName == "project" {
			lvl = apiv1.UsageRollup_USAGE_ROLLUP_PROJECT
		}
		sums, err := m.getCostSummaries(ctx, lvl)
		if err != nil {
			return "", nil, "", err
		}
		for _, s := range sums {
			if s.GetGroupKey() != key {
				continue
			}
			return "Cost — " + lvlName + " " + key, costSummaryFields(s, lvlName), costChildrenBody(s), nil
		}
		return "Cost — " + lvlName + " " + key, []kit2.Field{{Key: lvlName, Value: key}}, "", nil

	case strings.HasPrefix(id, "workflow:"):
		wid := strings.TrimPrefix(id, "workflow:")
		wfs, err := m.getWorkflowCosts(ctx)
		if err != nil {
			return "", nil, "", err
		}
		for _, w := range wfs {
			if w.GetWorkflowId() != wid {
				continue
			}
			fields := []kit2.Field{
				{Key: "workflow", Value: w.GetWorkflowName()},
				{Key: "workflow id", Value: w.GetWorkflowId()},
				{Key: "total cost", Value: fmtCost(w.GetTotalCostUsd())},
				{Key: "total tokens", Value: screenkit.FmtInt64(w.GetTotalTokens())},
				{Key: "runs", Value: screenkit.FmtInt(int(w.GetRunCount()))},
				{Key: "executions", Value: screenkit.FmtInt(int(w.GetExecutionCount()))},
			}
			return "Cost — workflow " + w.GetWorkflowName(), fields, "", nil
		}
		return "Cost — workflow", []kit2.Field{{Key: "workflow id", Value: wid}}, "", nil
	}
	return "", nil, "", nil
}

// joinBody joins non-empty body sections with a blank line between them.
func joinBody(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

func (m *Model) fetchUsage(ctx context.Context, _ string) ([]kit2.Item, string, error) {
	resp, err := m.cl.AIGateway.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{
		TenantId: m.tenantID, PageSize: 500,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.GetRecords()))
	for _, r := range resp.Msg.GetRecords() {
		title := r.GetWorkerName()
		if title == "" {
			title = r.GetTaskTitle()
		}
		if title == "" {
			title = r.GetProvider() + "/" + r.GetModel()
		}
		items = append(items, kit2.Item{
			ID:    r.GetId(),
			Title: title,
			Meta:  fmt.Sprintf("%s/%s · %s tok · %s", r.GetProvider(), r.GetModel(), fmtTokens(r.GetTotalTokens()), fmtCost(r.GetCostUsd())),
		})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// usageAggregate reads the AI gateway's usage records and rolls them up by
// provider and by model, with a grand total (the orchicon_get_usage shape).
func (m *Model) usageAggregate(ctx context.Context) (*usageAgg, error) {
	resp, err := m.cl.AIGateway.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{
		TenantId: m.tenantID, PageSize: 500,
	}))
	if err != nil {
		return nil, err
	}
	a := newUsageAgg()
	for _, r := range resp.Msg.GetRecords() {
		a.add(r)
	}
	return a, nil
}

// usageDetail renders one raw usage record (`/usage`).

func (m *Model) usageDetail(ctx context.Context, id string) (string, []kit2.Field, string, error) {
	resp, err := m.cl.AIGateway.GetUsage(ctx, connect.NewRequest(&apiv1.GetUsageRequest{
		TenantId: m.tenantID, PageSize: 500,
	}))
	if err != nil {
		return "", nil, "", err
	}
	for _, r := range resp.Msg.GetRecords() {
		if r.GetId() != id {
			continue
		}
		fields := []kit2.Field{
			{Key: "id", Value: r.GetId()},
			{Key: "provider", Value: r.GetProvider()},
			{Key: "model", Value: r.GetModel()},
			{Key: "worker", Value: r.GetWorkerName()},
			{Key: "task", Value: r.GetTaskTitle()},
			{Key: "execution", Value: r.GetExecutionId()},
			{Key: "prompt tokens", Value: screenkit.FmtInt64(r.GetPromptTokens())},
			{Key: "completion tokens", Value: screenkit.FmtInt64(r.GetCompletionTokens())},
			{Key: "total tokens", Value: screenkit.FmtInt64(r.GetTotalTokens())},
			{Key: "cost", Value: fmtCost(r.GetCostUsd())},
			{Key: "occurred", Value: screenkit.FmtTime(r.GetOccurredAt())},
		}
		return "Usage record", fields, "", nil
	}
	return "Usage record", []kit2.Field{{Key: "id", Value: id}}, "", nil
}

// ---- detail dispatch --------------------------------------------------

func (m *Model) detail(ctx context.Context, src, id string) (string, []kit2.Field, string, error) {
	switch src {
	case "dashboard":
		d, _, err := m.dashboard(ctx)
		if err != nil {
			return "", nil, "", err
		}
		if sec, ok := d[id]; ok {
			return sec.title, sec.fields, sec.body, nil
		}
		return "Dashboard", []kit2.Field{{Key: "id", Value: id}}, "", nil
	case "telemetry":
		return m.traceDetail(ctx, id)
	case "cost-explorer":
		return m.costDetail(ctx, id)
	case "usage":
		return m.usageDetail(ctx, id)
	}
	return "", nil, "", nil
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg jumps).
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// ActiveSourceName / ActiveItem expose the Base focus state to the shell's
// context engine.
func (m *Model) ActiveSourceName() string      { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (kit2.Item, bool) { return m.Base.ActiveItem() }

// ---- helpers ----------------------------------------------------------

// enumWord renders a proto enum to its bare lowercase word
// (EXECUTION_STATUS_RUNNING → "running") so aggregate labels read naturally.
func enumWord(e interface{ String() string }) string {
	s := strings.ToLower(e.String())
	for _, pre := range []string{"execution_status_", "work_item_status_", "worker_status_", "runtime_image_status_"} {
		s = strings.TrimPrefix(s, pre)
	}
	return s
}

func fmtCost(usd float64) string { return fmt.Sprintf("$%.2f", usd) }

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func fmtDuration(us int64) string {
	if us <= 0 {
		return "—"
	}
	return time.Duration(us * int64(time.Microsecond)).String()
}

// bucket is a cost/token roll-up.
type bucket struct {
	cost   float64
	tokens int64
	count  int
}

// usageAgg rolls usage records up by provider and by model with a total.
type usageAgg struct {
	cost      float64
	tokens    int64
	count     int
	providers map[string]*bucket
	models    map[string]*bucket
	pm        map[string]map[string]*bucket // provider → model → bucket
}

func newUsageAgg() *usageAgg {
	return &usageAgg{
		providers: map[string]*bucket{},
		models:    map[string]*bucket{},
		pm:        map[string]map[string]*bucket{},
	}
}

func (a *usageAgg) add(r *apiv1.UsageRecord) {
	a.cost += r.GetCostUsd()
	a.tokens += r.GetTotalTokens()
	a.count++

	p := r.GetProvider()
	if p == "" {
		p = "(unknown)"
	}
	mo := r.GetModel()
	if mo == "" {
		mo = "(unknown)"
	}
	addTo(a.providers, p, r)
	addTo(a.models, mo, r)
	if a.pm[p] == nil {
		a.pm[p] = map[string]*bucket{}
	}
	addTo(a.pm[p], mo, r)
}

func addTo(m map[string]*bucket, key string, r *apiv1.UsageRecord) {
	b := m[key]
	if b == nil {
		b = &bucket{}
		m[key] = b
	}
	b.cost += r.GetCostUsd()
	b.tokens += r.GetTotalTokens()
	b.count++
}

// items renders the cost-explorer list: the total first, then the provider
// breakdown, then the model breakdown. Empty (no records) returns nil so
// the pane's empty state fires.
func (a *usageAgg) items() []kit2.Item {
	if a.count == 0 {
		return nil
	}
	items := []kit2.Item{{
		ID:    "total",
		Title: "Total",
		Meta:  fmt.Sprintf("%s · %s tok · %d rec", fmtCost(a.cost), fmtTokens(a.tokens), a.count),
	}}
	for _, p := range sortedKeys(a.providers) {
		b := a.providers[p]
		items = append(items, kit2.Item{
			ID:    "provider:" + p,
			Title: "By provider · " + p,
			Meta:  fmt.Sprintf("%s · %s tok", fmtCost(b.cost), fmtTokens(b.tokens)),
		})
	}
	for _, mo := range sortedKeys(a.models) {
		b := a.models[mo]
		items = append(items, kit2.Item{
			ID:    "model:" + mo,
			Title: "By model · " + mo,
			Meta:  fmt.Sprintf("%s · %s tok", fmtCost(b.cost), fmtTokens(b.tokens)),
		})
	}
	return items
}

func (a *usageAgg) providerLines() string {
	keys := sortedKeys(a.providers)
	if len(keys) == 0 {
		return "(no provider usage in this window)"
	}
	var b strings.Builder
	b.WriteString("by provider\n")
	for _, k := range keys {
		bk := a.providers[k]
		b.WriteString(fmt.Sprintf("  %-18s %10s  %10s  %d rec\n", k, fmtCost(bk.cost), fmtTokens(bk.tokens), bk.count))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *usageAgg) modelLines() string {
	keys := sortedKeys(a.models)
	if len(keys) == 0 {
		return "(no model usage in this window)"
	}
	var b strings.Builder
	b.WriteString("by model\n")
	for _, k := range keys {
		bk := a.models[k]
		b.WriteString(fmt.Sprintf("  %-28s %10s  %10s  %d rec\n", k, fmtCost(bk.cost), fmtTokens(bk.tokens), bk.count))
	}
	return strings.TrimRight(b.String(), "\n")
}

// modelLinesFor renders the per-model breakdown inside one provider.
func (a *usageAgg) modelLinesFor(provider string) string {
	inner := a.pm[provider]
	keys := sortedKeys(inner)
	if len(keys) == 0 {
		return "(no model breakdown for this provider)"
	}
	var b strings.Builder
	b.WriteString("by model\n")
	for _, k := range keys {
		bk := inner[k]
		b.WriteString(fmt.Sprintf("  %-28s %10s  %10s  %d rec\n", k, fmtCost(bk.cost), fmtTokens(bk.tokens), bk.count))
	}
	return strings.TrimRight(b.String(), "\n")
}

// providerLinesFor renders which providers contributed to one model.
func (a *usageAgg) providerLinesFor(model string) string {
	var b strings.Builder
	b.WriteString("by provider\n")
	any := false
	for _, p := range sortedKeys(a.pm) {
		if bk, ok := a.pm[p][model]; ok {
			any = true
			b.WriteString(fmt.Sprintf("  %-18s %10s  %10s  %d rec\n", p, fmtCost(bk.cost), fmtTokens(bk.tokens), bk.count))
		}
	}
	if !any {
		return "(no provider breakdown for this model)"
	}
	return strings.TrimRight(b.String(), "\n")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
