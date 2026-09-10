// Package automation implements the Automation screen: workflows +
// schedules. Schedules have no dedicated service — they are recurring
// work items (work_item.proto recurring_schedule/next_run_at) listed via
// ListWorkItems with RecurringFilter_ONLY_RECURRING, exactly how the GUI
// surfaces them (frontend/src/api/workItems.ts recurringFilter).
package automation

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

// Model is the Automation screen.
type Model struct {
	screenkit.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamWorkflowEventsResponse]
	reconnected bool
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "automation"
	m.AddSource("workflows", "Workflows", m.fetchWorkflows)
	m.AddSource("schedules", "Schedules", m.fetchSchedules)
	m.SetDetail(m.detail)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "workflow-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "automation" }

// EnsureSubscriptions starts the workflow-events live stream once
// (idempotent; the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.WorkflowEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("workflow-events"))
}

func (m *Model) fetchWorkflows(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Workflows))
	for _, w := range resp.Msg.Workflows {
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetName(),
			Meta:  strings.ToLower(w.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchSchedules(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	onlyRecurring := apiv1.RecurringFilter_RECURRING_FILTER_ONLY_RECURRING
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        100,
		PageToken:       pageToken,
		RecurringFilter: onlyRecurring,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.WorkItems))
	for _, w := range resp.Msg.WorkItems {
		meta := "paused"
		if w.GetRecurringEnabled() {
			meta = "active"
		}
		if n := w.GetNextRunAt(); n != nil {
			meta += " · next " + screenkit.FmtTime(n)
		}
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetTitle(),
			Meta:  meta,
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case "workflows":
		resp, err := m.cl.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkflow()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "name", Value: w.GetName()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "type", Value: w.GetType()},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "current ver", Value: screenkit.FmtInt(int(w.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(w.GetUpdatedAt())},
		}
		// Version list trails in the body.
		var body string
		if vr, err := m.cl.Workflows.ListWorkflowVersions(ctx, connect.NewRequest(&apiv1.ListWorkflowVersionsRequest{WorkflowId: id})); err == nil {
			var b strings.Builder
			for _, v := range vr.Msg.GetVersions() {
				b.WriteString("v" + screenkit.FmtInt(int(v.GetVersion())) + "  " +
					strings.ToLower(v.GetStatus().String()) + "  " +
					v.GetVersionNote() + "\n")
			}
			body = strings.TrimRight(b.String(), "\n")
		}
		return "Workflow: " + w.GetName(), fields, body, nil

	case "schedules":
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "enabled", Value: screenkit.FmtBool(w.GetRecurringEnabled())},
			{Key: "next run", Value: screenkit.FmtTime(w.GetNextRunAt())},
			{Key: "workflow", Value: w.GetWorkflowId()},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}
		// Run history trails in the body.
		var body string
		if h, err := m.cl.WorkItems.GetWorkItemRunHistory(ctx, connect.NewRequest(&apiv1.GetWorkItemRunHistoryRequest{Id: id})); err == nil {
			var b strings.Builder
			for _, e := range h.Msg.GetEntries() {
				b.WriteString(screenkit.FmtTime(e.GetFireAt()) + "  " + e.GetStatus() + "\n")
			}
			body = strings.TrimRight(b.String(), "\n")
		}
		return "Schedule: " + w.GetTitle(), fields, body, nil
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
		cmd := m.reg.WaitStatus("workflow-events")
		if msg.Status == "open" && m.reconnected {
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
	return m.Base.Frame(b.String())
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg
// jumps); detail loads via RequestDetail when the item is not paged in.
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// ActiveSourceName / ActiveItem expose the Base focus state to the
// shell's context engine.
func (m *Model) ActiveSourceName() string           { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (screenkit.Item, bool) { return m.Base.ActiveItem() }
