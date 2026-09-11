// Package automation implements the Automation screen: workflows,
// recurring work items (the Recurring Items surface) and the Idea Cloud
// triage list.
//
// Recurring items have no dedicated service — they are work items with a
// recurring_schedule (work_item.proto recurring_schedule/next_run_at)
// listed via ListWorkItems with RecurringFilter_ONLY_RECURRING, exactly how
// the GUI surfaces them (frontend/src/api/workItems.ts recurringFilter).
// Their mutations are WorkItemService.CreateWorkItem / UpdateWorkItem
// (recurring_schedule + recurring_enabled) / DeleteWorkItem. Per-fire run
// history comes from GetWorkItemRunHistory.
//
// Ideas are idea-state work items surfaced by ListIdeas: idea_state_scope
// ACTIVE is the Idea Cloud (awaiting triage), REJECTED is the durable
// dismissed-spawn history the automation dedupe gate consults. PromoteIdea
// is the ONLY sanctioned path out of idea state (→ normal pending work
// item); DismissIdea maps to cancelled (leave every active view).
package automation

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/stream"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Source names (also the slash-command slugs the shell generates).
const (
	srcWorkflows = "workflows"
	srcSchedules = "schedules"
	srcIdeas     = "ideas"
	srcRejected  = "rejected"
)

// Form modes + confirm actions.
const (
	formCreate = "create"
	formEdit   = "edit"

	actDelete  = "delete"
	actDismiss = "dismiss"
	actPause   = "pause"
	actPromote = "promote"
)

// projectOpt / workflowOpt are the form's select options.
type projectOpt struct{ ID, Name string }
type workflowOpt struct{ ID, Name string }

// confirmState is the inline confirmation an irreversible-ish gesture
// (delete a recurring item, dismiss an idea) requires before it fires.
type confirmState struct {
	action string
	id     string
	title  string
}

func (c *confirmState) prompt() string {
	switch c.action {
	case actDelete:
		return "delete recurring item " + strconv.Quote(c.title) + "? (soft delete → cancelled)"
	case actDismiss:
		return "dismiss idea " + strconv.Quote(c.title) + "? it leaves the Idea Cloud and is kept as rejected history"
	}
	return "confirm?"
}

// Model is the Automation screen.
type Model struct {
	screenkit.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamWorkflowEventsResponse]
	reconnected bool

	projects  []projectOpt
	workflows []workflowOpt

	form     *screenkit.Form
	formMode string
	formID   string

	confirm *confirmState
	notice  string
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "automation"
	m.AddSource(srcWorkflows, "Workflows", m.fetchWorkflows)
	m.AddSource(srcSchedules, "Recurring Items", m.fetchSchedules)
	m.AddSource(srcIdeas, "Idea Cloud", m.fetchIdeas)
	m.AddSource(srcRejected, "Rejected Ideas", m.fetchRejected)
	m.SetDetail(m.detail)
	m.Base.SetEmpty(srcWorkflows, "no workflows yet — define one to bind a recurring item to")
	m.Base.SetEmpty(srcSchedules, "no recurring items yet — press n to create one")
	m.Base.SetEmpty(srcIdeas, "no ideas awaiting triage — automations whose outputs mode is 'idea' spawn them here")
	m.Base.SetEmpty(srcRejected, "no dismissed ideas — every dismissal is kept here as durable rejection history")
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

// ClaimsKeys reports whether the screen owns every key right now (an open
// form). The shell consults it before its own routes so a typed character
// is never stolen ('q' would quit, space would open the tab menu).
func (m *Model) ClaimsKeys() bool { return m.form != nil }

// ActiveForm returns the open form (nil when closed) — the shell/tests
// read the in-progress input through it.
func (m *Model) ActiveForm() *screenkit.Form { return m.form }

// Notice returns the last action's status line ("" = none).
func (m *Model) Notice() string { return m.notice }

// ConfirmPrompt returns the pending confirmation prompt ("" = none).
func (m *Model) ConfirmPrompt() string {
	if m.confirm == nil {
		return ""
	}
	return m.confirm.prompt()
}

// ---------------- fetches ----------------

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
		if s := w.GetRecurringSchedule(); s != nil {
			meta += " · " + cadence(s)
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

func (m *Model) fetchIdeas(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	return m.fetchIdeaScope(ctx, pageToken, apiv1.IdeaStateScope_IDEA_STATE_SCOPE_ACTIVE)
}

func (m *Model) fetchRejected(ctx context.Context, pageToken string) ([]screenkit.Item, string, error) {
	return m.fetchIdeaScope(ctx, pageToken, apiv1.IdeaStateScope_IDEA_STATE_SCOPE_REJECTED)
}

func (m *Model) fetchIdeaScope(ctx context.Context, pageToken string, scope apiv1.IdeaStateScope) ([]screenkit.Item, string, error) {
	resp, err := m.cl.WorkItems.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{
		PageSize:       100,
		PageToken:      pageToken,
		IdeaStateScope: scope,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]screenkit.Item, 0, len(resp.Msg.GetIdeas()))
	for _, w := range resp.Msg.GetIdeas() {
		meta := strings.ToLower(w.GetStatus().String())
		switch {
		case w.GetSpawnedByTitle() != "":
			meta += " · from " + w.GetSpawnedByTitle()
		case w.GetSpawnedBy() != "":
			meta += " · from " + shortID(w.GetSpawnedBy())
		}
		if scope == apiv1.IdeaStateScope_IDEA_STATE_SCOPE_REJECTED {
			meta = "dismissed · " + meta
		}
		items = append(items, screenkit.Item{
			ID:    w.GetId(),
			Title: w.GetTitle(),
			Meta:  meta,
		})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// ---------------- detail ----------------

func (m *Model) detail(ctx context.Context, src, id string) (string, []screenkit.Field, string, error) {
	switch src {
	case srcWorkflows:
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

	case srcSchedules:
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		state := "paused"
		if w.GetRecurringEnabled() {
			state = "active"
		}
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "recurring", Value: state},
			{Key: "cadence", Value: cadence(w.GetRecurringSchedule())},
			{Key: "next fire", Value: screenkit.FmtTime(w.GetNextRunAt())},
			{Key: "workflow", Value: w.GetWorkflowId()},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}
		body := m.runHistory(ctx, id)
		return "Recurring item: " + w.GetTitle(), fields, body, nil

	case srcIdeas, srcRejected:
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		fields := []screenkit.Field{
			{Key: "id", Value: w.GetId()},
			{Key: "title", Value: w.GetTitle()},
			{Key: "status", Value: strings.ToLower(w.GetStatus().String())},
			{Key: "kind", Value: strings.ToLower(w.GetKind().String())},
			{Key: "priority", Value: screenkit.FmtInt(int(w.GetPriority()))},
			{Key: "project", Value: w.GetProjectId()},
			{Key: "spawned by", Value: w.GetSpawnedBy()},
			{Key: "spawned by title", Value: w.GetSpawnedByTitle()},
			{Key: "spawn run", Value: w.GetSpawnedByRunId()},
			{Key: "created", Value: screenkit.FmtTime(w.GetCreatedAt())},
		}
		var body strings.Builder
		if d := w.GetDescription(); d != "" {
			body.WriteString(d + "\n")
		}
		body.WriteString("\nspawned by automation: " + orDash(dash(w.GetSpawnedByTitle(), w.GetSpawnedBy())) +
			" (run " + orDash(w.GetSpawnedByRunId()) + ")\n")
		if src == srcIdeas {
			body.WriteString("triage: p promote (becomes a normal pending work item) · x dismiss (→ cancelled, kept as rejected history)\n")
		} else {
			body.WriteString("dismissed ideas are kept as durable rejection history — the automation dedupe gate reads them before spawning again.\n")
		}
		return "Idea: " + w.GetTitle(), fields, strings.TrimRight(body.String(), "\n"), nil
	}
	return "", nil, "", nil
}

// runHistory renders the per-fire ledger: each fire's status + fire time,
// the bound workflow run (id + status), and that run's executions/outputs.
func (m *Model) runHistory(ctx context.Context, id string) string {
	h, err := m.cl.WorkItems.GetWorkItemRunHistory(ctx, connect.NewRequest(&apiv1.GetWorkItemRunHistoryRequest{Id: id}))
	if err != nil {
		return "run history: " + err.Error()
	}
	entries := h.Msg.GetEntries()
	if len(entries) == 0 {
		return "run history: no fires yet — the scheduler fires this item at its next occurrence"
	}
	var b strings.Builder
	b.WriteString("run history (newest first)\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s  fire=%s", screenkit.FmtTime(e.GetFireAt()), e.GetStatus())
		if e.GetWorkflowRunId() != "" {
			fmt.Fprintf(&b, "  run=%s (%s)", shortID(e.GetWorkflowRunId()), orDash(e.GetRunStatus()))
			if e.GetRunStartedAt() != nil {
				fmt.Fprintf(&b, "  %s → %s", screenkit.FmtTime(e.GetRunStartedAt()), screenkit.FmtTime(e.GetRunEndedAt()))
			}
		}
		if e.GetError() != "" {
			b.WriteString("  error: " + e.GetError())
		}
		b.WriteString("\n")
		for _, x := range e.GetExecutions() {
			fmt.Fprintf(&b, "    exec %s  %s  step=%s  %s\n",
				shortID(x.GetId()), orDash(x.GetStatus()), orDash(x.GetStepId()), oneLine(x.GetOutput()))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------- form building ----------------

// formReadyMsg carries either the freshly loaded option lists (create) or
// the item being edited back to the UI thread.
type formReadyMsg struct {
	mode      string
	item      *apiv1.WorkItem
	projects  []projectOpt
	workflows []workflowOpt
	err       error
}

// actionDoneMsg reports the outcome of a mutation.
type actionDoneMsg struct {
	action  string
	notice  string
	errText string
}

func (m *Model) prepCreate() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		msg := formReadyMsg{mode: formCreate}
		pr, err := cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{PageSize: 100}))
		if err != nil {
			msg.err = err
			return msg
		}
		for _, p := range pr.Msg.GetProjects() {
			msg.projects = append(msg.projects, projectOpt{ID: p.GetId(), Name: p.GetName()})
		}
		if wr, err := cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{PageSize: 100})); err == nil {
			for _, w := range wr.Msg.GetWorkflows() {
				msg.workflows = append(msg.workflows, workflowOpt{ID: w.GetId(), Name: w.GetName()})
			}
		}
		return msg
	}
}

func (m *Model) prepEdit() tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return formReadyMsg{mode: formEdit, err: err}
		}
		return formReadyMsg{mode: formEdit, item: resp.Msg.GetWorkItem()}
	}
}

func (m *Model) workflowNames() []string {
	out := make([]string, 0, len(m.workflows))
	for _, w := range m.workflows {
		out = append(out, w.Name)
	}
	return out
}

func (m *Model) newCreateForm() *screenkit.Form {
	projOpts := make([]string, 0, len(m.projects))
	for _, p := range m.projects {
		projOpts = append(projOpts, p.Name)
	}
	wfOpts := append([]string{"none"}, m.workflowNames()...)
	start := projOpts[0]
	now := time.Now().UTC()
	return screenkit.NewForm("New recurring item", []screenkit.FormField{
		{Key: "title", Label: "title", Required: true, Hint: "what the recurring item does"},
		{Key: "project", Label: "project", Options: projOpts, Value: start, Required: true},
		{Key: "kind", Label: "kind", Options: []string{"task", "feature", "subtask"}, Value: "task"},
		{Key: "workflow", Label: "workflow", Options: wfOpts},
		{Key: "frequency", Label: "frequency", Options: []string{"daily", "hourly", "weekly", "monthly", "minute"}, Value: "daily"},
		{Key: "interval", Label: "interval", Value: "1", Required: true, Validate: validateInterval, Hint: "every N periods, e.g. 2 = every 2 days"},
		{Key: "days", Label: "days", Validate: validateDays, Hint: "Mon,Wed,Fri — empty = every day"},
		{Key: "start_date", Label: "start date", Value: now.Format("2006-01-02"), Required: true, Validate: validateDate},
		{Key: "start_time", Label: "start time", Value: "09:00", Required: true, Validate: validateClock, Hint: "HH:MM, 24h"},
		{Key: "outputs", Label: "outputs", Options: []string{"standard", "idea", "none"}, Value: "standard", Hint: "idea = each fire's items await triage in the Idea Cloud"},
	})
}

func (m *Model) newEditForm(w *apiv1.WorkItem) *screenkit.Form {
	s := w.GetRecurringSchedule()
	if s == nil {
		return nil
	}
	enabled := "yes"
	if !w.GetRecurringEnabled() {
		enabled = "no"
	}
	return screenkit.NewForm("Edit recurring item", []screenkit.FormField{
		{Key: "title", Label: "title", Value: w.GetTitle(), Required: true},
		{Key: "frequency", Label: "frequency", Options: []string{"daily", "hourly", "weekly", "monthly", "minute"}, Value: inOptions(s.GetFrequency(), []string{"daily", "hourly", "weekly", "monthly", "minute"}, "daily")},
		{Key: "interval", Label: "interval", Value: strconv.Itoa(int(maxInt32(s.GetInterval(), 1))), Required: true, Validate: validateInterval},
		{Key: "days", Label: "days", Value: strings.Join(s.GetDays(), ","), Validate: validateDays, Hint: "Mon,Wed,Fri — empty = every day"},
		{Key: "start_date", Label: "start date", Value: s.GetStartDate(), Required: true, Validate: validateDate},
		{Key: "start_time", Label: "start time", Value: s.GetStartTime(), Required: true, Validate: validateClock},
		{Key: "outputs", Label: "outputs", Options: []string{"standard", "idea", "none"}, Value: inOptions(s.GetOutputsMode(), []string{"standard", "idea", "none"}, "standard")},
		{Key: "enabled", Label: "enabled", Options: []string{"yes", "no"}, Value: enabled, Hint: "no = paused: keeps the schedule, stops the due-scan"},
	})
}

// ---------------- mutations ----------------

// submitForm turns the open, validated form into its mutation RPC.
func (m *Model) submitForm() tea.Cmd {
	if m.form == nil {
		return nil
	}
	if err := m.form.Validate(); err != nil {
		return nil // stays open with Err set
	}
	f := m.form
	cl := m.cl
	switch m.formMode {
	case formCreate:
		projID := ""
		for _, p := range m.projects {
			if p.Name == f.Value("project") {
				projID = p.ID
			}
		}
		if projID == "" {
			m.notice = "unknown project " + strconv.Quote(f.Value("project"))
			return nil
		}
		wfID := ""
		if name := f.Value("workflow"); name != "" && name != "none" {
			for _, w := range m.workflows {
				if w.Name == name {
					wfID = w.ID
				}
			}
		}
		req := &apiv1.CreateWorkItemRequest{
			ProjectId:         projID,
			Kind:              kindFromForm(f.Value("kind")),
			Title:             strings.TrimSpace(f.Value("title")),
			WorkflowId:        wfID,
			RecurringSchedule: scheduleFromForm(f),
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			resp, err := cl.WorkItems.CreateWorkItem(ctx, connect.NewRequest(req))
			if err != nil {
				return actionDoneMsg{action: formCreate, errText: err.Error()}
			}
			return actionDoneMsg{action: formCreate, notice: "created recurring item " + strconv.Quote(resp.Msg.GetWorkItem().GetTitle()) + " · " + cadence(req.GetRecurringSchedule())}
		}
	case formEdit:
		title := strings.TrimSpace(f.Value("title"))
		sched := scheduleFromForm(f)
		enabled := f.Value("enabled") != "no"
		id := m.formID
		req := &apiv1.UpdateWorkItemRequest{
			Id:                id,
			Title:             &title,
			RecurringSchedule: sched,
			RecurringEnabled:  &enabled,
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req)); err != nil {
				return actionDoneMsg{action: formEdit, errText: err.Error()}
			}
			return actionDoneMsg{action: formEdit, notice: "saved " + strconv.Quote(title) + " · " + cadence(sched)}
		}
	}
	return nil
}

// togglePause flips a recurring item's recurring_enabled (pause ⇄ resume).
// The current state is read server-side so the toggle is never guessed.
func (m *Model) togglePause() tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id, title := it.ID, it.Title
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cur, err := cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return actionDoneMsg{action: actPause, errText: err.Error()}
		}
		if cur.Msg.GetWorkItem().GetRecurringSchedule() == nil {
			return actionDoneMsg{action: actPause, errText: "not a recurring item"}
		}
		next := !cur.Msg.GetWorkItem().GetRecurringEnabled()
		if _, err := cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
			Id:               id,
			RecurringEnabled: &next,
		})); err != nil {
			return actionDoneMsg{action: actPause, errText: err.Error()}
		}
		verb := "paused"
		if next {
			verb = "resumed"
		}
		return actionDoneMsg{action: actPause, notice: verb + " " + strconv.Quote(title)}
	}
}

func (m *Model) promoteIdea() tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id, title := it.ID, it.Title
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := cl.WorkItems.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: id})); err != nil {
			return actionDoneMsg{action: actPromote, errText: err.Error()}
		}
		return actionDoneMsg{action: actPromote, notice: "promoted " + strconv.Quote(title) + " — it is now a pending work item"}
	}
}

// runConfirm executes the confirmed gesture.
func (m *Model) runConfirm() tea.Cmd {
	c := m.confirm
	m.confirm = nil
	if c == nil {
		return nil
	}
	cl := m.cl
	switch c.action {
	case actDelete:
		id, title := c.id, c.title
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.WorkItems.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: id})); err != nil {
				return actionDoneMsg{action: actDelete, errText: err.Error()}
			}
			return actionDoneMsg{action: actDelete, notice: "deleted " + strconv.Quote(title)}
		}
	case actDismiss:
		id, title := c.id, c.title
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := cl.WorkItems.DismissIdea(ctx, connect.NewRequest(&apiv1.DismissIdeaRequest{Id: id})); err != nil {
				return actionDoneMsg{action: actDismiss, errText: err.Error()}
			}
			return actionDoneMsg{action: actDismiss, notice: "dismissed " + strconv.Quote(title) + " — kept as rejected history"}
		}
	}
	return nil
}

// ---------------- key handling ----------------

// handleKey implements the screen's own keys. Returns (cmd, handled):
// handled=false means the key falls through to the shared list/detail
// navigation.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if m.form != nil {
		submitted, cancelled := m.form.Update(msg)
		if cancelled {
			m.form = nil
			m.notice = "cancelled"
			return nil, true
		}
		if submitted {
			return m.submitForm(), true
		}
		return nil, true
	}
	if m.confirm != nil {
		switch msg.String() {
		case "y", "Y":
			return m.runConfirm(), true
		case "esc", "n", "N":
			m.confirm = nil
			m.notice = "cancelled"
			return nil, true
		}
		return nil, true // the confirmation owns every key
	}
	src := m.ActiveSourceName()
	switch msg.String() {
	case "n":
		if src == srcSchedules {
			return m.prepCreate(), true
		}
	case "e":
		if src == srcSchedules {
			return m.prepEdit(), true
		}
	case "p":
		switch src {
		case srcSchedules:
			return m.togglePause(), true
		case srcIdeas:
			return m.promoteIdea(), true
		}
	case "x":
		if it, ok := m.ActiveItem(); ok {
			switch src {
			case srcSchedules:
				m.confirm = &confirmState{action: actDelete, id: it.ID, title: it.Title}
				return nil, true
			case srcIdeas:
				m.confirm = &confirmState{action: actDismiss, id: it.ID, title: it.Title}
				return nil, true
			}
		}
	case "r":
		return m.Load(), true
	}
	return nil, false
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

	case formReadyMsg:
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		switch msg.mode {
		case formCreate:
			if len(msg.projects) == 0 {
				m.notice = "no projects yet — a recurring item belongs to a project"
				return m, nil
			}
			m.projects, m.workflows = msg.projects, msg.workflows
			m.form = m.newCreateForm()
		case formEdit:
			if m.form = m.newEditForm(msg.item); m.form == nil {
				m.notice = "this item is not recurring — there is no recurrence to edit"
				return m, nil
			}
		}
		m.formMode, m.formID = msg.mode, msg.item.GetId()
		m.notice = ""
		return m, nil

	case actionDoneMsg:
		if msg.errText != "" {
			m.notice = msg.action + " failed: " + msg.errText
			return m, nil
		}
		m.form = nil
		m.notice = msg.notice
		// The plane is the source of truth: a mutated item must leave/enter
		// its scoped lists (created → Recurring Items, promoted → no longer
		// an idea, dismissed → active views drop it), so refetch every pane.
		return m, m.Load()

	case tea.KeyMsg:
		if cmd, handled := m.handleKey(msg); handled {
			return m, cmd
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

func (m *Model) View() string {
	if m.form != nil {
		w, _ := m.Base.Size()
		var b strings.Builder
		b.WriteString(m.form.View())
		b.WriteString("\n")
		b.WriteString(theme.HintText.Render("ctrl+s: save · ↑/↓ or tab: field · ←/→ or space: option · backspace: edit · esc: cancel"))
		out := b.String()
		if w > 0 {
			out = " " + strings.ReplaceAll(out, "\n", "\n ")
		}
		return m.Base.Frame(out)
	}
	var b strings.Builder
	b.WriteString(m.Base.View())
	b.WriteString("\n")
	if m.confirm != nil {
		b.WriteString(theme.StatusWarn.Render("⚠ "+m.confirm.prompt()+" — y: confirm · esc: cancel") + "\n")
	}
	if m.notice != "" {
		b.WriteString(theme.HintText.Render(m.notice) + "\n")
	}
	b.WriteString(theme.HintText.Render(m.hints()))
	return m.Base.Frame(b.String())
}

func (m *Model) hints() string {
	switch m.ActiveSourceName() {
	case srcSchedules:
		return "n: new recurring item · e: edit · p: pause/resume · x: delete · enter: detail (run history) · f: more pages"
	case srcIdeas:
		return "p: promote (→ work item) · x: dismiss (confirm) · ←/→ or h/l: pane · enter: detail · r: refresh"
	case srcRejected:
		return "rejected history — automations consult it before re-spawning · ←/→ or h/l: pane · enter: detail"
	default:
		return "enter: detail focus · ←/→ or h/l: pane · f: more pages · r: refresh"
	}
}

// ---------------- form helpers ----------------

func scheduleFromForm(f *screenkit.Form) *apiv1.RecurringSchedule {
	interval, _ := strconv.Atoi(strings.TrimSpace(f.Value("interval")))
	if interval < 1 {
		interval = 1
	}
	return &apiv1.RecurringSchedule{
		Frequency:   strings.TrimSpace(f.Value("frequency")),
		Interval:    int32(interval),
		Days:        splitDays(f.Value("days")),
		StartDate:   strings.TrimSpace(f.Value("start_date")),
		StartTime:   strings.TrimSpace(f.Value("start_time")),
		OutputsMode: strings.TrimSpace(f.Value("outputs")),
	}
}

func kindFromForm(v string) apiv1.WorkItemKind {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "feature":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE
	case "subtask":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK
	default:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_TASK
	}
}

var weekdays = map[string]bool{"Mon": true, "Tue": true, "Wed": true, "Thu": true, "Fri": true, "Sat": true, "Sun": true}

// splitDays normalizes a "mon,wed" list to ["Mon","Wed"] (canonical
// weekday spellings — the proto's days[] vocabulary).
func splitDays(v string) []string {
	var out []string
	for _, raw := range strings.Split(v, ",") {
		d := strings.TrimSpace(raw)
		if d == "" {
			continue
		}
		d = strings.ToUpper(d[:1]) + strings.ToLower(d[1:])
		out = append(out, d)
	}
	return out
}

func validateInterval(v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("must be a whole number")
	}
	if n < 1 {
		return fmt.Errorf("must be >= 1")
	}
	return nil
}

func validateDate(v string) error {
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("use YYYY-MM-DD")
	}
	return nil
}

func validateClock(v string) error {
	if _, err := time.Parse("15:04", strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("use HH:MM (24h)")
	}
	return nil
}

func validateDays(v string) error {
	for _, d := range splitDays(v) {
		if !weekdays[d] {
			return fmt.Errorf("%q is not a weekday (Mon,Tue,Wed,Thu,Fri,Sat,Sun)", d)
		}
	}
	return nil
}

// cadence renders a recurrence definition for list/detail context.
func cadence(s *apiv1.RecurringSchedule) string {
	if s == nil {
		return "—"
	}
	freq := s.GetFrequency()
	if freq == "" {
		freq = "daily"
	}
	out := "every " + freq
	if n := s.GetInterval(); n > 1 {
		out = fmt.Sprintf("every %d %s", n, freq)
	}
	if d := s.GetDays(); len(d) > 0 {
		out += " on " + strings.Join(d, ",")
	}
	if t := s.GetStartTime(); t != "" {
		out += " at " + t
	}
	if m := s.GetOutputsMode(); m != "" && m != "standard" {
		out += " (" + m + " outputs)"
	}
	return out
}

func inOptions(v string, opts []string, fallback string) string {
	for _, o := range opts {
		if o == v {
			return v
		}
	}
	return fallback
}

func maxInt32(v, floor int32) int32 {
	if v < floor {
		return floor
	}
	return v
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func dash(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// oneLine collapses an execution output to one bounded line (the ledger is
// a scan-list, not a log viewer).
func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if s == "" {
		return "—"
	}
	if len(s) > 90 {
		return s[:89] + "…"
	}
	return s
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
