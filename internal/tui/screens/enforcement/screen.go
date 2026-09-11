// Package enforcement implements the Enforcement screen: policies, pending
// step approvals, policy decisions, and recovery executions — with the WRITE
// paths the GUI exposes (approve/reject a pending step approval, create/edit/
// publish a policy and inspect version bodies, and the recovery action
// surface). StreamRecoveryEvents drives footer status (recovery rides the
// enforcement UX in the GUI).
//
// Chords (content focus, per focused source):
//
//	approvals   a approve (reason)      x reject (reason required)
//	policies    n new (rego body)       e edit draft      p publish
//	            v versions → enter inspects a version body
//	recoveries  a approve continuation plan   x reject plan (reason)
//	            c cancel recovery             m mark task succeeded
//
// Every write is Confirm-gated or form-gated, disables itself while in
// flight, reconciles its list afterwards, and surfaces a plane refusal
// verbatim in the composer dock (never a silent no-op).
package enforcement

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Model is the Enforcement screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamRecoveryEventsResponse]
	reconnected bool

	// Source detail caches: the list RPCs carry the rich rows and the
	// screen has no Get-RPC for approvals/decisions, so the list hit is the
	// detail source of truth.
	approvalItems map[string]*apiv1.ApprovalItem
	recoveries    map[string]*apiv1.RecoveryExecution
	planStatus    map[string]apiv1.PlanStatus
	planCache     map[string]*apiv1.ContinuationPlan

	// versions caches a policy's versions for the inspector (keyed by
	// version number); policyFor names the policy they belong to.
	versions  map[string]*apiv1.PolicyVersion
	policyFor string

	// Write-path state.
	ov       *overlay // the open modal (nil = none)
	actRef   string   // the entity the open overlay writes to
	inFlight string   // the action whose RPC is in flight ("" = idle)
	lastErr  string   // last refusal (also pushed to the dock)
	lastOK   string   // last success notice

	// decisionsFor caches the policy decisions that produced an approval
	// (the "/approvals" detail's policy context).
	decisionsFor map[string][]*apiv1.PolicyDecision
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{
		cl:            cl,
		reg:           reg,
		tenantID:      tenantID,
		approvalItems: map[string]*apiv1.ApprovalItem{},
		recoveries:    map[string]*apiv1.RecoveryExecution{},
		planStatus:    map[string]apiv1.PlanStatus{},
		planCache:     map[string]*apiv1.ContinuationPlan{},
		versions:      map[string]*apiv1.PolicyVersion{},
		decisionsFor:  map[string][]*apiv1.PolicyDecision{},
	}
	// Every write goes through the ONE mutation executor (dock feedback,
	// rollback, and the affected source's reconcile).
	m.Base.SetExecutor(&mutate.Executor{Sink: m})
	m.NameStr = "enforcement"
	m.AddSource("policies", "Policies", m.fetchPolicies)
	m.AddSource("approvals", "Pending Approvals", m.fetchApprovals)
	m.AddSource("decisions", "Decisions", m.fetchDecisions)
	m.AddSource("recoveries", "Recoveries", m.fetchRecoveries)
	m.SetDetail(m.detail)
	m.Base.SetStatuses([]screenkit.StatusMsg{
		{Name: "recovery-events", Status: "idle"},
	})
	return m
}

func (m *Model) Name() string { return "enforcement" }

// EnsureSubscriptions starts the recovery-events live stream once
// (idempotent; the shell calls it on every switch to this tab).
func (m *Model) EnsureSubscriptions() {
	if m.sub == nil {
		m.sub = m.reg.RecoveryEvents(m.cl, m.tenantID)
	}
}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) { m.Base.SetSize(w, h) }

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.Load(), m.reg.WaitStatus("recovery-events"))
}

// ClaimsKeys reports whether the screen owns the keyboard: while a modal
// (form / Confirm / picker / viewer) is open the shell must hand over every
// key verbatim, so a Rego body or reason containing q / d / y / ? is never
// eaten by a global chord.
func (m *Model) ClaimsKeys() bool { return m.ov != nil }

// activeID returns the focused source's selected item id ("" = none).
func (m *Model) activeID() string {
	it, ok := m.Base.ActiveItem()
	if !ok {
		return ""
	}
	return it.ID
}

// --- list panes ------------------------------------------------------------

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
			Meta:  policyStatusName(p.GetStatus()),
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
		// Cache the full ApprovalItem: it is the ONLY carrier of the
		// upstream context (summary / AC / touched files / reason) — v1 has
		// no GetApproval RPC.
		m.approvalItems[a.GetStepRunId()] = a
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
			Meta:  effectName(d.GetEffect()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchRecoveries(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	resp, err := m.cl.Recovery.ListRecoveries(ctx, connect.NewRequest(&apiv1.ListRecoveriesRequest{
		TenantId:  "",
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.Recoveries))
	for _, r := range resp.Msg.Recoveries {
		m.recoveries[r.GetId()] = r
		items = append(items, screenkit.Item{
			ID:    r.GetId(),
			Title: r.GetTaskId() + " · " + r.GetCurrentStep(),
			Meta:  strings.ToLower(r.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

// --- detail ----------------------------------------------------------------

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
			{Key: "status", Value: policyStatusName(p.GetStatus())},
			{Key: "current ver", Value: screenkit.FmtInt(int(p.GetCurrentVersion()))},
			{Key: "created", Value: screenkit.FmtTime(p.GetCreatedAt())},
			{Key: "updated", Value: screenkit.FmtTime(p.GetUpdatedAt())},
			{Key: "actions", Value: "n new · e edit draft · p publish · v versions"},
		}
		// The version trail in the body; the versions picker (v → enter)
		// inspects a single version's Rego body verbatim.
		var body string
		if vr, err := m.cl.Policies.ListPolicyVersions(ctx, connect.NewRequest(&apiv1.ListPolicyVersionsRequest{PolicyId: id})); err == nil {
			var b strings.Builder
			for _, v := range vr.Msg.GetVersions() {
				b.WriteString("v" + screenkit.FmtInt(int(v.GetVersion())) + "  " +
					versionStatusName(v.GetStatus()) + "  " +
					v.GetVersionNote() + "\n")
			}
			body = strings.TrimRight(b.String(), "\n")
		}
		return "Policy: " + p.GetName(), fields, body, nil

	case "approvals":
		// The list response carries the full ApprovalItem (no GetApproval
		// RPC in v1) — render its upstream context, plus the policy
		// decision(s) recorded for the step run.
		if a, ok := m.approvalItems[id]; ok {
			fields := []screenkit.Field{
				{Key: "step run", Value: a.GetStepRunId()},
				{Key: "workflow", Value: a.GetWorkflowName()},
				{Key: "project", Value: a.GetProjectName()},
				{Key: "work item", Value: a.GetWorkItemName()},
				{Key: "upstream worker", Value: a.GetUpstreamWorker()},
				{Key: "status", Value: strings.ToLower(a.GetStatus())},
				{Key: "created", Value: screenkit.FmtTime(a.GetCreatedAt())},
				{Key: "reason", Value: a.GetReason()},
				{Key: "attachments", Value: strings.Join(a.GetAttachmentNames(), ", ")},
				{Key: "actions", Value: "a approve · x reject"},
			}
			var b strings.Builder
			if s := a.GetUpstreamSummary(); s != "" {
				b.WriteString("upstream summary:\n" + s + "\n")
			}
			if ac := a.GetAcceptanceCriteria(); ac != "" {
				b.WriteString("\nacceptance criteria:\n" + ac + "\n")
			}
			if tf := a.GetTouchedFiles(); len(tf) > 0 {
				b.WriteString("\ntouched files:\n  " + strings.Join(tf, "\n  ") + "\n")
			}
			// Policy context: the decisions recorded against this step run.
			if decs, err := m.cl.Policies.ListDecisions(ctx, connect.NewRequest(&apiv1.ListDecisionsRequest{
				TargetType: "step_run",
				TargetId:   id,
				PageSize:   10,
			})); err == nil {
				m.decisionsFor[id] = decs.Msg.GetDecisions()
				if len(decs.Msg.GetDecisions()) > 0 {
					b.WriteString("\npolicy context:\n")
					for _, d := range decs.Msg.GetDecisions() {
						b.WriteString("  " + effectName(d.GetEffect()) + "  policy " + d.GetPolicyId() +
							" v" + screenkit.FmtInt(int(d.GetPolicyVersion())) + "  " + d.GetDecisionPoint() + "\n")
					}
				}
			}
			return "Approval " + a.GetStepRunId(), fields, strings.TrimRight(b.String(), "\n"), nil
		}
		return "Pending Approval", []screenkit.Field{{Key: "step run", Value: id}, {Key: "actions", Value: "a approve · x reject"}}, "", nil

	case "decisions":
		// ListDecisions carries full PolicyDecision rows; render a fresh
		// fetch when the row is not in the list cache.
		if resp, err := m.cl.Policies.GetDecision(ctx, connect.NewRequest(&apiv1.GetDecisionRequest{Id: id})); err == nil {
			d := resp.Msg.GetDecision()
			fields := []screenkit.Field{
				{Key: "id", Value: d.GetId()},
				{Key: "decision point", Value: d.GetDecisionPoint()},
				{Key: "effect", Value: effectName(d.GetEffect())},
				{Key: "policy", Value: d.GetPolicyId() + " v" + screenkit.FmtInt(int(d.GetPolicyVersion()))},
				{Key: "target", Value: d.GetTargetType() + " " + d.GetTargetId()},
				{Key: "actor", Value: d.GetActorType() + " " + d.GetActorId()},
				{Key: "trace id", Value: d.GetTraceId()},
				{Key: "occurred", Value: screenkit.FmtTime(d.GetOccurredAt())},
			}
			var b strings.Builder
			if d.GetInput() != "" {
				b.WriteString("input:\n" + d.GetInput() + "\n")
			}
			if d.GetResult() != "" {
				b.WriteString("\nresult:\n" + d.GetResult() + "\n")
			}
			if d.GetError() != "" {
				b.WriteString("\nerror: " + d.GetError() + "\n")
			}
			return "Decision " + d.GetId(), fields, strings.TrimRight(b.String(), "\n"), nil
		}
		if it, ok := m.SourceItem("decisions", id); ok {
			return "Decision", []screenkit.Field{
				{Key: "id", Value: it.ID},
				{Key: "point · target", Value: it.Title},
				{Key: "effect", Value: it.Meta},
			}, "", nil
		}
		return "Decision", []screenkit.Field{{Key: "id", Value: id}}, "", nil

	case "recoveries":
		r := m.recoveries[id]
		if r == nil {
			if resp, err := m.cl.Recovery.GetRecovery(ctx, connect.NewRequest(&apiv1.GetRecoveryRequest{Id: id})); err == nil {
				r = resp.Msg.GetRecovery()
				m.recoveries[id] = r
			}
		}
		if r == nil {
			return "Recovery " + id, []screenkit.Field{{Key: "id", Value: id}}, "", nil
		}
		// The continuation plan carries the human escalation state; cache
		// its status so the action surface knows what the plane allows.
		var plan *apiv1.ContinuationPlan
		if pr, err := m.cl.Recovery.GetContinuationPlan(ctx, connect.NewRequest(&apiv1.GetContinuationPlanRequest{RecoveryId: id})); err == nil {
			plan = pr.Msg.GetPlan()
		}
		m.planCache[id] = plan
		if plan != nil {
			m.planStatus[id] = plan.GetStatus()
		}
		fields := []screenkit.Field{
			{Key: "id", Value: r.GetId()},
			{Key: "status", Value: strings.ToLower(r.GetStatus().String())},
			{Key: "level", Value: strings.ToLower(r.GetLevel().String())},
			{Key: "current step", Value: r.GetCurrentStep()},
			{Key: "task", Value: r.GetTaskId()},
			{Key: "project", Value: r.GetProjectId()},
			{Key: "trigger", Value: r.GetTriggerReason()},
			{Key: "resumption", Value: r.GetResumptionPath()},
			{Key: "needs human", Value: screenkit.FmtBool(r.GetNeedsHumanApproval())},
			{Key: "budget", Value: screenkit.FmtInt64(r.GetBudgetTokensUsed()) + "/" + screenkit.FmtInt64(r.GetBudgetTokensLimit())},
			{Key: "triggered", Value: screenkit.FmtTime(r.GetTriggeredAt())},
			{Key: "ended", Value: screenkit.FmtTime(r.GetEndedAt())},
		}
		var acts []string
		if m.planStatus[id] == apiv1.PlanStatus_PLAN_STATUS_PENDING {
			acts = append(acts, "a approve plan", "x reject plan")
		}
		if m.recoveryActionAvailable(r, "cancel") == nil {
			acts = append(acts, "c cancel recovery")
		}
		if m.recoveryActionAvailable(r, "succeed") == nil {
			acts = append(acts, "m mark task succeeded")
		}
		if len(acts) == 0 {
			acts = append(acts, "(none available)")
		}
		fields = append(fields, screenkit.Field{Key: "actions", Value: strings.Join(acts, " · ")})
		var b strings.Builder
		if r.GetSummary() != "" {
			b.WriteString("summary:\n" + r.GetSummary() + "\n")
		}
		if plan != nil {
			b.WriteString("\ncontinuation plan v" + screenkit.FmtInt(int(plan.GetVersion())) +
				" (" + strings.ToLower(plan.GetStatus().String()) + ")\n")
			if plan.GetContextSummary() != "" {
				b.WriteString("  context: " + plan.GetContextSummary() + "\n")
			}
			if plan.GetRemaining() != "" {
				b.WriteString("  remaining: " + plan.GetRemaining() + "\n")
			}
		}
		return "Recovery " + r.GetId(), fields, strings.TrimRight(b.String(), "\n"), nil
	}
	return "", nil, "", nil
}

// --- update ----------------------------------------------------------------

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

	case mutate.Result:
		// One write's outcome: clear the in-flight guard, then let the kit2
		// base run the executor's Apply (dock feedback + reconcile).
		m.inFlight = ""
		return m, m.Base.HandleMutation(msg)

	case editLoadedMsg:
		return m, m.handleEditLoaded(msg)

	case versionsLoadedMsg:
		return m, m.handleVersionsLoaded(msg)

	case tea.KeyMsg:
		// The overlay owns every key while open (the shell has already
		// forwarded them verbatim via ClaimsKeys).
		if m.ov != nil {
			return m, m.handleOverlayKey(msg)
		}
		if handled, cmd := m.handleActionKey(msg); handled {
			return m, cmd
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

func (m *Model) View() string {
	if m.ov != nil {
		return m.Base.Frame(m.ov.view())
	}
	tail := []string{}
	if m.inFlight != "" {
		tail = append(tail, theme.HintText.Render("⏳ "+m.inFlight+" in flight — the action is disabled until the plane answers"))
	}
	if m.lastErr != "" {
		tail = append(tail, theme.ErrorText.Render("⚠ "+m.lastErr))
	} else if m.lastOK != "" {
		tail = append(tail, theme.HintText.Render("✓ "+m.lastOK))
	}
	hint := "enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh"
	switch m.Base.ActiveSourceName() {
	case "approvals":
		hint = "a: approve · x: reject (the detail shows the upstream + policy context)"
	case "policies":
		hint = "n: new policy · e: edit draft · p: publish · v: versions (enter inspects a body)"
	case "recoveries":
		hint = "a: approve plan · x: reject plan · c: cancel recovery · m: mark task succeeded (Confirm-gated)"
	}
	tail = append(tail, theme.HintText.Render(hint))

	// Reserve the last rows for the action hint + status so the frame can
	// never clip the chords out of view.
	body := strings.Split(m.Base.View(), "\n")
	if keep := len(body) - len(tail); keep > 1 {
		body = body[:keep]
	}
	return m.Base.Frame(strings.Join(body, "\n") + "\n" + strings.Join(tail, "\n"))
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
