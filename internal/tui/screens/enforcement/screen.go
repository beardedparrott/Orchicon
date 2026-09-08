// Package enforcement implements the Enforcement screen: policies +
// pending step approvals (read-only), with StreamRecoveryEvents driving
// footer status (recovery rides the enforcement UX in the GUI).
package enforcement

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Enforcement screen.
type Model struct {
	screenkit.Base
	cl          *client.Clients
	reg         *subs.Registry
	reconnected bool
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry) *Model {
	m := &Model{cl: cl, reg: reg}
	m.NameStr = "enforcement"
	m.AddSource("policies", "Policies", m.fetchPolicies)
	m.AddSource("approvals", "Pending Approvals", m.fetchApprovals)
	m.AddSource("decisions", "Decisions", m.fetchDecisions)
	m.SetDetail(m.detail)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "recovery-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "enforcement" }

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("recovery-events"))
}

func (m *Model) fetchPolicies(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Policies.ListPolicies(ctx, connect.NewRequest(&apiv1.ListPoliciesRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Policies))
	for _, p := range resp.Msg.Policies {
		items = append(items, screenkit.Item{
			ID:    p.GetId(),
			Title: p.GetName(),
			Meta:  strings.ToLower(p.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchApprovals(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Approvals.ListPendingStepApprovals(ctx, connect.NewRequest(&apiv1.ListPendingStepApprovalsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Items))
	for _, a := range resp.Msg.Items {
		items = append(items, screenkit.Item{
			ID:    a.GetStepRunId(),
			Title: a.GetWorkflowName() + " → " + a.GetProjectName(),
			Meta:  strings.ToLower(a.GetStatus()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchDecisions(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Policies.ListDecisions(ctx, connect.NewRequest(&apiv1.ListDecisionsRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Decisions))
	for _, d := range resp.Msg.Decisions {
		items = append(items, screenkit.Item{
			ID:    d.GetId(),
			Title: d.GetDecisionPoint() + " · " + d.GetTargetType(),
			Meta:  strings.ToLower(d.GetEffect().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case "policies":
		resp, err := m.cl.Policies.GetPolicy(ctx, connect.NewRequest(&apiv1.GetPolicyRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		p := resp.Msg.GetPolicy()
		fields := []screenkit.Field{
			{Key: "id", Value: p.GetId()},
			{Key: "name", Value: p.GetName()},
			{Key: "status", Value: strings.ToLower(p.GetStatus().String())},
			{Key: "current ver", Value: screenkit.FmtInt(int(p.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(p.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(p.GetUpdatedAt())},
		}
		// Version list trails in the body.
		var body string
		if vr, err := m.cl.Policies.ListPolicyVersions(ctx, connect.NewRequest(&apiv1.ListPolicyVersionsRequest{PolicyId: id})); err == nil {
			var b strings.Builder
			for _, v := range vr.Msg.GetVersions() {
				b.WriteString("v" + screenkit.FmtInt(int(v.GetVersion())) + "  " +
					strings.ToLower(v.GetStatus().String()) + "  " +
					v.GetVersionNote() + "\n")
			}
			body = strings.TrimRight(b.String(), "\n")
		}
		return "Policy: " + p.GetName(), fields, body, nil

	case "approvals":
		// The list response carries the full ApprovalItem; refetch is not
		// available (no GetApproval RPC) — read-only v1 shows what the list
		// returned. Detail renders from the last list hit.
		if it, ok := m.SourceItem("approvals", id); ok {
			fields := []screenkit.Field{
				{Key: "step run", Value: it.ID},
				{Key: "workflow → project", Value: it.Title},
				{Key: "status", Value: it.Meta},
			}
			return "Pending Approval", fields, "", nil
		}
		return "Pending Approval", []screenkit.Field{{Key: "id", Value: id}}, "", nil

	case "decisions":
		// Same shape: ListDecisions carries full PolicyDecision rows; there
		// is no GetDecision Get-RPC in v1 — render from list data.
		if it, ok := m.SourceItem("decisions", id); ok {
			return "Decision", []screenkit.Field{
				{Key: "id", Value: it.ID},
				{Key: "point · target", Value: it.Title},
				{Key: "effect", Value: it.Meta},
			}, "", nil
		}
		return "Decision", []screenkit.Field{{Key: "id", Value: id}}, "", nil
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
		cmd := m.reg.WaitStatus("recovery-events")
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
	b.WriteString(theme.HintText.Render("enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh (approvals/decisions render from list data — no Get-RPC in v1)"))
	return b.String()
}
