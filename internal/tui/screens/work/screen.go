// Package work implements the Work screen: projects + work items
// (read-only), backed by ListProjects / ListWorkItems / GetProject /
// GetWorkItem, with the StreamProjectEvents subscription driving
// footer status + list refresh (useStream semantics).
package work

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Work screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamProjectEventsResponse]
	reconnected bool // saw a non-open status since last open
}

// New builds the screen. The project-events subscription lives for the
// screen's lifetime and is closed by Close (tab switch = unsubscribe).
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "work"
	m.AddSource("projects", "Projects", m.fetchProjects)
	m.AddSource("workitems", "Work Items", m.fetchWorkItems)
	m.SetDetail(m.detail)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "project-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "work" }

// EnsureSubscriptions starts the project-events live stream once
// (idempotent; the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.ProjectEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (useStream: navigating away unsubscribes).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("project-events"))
}

func (m *Model) fetchProjects(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Projects))
	for _, p := range resp.Msg.Projects {
		items = append(items, screenkit.Item{
			ID:    p.GetId(),
			Title: p.GetName(),
			Meta:  strings.ToLower(p.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchWorkItems(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.WorkItems))
	for _, w := range resp.Msg.WorkItems {
		meta := strings.ToLower(w.GetStatus().String())
		if w.GetRecurringSchedule() != nil {
			meta += " · recurring"
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
	case "projects":
		resp, err := m.cl.Projects.GetProject(ctx, connect.NewRequest(&apiv1.GetProjectRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		p := resp.Msg.GetProject()
		fields := []screenkit.Field{
			{Key: "id", Value: p.GetId()},
			{Key: "name", Value: p.GetName()},
			{Key: "slug", Value: p.GetSlug()},
			{Key: "status", Value: strings.ToLower(p.GetStatus().String())},
			{Key: "repo", Value: p.GetRepoSlug()},
			{Key: "project dir", Value: p.GetProjectDir()},
			{Key: "max concurrent", Value: screenkit.FmtInt(int(p.GetMaxConcurrentRuns()))},
			{Key: "created", Value: screenkit.FmtTime(p.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(p.GetUpdatedAt())},
		}
		return "Project: " + p.GetName(), fields, p.GetGoals(), nil

	case "workitems":
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "kind", Value: strings.ToLower(w.GetKind().String())},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "worker", Value: w.GetAssignedWorkerRef()},
			{Key: "workflow run", Value: w.GetWorkflowRunId()},
			{Key: "priority", Value: screenkit.FmtInt(int(w.GetPriority()))},
			{Key: "scheduled", Value: screenkit.FmtTime(w.GetScheduledStartAt())},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(w.GetUpdatedAt())},
		}
		var body string
		if d := w.GetDescription(); d != "" {
			body += d + "\n\n"
		}
		if ac := w.GetAcceptanceCriteria(); ac != "" {
			body += "Acceptance criteria:\n" + ac
		}
		return "Work Item: " + w.GetTitle(), fields, body, nil
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
		// Re-arm the status wait; a transition back to open after a drop
		// means events may have been missed — refetch page 1 (the hook's
		// "invalidate on reconnect" behavior).
		cmd := m.reg.WaitStatus("project-events")
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
