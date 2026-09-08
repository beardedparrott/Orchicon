// Package execution implements the Execution screen: executions +
// workflow runs (read-only) with the live StreamExecutionEvents
// subscription — the first consumer of the useStream-mirroring engine.
package execution

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Execution screen.
type Model struct {
	screenkit.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamExecutionEventsResponse]
	reconnected bool
}

// New builds the screen. Execution events stream live; workflow events
// are fetched on demand only (v1 keeps one live stream per screen).
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "execution"
	m.AddSource("executions", "Executions", m.fetchExecutions)
	m.AddSource("runs", "Workflow Runs", m.fetchRuns)
	m.SetDetail(m.detail)
	m.SetOnDetail(m.onDetail)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "execution-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "execution" }

// EnsureSubscriptions starts the execution-events live stream once
// (idempotent; the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.ExecutionEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("execution-events"), m.reg.WaitEventPoke("execution-events"))
}

func (m *Model) fetchExecutions(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Executions))
	for _, e := range resp.Msg.Executions {
		items = append(items, screenkit.Item{
			ID:    e.GetId(),
			Title: e.GetId(),
			Meta:  strings.ToLower(e.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchRuns(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflowRuns(ctx, connect.NewRequest(&apiv1.ListWorkflowRunsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Runs))
	for _, r := range resp.Msg.Runs {
		items = append(items, screenkit.Item{
			ID:    r.GetId(),
			Title: r.GetId(),
			Meta:  strings.ToLower(r.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case "executions":
		resp, err := m.cl.Executions.GetExecution(ctx, connect.NewRequest(&apiv1.GetExecutionRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		e := resp.Msg.GetExecution()
		fields := []screenkit.Field{
			{Key: "id", Value: e.GetId()},
			{Key: "status", Value: strings.ToLower(e.GetStatus().String())},
			{Key: "health", Value: strings.ToLower(e.GetHealthState().String())},
			{Key: "worker", Value: e.GetWorkerId()},
			{Key: "project", Value: e.GetProjectId()},
			{Key: "tokens", Value: screenkit.FmtInt64(e.GetTokenUsage())},
			{Key: "started", Value: screenkit.FmtTime(e.GetStartedAt())},
			{Key: "ended", Value: screenkit.FmtTime(e.GetEndedAt())},
		}
		return "Execution " + e.GetId(), fields, "", nil
	case "runs":
		resp, err := m.cl.Workflows.GetWorkflowRun(ctx, connect.NewRequest(&apiv1.GetWorkflowRunRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		r := resp.Msg.GetRun()
		fields := []screenkit.Field{
			{Key: "id", Value: r.GetId()},
			{Key: "status", Value: strings.ToLower(r.GetStatus().String())},
			{Key: "workflow", Value: r.GetWorkflowId()},
			{Key: "version", Value: screenkit.FmtInt(int(r.GetWorkflowVersion()))},
			{Key: "current step", Value: r.GetCurrentStep()},
			{Key: "work item", Value: r.GetWorkItemId()},
			{Key: "branch", Value: r.GetWorktreeBranch()},
			{Key: "pr", Value: r.GetPrUrl()},
			{Key: "started", Value: screenkit.FmtTime(r.GetStartedAt())},
			{Key: "ended", Value: screenkit.FmtTime(r.GetEndedAt())},
		}
		// Step runs trail in the body when present.
		var body string
		if sr, err := m.cl.Workflows.GetWorkflowStepRuns(ctx, connect.NewRequest(&apiv1.GetWorkflowStepRunsRequest{RunId: id})); err == nil {
			var b strings.Builder
			for _, s := range sr.Msg.GetStepRuns() {
				b.WriteString(screenkit.StatusBadge(strings.ToLower(s.GetStatus().String())) + " " +
					s.GetStepName() + "  (" + screenkit.FmtTime(s.GetStartedAt()) + ")\n")
			}
			body = strings.TrimRight(b.String(), "\n")
		}
		return "Workflow Run " + r.GetId(), fields, body, nil
	}
	return "", nil, "", nil
}

func (m *Model) Update(msg tea.Msg) (screenkit.Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.SetSize(msg.Width, msg.Height)
		return m, nil

	case subs.EventPokeMsg:
		if msg.Name == "execution-events" && m.Base.ActiveSourceName() == "executions" && m.Base.DetailID() != "" {
			if sh, ok := m.Shell().(interface {
				RefreshExecutionSession(execID string, events []*apiv1.StreamExecutionEventsResponse)
			}); ok {
				sh.RefreshExecutionSession(m.Base.DetailID(), m.SessionEvents(m.Base.DetailID()))
			}
		}
		return m, m.reg.WaitEventPoke("execution-events")

	case subs.StatusMsg:
		m.Base.SetStatus(msg.Name, string(msg.Status))
		cmd := m.reg.WaitStatus("execution-events")
		if msg.Status == "open" && m.reconnected {
			// reconnect gap: refetch lists (invalidate-on-reconnect)
			return m, tea.Batch(cmd, m.Load(), m.requestSessionRefresh())
		}
		if msg.Status != "open" {
			m.reconnected = true
		} else {
			// first open with a detail already showing: refresh its
			// session view so buffered events merge in
			return m, tea.Batch(cmd, m.requestSessionRefresh())
		}
		return m, cmd
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
	b.WriteString(theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh"))
	return b.String()
}

// RunningExecutionID returns the selected execution's ID when its
// status is RUNNING — the interjection context for the chat dock
// (plan §3: composer send on a running execution routes into the live
// session via SendExecutionMessage).
func (m *Model) RunningExecutionID() (string, bool) {
	it, ok := m.Base.ActiveItem()
	if !ok || m.Base.ActiveSourceName() != "executions" {
		return "", false
	}
	return m.runningID(it.ID), true
}

// runningID reports whether the execution is running, from the detail
// status the screen has cached (ListExecutions meta / GetExecution).
func (m *Model) runningID(id string) string {
	// The detail pane refreshes the status; consult the cached fetch
	// state via the list item meta (set to the lowercase status string).
	if it, ok := m.Base.SourceItem("executions", id); ok {
		if strings.EqualFold(it.Meta, "running") || strings.EqualFold(it.Meta, "in_progress") ||
			strings.EqualFold(it.Meta, "in-progress") || strings.EqualFold(it.Meta, "starting") ||
			strings.EqualFold(it.Meta, "dispatching") || strings.EqualFold(it.Meta, "queued") {
			return id
		}
	}
	return ""
}

// ActiveContext returns the context engine triple (source, id, label)
// for the dock chip / footer chip.
func (m *Model) ActiveContext() (src, id, label string) {
	src = m.Base.ActiveSourceName()
	it, ok := m.Base.ActiveItem()
	if !ok {
		return src, "", ""
	}
	label = it.Title
	if label == "" {
		label = it.ID
	}
	if m.runningID(it.ID) != "" {
		label = "running " + label
	}
	return src, it.ID, label
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// DetailWidth exposes the detail pane width (session bubble wrapping).
func (m *Model) DetailWidth() int { return m.Base.DetailWidth() }

// onDetail fires when the detail pane shows an execution: the shell
// loads its durable session and merges the live event stream into it.
func (m *Model) onDetail(src, id string) tea.Cmd {
	if src != "executions" {
		return nil
	}
	if sh, ok := m.Shell().(interface{ OpenExecutionSession(string) tea.Cmd }); ok {
		return sh.OpenExecutionSession(id)
	}
	return nil
}

// requestSessionRefresh re-opens the session view for the detail pane's
// current execution (sub reconnect: buffered events changed).
func (m *Model) requestSessionRefresh() tea.Cmd {
	id := m.Base.DetailID()
	if id == "" || m.Base.ActiveSourceName() != "executions" {
		return nil
	}
	if sh, ok := m.Shell().(interface{ OpenExecutionSession(string) tea.Cmd }); ok {
		return sh.OpenExecutionSession(id)
	}
	return nil
}

// SessionEvents returns the live stream events for one execution,
// filtered from the subscription's ring buffer (oldest first).
func (m *Model) SessionEvents(execID string) []*apiv1.StreamExecutionEventsResponse {
	if m.sub == nil {
		return nil
	}
	var out []*apiv1.StreamExecutionEventsResponse
	for _, ev := range m.sub.Events() {
		if ev.GetEvent().GetExecutionId() == execID {
			out = append(out, ev)
		}
	}
	return out
}

// RenderSession paints the merged live-session view into the open
// detail pane (durable parts + live events, phase-grouped).
func (m *Model) RenderSession(items []chat.ChatItem) {
	id := m.Base.DetailID()
	title := "Execution " + id
	fields := []screenkit.Field{
		{Key: "id", Value: id},
		{Key: "session events", Value: screenkit.FmtInt(len(items))},
	}
	if it, ok := m.Base.SourceItem("executions", id); ok {
		fields = append(fields, screenkit.Field{Key: "status", Value: it.Meta})
	}
	m.Base.SetDetailContent(title, fields, chat.RenderItems(items, m.Base.DetailWidth()))
}

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }
