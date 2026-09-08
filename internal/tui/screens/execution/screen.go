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
	return tea.Batch(m.Load(), m.reg.WaitStatus("execution-events"))
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

	case subs.StatusMsg:
		m.Base.SetStatus(msg.Name, string(msg.Status))
		cmd := m.reg.WaitStatus("execution-events")
		if msg.Status == "open" && m.reconnected {
			// reconnect gap: refetch lists (invalidate-on-reconnect)
			return m, tea.Batch(cmd, m.Load())
		}
		if msg.Status != "open" {
			m.reconnected = true
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
