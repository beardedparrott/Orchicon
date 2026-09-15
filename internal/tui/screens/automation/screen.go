// Package automation implements the Automation screen: workflows,
// recurring work items (the Recurring Items surface) and the Idea Cloud
// triage list.
//
// Recurring items have no dedicated service — they are work items with a
// recurring_schedule (work_item.proto recurring_schedule/next_run_at)
// listed via ListWorkItems with RecurringFilter_ONLY_RECURRING, exactly how
// the GUI surfaces them (frontend/src/api/workItems.ts recurringFilter).
// Their mutations are WorkItemService.CreateWorkItem / UpdateWorkItem
// (recurring_schedule + recurring_enabled) / DeleteWorkItem, all through
// the kit2 mutation executor. Per-fire run history comes from
// GetWorkItemRunHistory.
//
// Ideas are idea-state work items surfaced by ListIdeas: idea_state_scope
// ACTIVE is the Idea Cloud (awaiting triage), REJECTED is the durable
// dismissed-spawn history the automation dedupe gate consults. PromoteIdea
// is the ONLY sanctioned path out of idea state (→ normal pending work
// item); DismissIdea maps to cancelled (leave every active view).
package automation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// Source names (also the slash-command slugs the shell generates).
const (
	srcWorkflows = "workflows"
	srcSchedules = "schedules"
	srcIdeas     = "ideas"
	srcRejected  = "rejected"
)

// Form modes.
const (
	formCreate = "create"
	formEdit   = "edit"
)

// projectOpt / workflowOpt are the form's select options.
type projectOpt struct{ ID, Name string }
type workflowOpt struct{ ID, Name string }

// Model is the Automation screen.
type Model struct {
	kit2.Base
	cl          *client.Clients
	reg         *subs.Registry
	tenantID    string // "" lets the plane resolve it from the credential
	sub         *stream.Sub[*apiv1.StreamWorkflowEventsResponse]
	reconnected bool

	w, h int

	projects  []projectOpt
	workflows []workflowOpt

	// form is the open typed form (create/edit a recurring item).
	form     *kit2.Form
	formMode string
	formID   string
	// datePicker is the open calendar modal (a KDate field activated), layered
	// above the form that opened it. dateField is the field it writes back into.
	datePicker *kit2.DatePicker
	dateField  string
	// rpcPromote/rpcDismiss are thunks so a bulk triage is testable without a
	// plane, and so a partial failure can be reported per-idea.
	rpcPromote func(ctx context.Context, id string) error
	rpcDismiss func(ctx context.Context, id string) error
	// pending is the action the open confirmation dialog will run.
	pending *kit2.Action
	bar     *kit2.ActionBar

	notice string
}

// New builds the screen.
func New(cl *client.Clients, reg *subs.Registry, tenantID string) *Model {
	m := &Model{cl: cl, reg: reg, tenantID: tenantID}
	m.NameStr = "automation"
	// Workflows are NOT a source here: they belong to the Execution domain (the
	// operator's "Workflows should be under Execution not Automation"). Automation
	// keeps the recurring items that BIND a workflow — its create form still
	// fetches workflow options for the binding field.
	m.AddSource(srcSchedules, "Recurring Items", m.fetchSchedules)
	m.AddSource(srcIdeas, "Idea Cloud", m.fetchIdeas)
	m.AddSource(srcRejected, "Rejected Ideas", m.fetchRejected)
	m.SetDetail(m.detail)
	m.Base.SetSourceEmpty(srcSchedules, "no recurring items yet — press n to create one")
	m.Base.SetSourceEmpty(srcIdeas, "no ideas awaiting triage — automations whose outputs mode is 'idea' spawn them here")
	m.Base.SetSourceEmpty(srcRejected, "no dismissed ideas — every dismissal is kept here as durable rejection history")
	m.bar = kit2.NewActionBar()
	m.rpcPromote = m.defaultPromote
	m.rpcDismiss = m.defaultDismiss
	return m
}

func (m *Model) Name() string { return "automation" }

// EnsureSubscriptions: automation has no live stream of its own. Workflow
// events moved to Execution with the Workflows source.
func (m *Model) EnsureSubscriptions() {}

// Close unsubscribes (tab switch = unsubscribe).
func (m *Model) Close() { m.reg.CloseAll() }

func (m *Model) SetSize(w, h int) {
	m.w, m.h = w, h
	m.Base.SetSize(w, h)
	// A modal must follow a resize while it is up, or it stays sized for the old
	// viewport and the centring lands it off-screen.
	if m.datePicker != nil {
		m.datePicker.SetScreen(w, h)
	}
}

func (m *Model) Init() tea.Cmd {
	return m.Load()
}

// ClaimsKeys reports whether the screen owns every key right now (an open
// form or confirmation dialog). The shell consults it before its own routes
// so a typed character is never stolen ('q' would quit, space would open
// the tab menu, '/' the palette).
func (m *Model) ClaimsKeys() bool {
	return m.form != nil || m.Open != nil || m.datePicker != nil || m.Base.EditingDetail()
}

// ModalFormOpen reports a form drawn as its own centred WINDOW, which is the one
// state where Tab belongs to the form (field advance) rather than to the shell's
// tab ring. See router.go's tab chord.
// FormOpen reports whether a FORM is open. While one is up, Tab moves through the
// form's FIELDS rather than the tab ring.
// FORM-TAB: while a form is open Tab moves through its FIELDS, and an inline
// details-pane editor counts — the earlier rule only yielded to a centred window,
// which let Tab escape this host.
func (m *Model) FormOpen() bool {
	return m.form != nil || m.datePicker != nil || m.Base.EditingDetail()
}

// ActiveForm returns the open form (nil when closed) — tests and the shell
// read the in-progress input through it.
// ActiveForm returns the form the operator is currently editing, whichever host
// holds it — the legacy modal field or the inline details-pane editor. Tests drive
// writes through this, so they assert WHAT is being edited rather than WHERE it is
// drawn: the host is a presentation choice, not part of the contract.
func (m *Model) ActiveForm() *kit2.Form {
	if m.form != nil {
		return m.form
	}
	return m.Base.DetailForm()
}

// DialogOpen reports whether a confirmation dialog is up.
func (m *Model) DialogOpen() bool { return m.Open != nil }

// Notice returns the last action's status line ("" = none).
func (m *Model) Notice() string { return m.notice }

// ---------------- fetches ----------------

func (m *Model) fetchWorkflows(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{
		PageSize:  100,
		PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.Workflows))
	for _, w := range resp.Msg.Workflows {
		items = append(items, kit2.Item{
			ID:    w.GetId(),
			Title: w.GetName(),
			Meta:  strings.ToLower(w.GetStatus().String()),
		})
	}
	return items, resp.Msg.NextPageToken, nil
}

func (m *Model) fetchSchedules(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	resp, err := m.cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        100,
		PageToken:       pageToken,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_ONLY_RECURRING,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.WorkItems))
	for _, w := range resp.Msg.WorkItems {
		items = append(items, kit2.Item{ID: w.GetId(), Title: w.GetTitle(), Meta: scheduleMeta(w)})
	}
	return items, resp.Msg.NextPageToken, nil
}

// scheduleMeta is the list row's right-hand context: pause state, cadence
// and the next fire time.
func scheduleMeta(w *apiv1.WorkItem) string {
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
	return meta
}

func (m *Model) fetchIdeas(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	return m.fetchIdeaScope(ctx, pageToken, apiv1.IdeaStateScope_IDEA_STATE_SCOPE_ACTIVE)
}

func (m *Model) fetchRejected(ctx context.Context, pageToken string) ([]kit2.Item, string, error) {
	return m.fetchIdeaScope(ctx, pageToken, apiv1.IdeaStateScope_IDEA_STATE_SCOPE_REJECTED)
}

func (m *Model) fetchIdeaScope(ctx context.Context, pageToken string, scope apiv1.IdeaStateScope) ([]kit2.Item, string, error) {
	resp, err := m.cl.WorkItems.ListIdeas(ctx, connect.NewRequest(&apiv1.ListIdeasRequest{
		PageSize:       100,
		PageToken:      pageToken,
		IdeaStateScope: scope,
	}))
	if err != nil {
		return nil, "", err
	}
	items := make([]kit2.Item, 0, len(resp.Msg.GetIdeas()))
	for _, w := range resp.Msg.GetIdeas() {
		items = append(items, kit2.Item{ID: w.GetId(), Title: w.GetTitle(), Meta: ideaMeta(w, scope)})
	}
	return items, resp.Msg.GetNextPageToken(), nil
}

// ideaMeta renders an idea row's provenance: the spawned-by badge (title
// when the server resolved it, id otherwise) plus the spawn run.
func ideaMeta(w *apiv1.WorkItem, scope apiv1.IdeaStateScope) string {
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
	return meta
}

// ---------------- detail ----------------

func (m *Model) detail(ctx context.Context, src, id string) (string, []kit2.Field, string, error) {
	switch src {
	case srcWorkflows:
		resp, err := m.cl.Workflows.GetWorkflow(ctx, connect.NewRequest(&apiv1.GetWorkflowRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkflow()
		fields := []kit2.Field{
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
		fields := []kit2.Field{
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
		return "Recurring item: " + w.GetTitle(), fields, m.runHistory(ctx, id), nil

	case srcIdeas, srcRejected:
		resp, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return "", nil, "", err
		}
		w := resp.Msg.GetWorkItem()
		fields := []kit2.Field{
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
			body.WriteString("dismissed ideas are durable rejection history — the automation dedupe gate reads them before spawning again.\n")
		}
		return "Idea: " + w.GetTitle(), fields, strings.TrimRight(body.String(), "\n"), nil
	}
	return "", nil, "", nil
}

// runHistory renders the per-fire ledger: each fire's status + fire time,
// the bound workflow run (id + status, start → end), and that run's
// executions/outputs.
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

// ---------------- recurring item forms ----------------

// formReadyMsg carries either the freshly loaded option lists (create) or
// the item being edited back to the UI thread.
type formReadyMsg struct {
	mode      string
	item      *apiv1.WorkItem
	projects  []projectOpt
	workflows []workflowOpt
	err       error
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

func selOptions(vals ...string) []kit2.Option {
	out := make([]kit2.Option, 0, len(vals))
	for _, v := range vals {
		out = append(out, kit2.Option{Value: v, Label: v})
	}
	return out
}

// newCreateForm builds the typed create form. Submission goes through the
// mutation executor (never a bare RPC from the update loop).
func (m *Model) newCreateForm() *kit2.Form {
	projOpts := make([]kit2.Option, 0, len(m.projects))
	for _, p := range m.projects {
		projOpts = append(projOpts, kit2.Option{Value: p.Name, Label: p.Name})
	}
	wfOpts := append([]kit2.Option{{Value: "none", Label: "none"}}, make([]kit2.Option, 0, len(m.workflows))...)
	for _, w := range m.workflows {
		wfOpts = append(wfOpts, kit2.Option{Value: w.Name, Label: w.Name})
	}
	now := time.Now().UTC()
	f := kit2.NewForm("New recurring item",
		kit2.FieldSpec{Name: "title", Label: "Title", Kind: kit2.KText, Required: true, Placeholder: "nightly triage sweep"},
		kit2.FieldSpec{Name: "project", Label: "Project", Kind: kit2.KSelect, Options: projOpts, Required: true, Initial: projOpts[0].Value},
		kit2.FieldSpec{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Options: selOptions("task", "feature", "subtask"), Initial: "task"},
		kit2.FieldSpec{Name: "workflow", Label: "Workflow", Kind: kit2.KSelect, Options: wfOpts, Initial: "none"},
		kit2.FieldSpec{Name: "frequency", Label: "Frequency", Kind: kit2.KSelect, Options: selOptions("daily", "hourly", "weekly", "monthly", "minute"), Initial: "daily"},
		kit2.FieldSpec{Name: "interval", Label: "Interval", Kind: kit2.KNumber, Required: true, Initial: "1", Validate: validateInterval},
		// Weekdays are TOGGLED, not typed: "Mon,Wed,Fri" is a spelling test, and the
		// multi-select shows the whole week with the chosen days marked.
		kit2.FieldSpec{Name: "days", Label: "Days (space toggles)", Kind: kit2.KMultiSelect, Options: weekdayOptions()},
		// A DATE is chosen from the calendar, not typed: "YYYY-MM-DD" is a format to
		// remember and a text box cannot show that the 14th is a Saturday.
		kit2.FieldSpec{Name: "start_date", Label: "Start date", Kind: kit2.KDate, Required: true, Initial: now.Format("2006-01-02")},
		kit2.FieldSpec{Name: "start_time", Label: "Start time", Kind: kit2.KText, Required: true, Initial: "09:00", Validate: validateClock},
		kit2.FieldSpec{Name: "outputs", Label: "Outputs", Kind: kit2.KSelect, Options: selOptions("standard", "idea", "none"), Initial: "standard"},
		// The WINDOW confines fires to a daily interval [start, end). Both empty =
		// 24/7 (the legacy behaviour); both set = a half-open window, which the
		// server validates as end > start on the SAME day (wrapping midnight is
		// out of scope in v1) and, for daily/weekly/monthly, requires start_time
		// to lie INSIDE it.
		kit2.FieldSpec{Name: "window_start", Label: "Window start (HH:MM, empty = 24/7)", Kind: kit2.KText, Placeholder: "09:00", Validate: validateClock},
		kit2.FieldSpec{Name: "window_end", Label: "Window end (HH:MM, exclusive)", Kind: kit2.KText, Placeholder: "17:00", Validate: validateClock},
	)
	m.wireForm(f, formCreate, "")
	return f
}

// newEditForm builds the edit form for an existing recurring item (nil when
// the item carries no recurrence).
func (m *Model) newEditForm(w *apiv1.WorkItem) *kit2.Form {
	s := w.GetRecurringSchedule()
	if s == nil {
		return nil
	}
	enabled := "true"
	if !w.GetRecurringEnabled() {
		enabled = "false"
	}
	freqs := selOptions("daily", "hourly", "weekly", "monthly", "minute")
	f := kit2.NewForm("Edit recurring item",
		kit2.FieldSpec{Name: "title", Label: "Title", Kind: kit2.KText, Required: true, Initial: w.GetTitle()},
		kit2.FieldSpec{Name: "frequency", Label: "Frequency", Kind: kit2.KSelect, Options: freqs, Initial: inOptions(s.GetFrequency(), []string{"daily", "hourly", "weekly", "monthly", "minute"}, "daily")},
		kit2.FieldSpec{Name: "interval", Label: "Interval", Kind: kit2.KNumber, Required: true, Initial: strconv.Itoa(int(maxInt32(s.GetInterval(), 1))), Validate: validateInterval},
		kit2.FieldSpec{Name: "days", Label: "Days (space toggles)", Kind: kit2.KMultiSelect, Options: weekdayOptions(), Initial: strings.Join(s.GetDays(), ",")},
		kit2.FieldSpec{Name: "start_date", Label: "Start date", Kind: kit2.KDate, Required: true, Initial: s.GetStartDate()},
		kit2.FieldSpec{Name: "start_time", Label: "Start time", Kind: kit2.KText, Required: true, Initial: s.GetStartTime(), Validate: validateClock},
		kit2.FieldSpec{Name: "outputs", Label: "Outputs", Kind: kit2.KSelect, Options: selOptions("standard", "idea", "none"), Initial: inOptions(s.GetOutputsMode(), []string{"standard", "idea", "none"}, "standard")},
		kit2.FieldSpec{Name: "window_start", Label: "Window start (HH:MM, empty = 24/7)", Kind: kit2.KText, Initial: s.GetWindowStart(), Placeholder: "09:00", Validate: validateClock},
		kit2.FieldSpec{Name: "window_end", Label: "Window end (HH:MM, exclusive)", Kind: kit2.KText, Initial: s.GetWindowEnd(), Placeholder: "17:00", Validate: validateClock},
		kit2.FieldSpec{Name: "enabled", Label: "Enabled", Kind: kit2.KCheckbox, Initial: enabled},
	)
	m.wireForm(f, formEdit, w.GetId())
	return f
}

// wireForm installs the submit handler: it builds the mutation request from
// the collected values and hands it to the executor.
func (m *Model) wireForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 66
	// A KDate field opens the host's calendar instead of accepting text.
	f.OnOpenDatePicker = m.openDatePicker
	f.OnSubmit = func(v map[string]string, multi map[string][]string) (tea.Cmd, error) {
		// One gate for BOTH modes: the window rules are the server's
		// (internal/workitem/validate.go), and catching them here reports them at
		// the form rather than coming back as a failed mutation.
		if err := validateScheduleWindow(v); err != nil {
			return nil, err
		}
		switch mode {
		case formCreate:
			projID := ""
			for _, p := range m.projects {
				if p.Name == v["project"] {
					projID = p.ID
				}
			}
			if projID == "" {
				return nil, fmt.Errorf("unknown project %q", v["project"])
			}
			wfID := ""
			if name := v["workflow"]; name != "" && name != "none" {
				for _, w := range m.workflows {
					if w.Name == name {
						wfID = w.ID
					}
				}
			}
			req := &apiv1.CreateWorkItemRequest{
				ProjectId:         projID,
				Kind:              kindFromForm(v["kind"]),
				Title:             strings.TrimSpace(v["title"]),
				WorkflowId:        wfID,
				RecurringSchedule: scheduleFromValues(v, multi),
			}
			name := "create recurring item " + strconv.Quote(req.GetTitle())
			return m.Mutate(mutate.Request{
				Name: name, Source: srcSchedules,
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.CreateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil
		case formEdit:
			sched := scheduleFromValues(v, multi)
			enabled := v["enabled"] == "true"
			req := &apiv1.UpdateWorkItemRequest{
				Id:                id,
				Title:             strPtr(strings.TrimSpace(v["title"])),
				RecurringSchedule: sched,
				RecurringEnabled:  &enabled,
			}
			name := "save recurring item " + strconv.Quote(req.GetTitle())
			return m.Mutate(mutate.Request{
				Name: name, Source: srcSchedules,
				Rollback: func() { m.Refresh(srcSchedules) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil
		}
		return nil, nil
	}
}

// ---------------- entity actions (pause/resume, delete, promote, dismiss) ----------------

// actionsForSelection builds the entity-bound actions for the selected row.
// Destructive actions carry a confirmation; every RPC runs through the
// mutation executor with an optimistic apply + rollback.
func (m *Model) actionsForSelection() []kit2.Action {
	item, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	switch m.ActiveSourceName() {
	case srcSchedules:
		id, title := item.ID, item.Title
		return []kit2.Action{
			{
				Label: "pause/resume", Key: "p", Source: srcSchedules,
				Do: func(ctx context.Context) error { return m.rpcTogglePause(ctx, id) },
			},
			{
				Label: "delete", Key: "x", Danger: true, Source: srcSchedules,
				Confirm:  "Delete " + title + "?\nThe recurring item is cancelled (soft delete) and stops firing. Its fire history is kept.",
				Apply:    func() { m.RemoveRow(srcSchedules, id) },
				Rollback: func() { m.Refresh(srcSchedules) },
				Do:       func(ctx context.Context) error { return m.rpcDelete(ctx, id) },
			},
		}
	case srcIdeas:
		id, title := item.ID, item.Title
		return []kit2.Action{
			{
				Label: "promote", Key: "p", Source: srcIdeas,
				Apply:    func() { m.RemoveRow(srcIdeas, id) },
				Rollback: func() { m.Refresh(srcIdeas) },
				Do:       func(ctx context.Context) error { return m.rpcPromote(ctx, id) },
			},
			{
				Label: "dismiss", Key: "x", Danger: true, Source: srcIdeas,
				Confirm:  "Dismiss " + title + "?\nIt leaves the Idea Cloud and is kept as rejected history (the dedupe gate will not re-propose it).",
				Apply:    func() { m.RemoveRow(srcIdeas, id) },
				Rollback: func() { m.Refresh(srcIdeas) },
				Do:       func(ctx context.Context) error { return m.rpcDismiss(ctx, id) },
			},
		}
	}
	return nil
}

// openAction opens the confirmation dialog for an action that needs one, or
// runs it immediately.
func (m *Model) openAction(a kit2.Action) tea.Cmd {
	if !a.NeedsConfirm() {
		return m.runAction(a)
	}
	d := kit2.Confirm(a.Label, a.Confirm, a.Label)
	d.Danger = a.Danger
	m.Open = d
	pending := a
	m.pending = &pending
	m.OnDialog = func(choice string) tea.Cmd {
		pa := m.pending
		m.pending = nil
		m.OnDialog = nil
		if pa == nil || choice == "" {
			m.notice = "cancelled"
			return nil // dismissed
		}
		m.notice = choice + " confirmed"
		return m.runAction(*pa)
	}
	return nil
}

func (m *Model) runAction(a kit2.Action) tea.Cmd {
	return m.Mutate(mutate.Request{
		Name: a.Label, Source: a.Source,
		Apply: a.Apply, Rollback: a.Rollback, Do: a.Do,
	})
}

// actionByKey returns the action bound to a key for the focused source.
func (m *Model) actionByKey(key string) (kit2.Action, bool) {
	for _, a := range m.actionsForSelection() {
		if a.Key == key {
			return a, true
		}
	}
	return kit2.Action{}, false
}

func (m *Model) rpcTogglePause(ctx context.Context, id string) error {
	cur, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
	if err != nil {
		return err
	}
	if cur.Msg.GetWorkItem().GetRecurringSchedule() == nil {
		return fmt.Errorf("not a recurring item")
	}
	next := !cur.Msg.GetWorkItem().GetRecurringEnabled()
	_, err = m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:               id,
		RecurringEnabled: &next,
	}))
	return err
}

func (m *Model) rpcDelete(ctx context.Context, id string) error {
	_, err := m.cl.WorkItems.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: id}))
	return err
}

func (m *Model) defaultPromote(ctx context.Context, id string) error {
	_, err := m.cl.WorkItems.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: id}))
	return err
}

func (m *Model) defaultDismiss(ctx context.Context, id string) error {
	_, err := m.cl.WorkItems.DismissIdea(ctx, connect.NewRequest(&apiv1.DismissIdeaRequest{Id: id}))
	return err
}

// ---------------- update ----------------

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

	case ideaBulkDoneMsg:
		return m, m.onIdeaBulkDone(msg)

	case formReadyMsg:
		if msg.err != nil {
			m.notice = "couldn't open the form: " + msg.err.Error()
			return m, nil
		}
		// The forms open IN THE DETAILS PANE, not a modal — the same host the
		// work-item, worker and Control forms use, so the keys, validation and submit
		// path cannot diverge between screens.
		switch msg.mode {
		case formCreate:
			if len(msg.projects) == 0 {
				m.notice = "no projects yet — a recurring item belongs to a project"
				return m, nil
			}
			m.projects, m.workflows = msg.projects, msg.workflows
			if f := m.newCreateForm(); f != nil {
				m.Base.BeginDetailEdit(f.Title, f)
			}
			m.formMode, m.formID = formCreate, ""
		case formEdit:
			f := m.newEditForm(msg.item)
			if f == nil {
				m.notice = "this item is not recurring — there is no recurrence to edit"
				return m, nil
			}
			m.Base.BeginDetailEdit(f.Title, f)
			m.formMode, m.formID = formEdit, msg.item.GetId()
		}
		m.notice = ""
		return m, nil

	case tea.KeyMsg:
		// The CALENDAR is the topmost modal: it owns every key while it is up, so a
		// keystroke aimed at it can never act on the form underneath.
		if m.datePicker != nil {
			_, cmd := m.datePicker.HandleKey(msg)
			return m, tea.Batch(cmd, m.finishDatePicker())
		}
		// The INLINE details-pane editor owns every key while it is up — it is the
		// focused surface, so this comes FIRST.
		//
		// It has to: handleKey answers 'e' (edit), 'n' (new) and 'p'/'x'
		// (promote/dismiss), so with a form open in the pane a plain letter fired a
		// CHORD instead of being typed. That is why typing "Nightly triage sweep"
		// into the title lost every 'e' — each one re-prepared the edit form and
		// discarded the text so far.
		if m.Base.EditingDetail() {
			if handled, cmd := m.Base.Update(msg); handled {
				return m, cmd
			}
		}
		// The legacy MODAL host (kept for any screen still using it).
		if m.form != nil {
			if msg.String() == "esc" {
				m.form = nil
				m.notice = "cancelled"
				return m, nil
			}
			cmd, _ := m.form.HandleKey(msg)
			if m.form.Submitted {
				m.form = nil
				m.notice = "saved " + strconv.Quote(m.formID)
			}
			return m, cmd
		}
		if m.Open != nil {
			break // the dialog owns every key — kit2.Base resolves it
		}
		if cmd, handled := m.handleKey(msg); handled {
			return m, cmd
		}
	}

	if handled, cmd := m.Base.Update(msg); handled {
		return m, cmd
	}
	return m, nil
}

// handleKey implements the screen's own keys. handled=false falls through to
// the shared list/detail navigation.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Cmd, bool) {
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
	case "A":
		// BULK triage on the Idea Cloud: 'A' accepts every listed idea, 'R'
		// rejects them all. Triage is the reason the cloud exists, and a run that
		// spawned eight ideas should not ask for eight keystrokes.
		if src == srcIdeas {
			return m.acceptAllIdeas(), true
		}
	case "R":
		if src == srcIdeas {
			return m.rejectAllIdeas(), true
		}
	case "p", "x":
		if a, ok := m.actionByKey(msg.String()); ok {
			return m.openAction(a), true
		}
	}
	return nil, false
}

// ---------------- view ----------------

func (m *Model) View() string {
	m.refreshActionBar()

	body := m.Base.View()
	if m.notice != "" {
		body += "\n" + theme.HintText.Render(m.notice)
	}
	if m.w > 0 && m.h > 0 {
		body = kit2.FitLines(body, m.w, m.h)
		// The CALENDAR is the topmost modal — it is layered above the form it was
		// opened from, so it is checked first.
		if m.datePicker != nil {
			body = kit2.Center(body, m.datePicker.View(), m.w, m.h)
		} else if m.form != nil {
			body = kit2.Center(body, formBox(m.form, m.w), m.w, m.h)
		} else if m.Open != nil {
			box := m.Open.Box(minInt(64, m.w-4), minInt(12, m.h-2))
			body = kit2.Center(body, box, m.w, m.h)
		}
		return kit2.FitLines(body, m.w, m.h)
	}
	if m.form != nil {
		return body + "\n" + formBox(m.form, 66)
	}
	return body
}

// formBox wraps the typed form in a titled dialog-sized box.
func formBox(f *kit2.Form, w int) string {
	bw := minInt(70, w-4)
	if bw < 24 {
		bw = 24
	}
	d := &kit2.Dialog{Title: f.Title, Body: f.View(), Buttons: []string{"submit", "cancel"}}
	return d.Box(bw, minInt(22, len(f.Specs)*2+5))
}

func (m *Model) refreshActionBar() {
	actions := m.actionsForSelection()
	m.bar.Actions = actions
	if m.bar.Sel >= len(actions) {
		m.bar.Sel = 0
	}
	m.Base.Bar = m.bar
}

// HintLine is the screen's key cheat-sheet.
func (m *Model) HintLine() string {
	switch m.ActiveSourceName() {
	case srcSchedules:
		return theme.HintText.Render("n: new recurring item · e: edit · p: pause/resume · x: delete (confirm) · enter: detail (run history) · f: more pages")
	case srcIdeas:
		return theme.HintText.Render("p: promote (→ work item) " + theme.DetailKey.Render("·") + " x: dismiss (confirm) " + theme.DetailKey.Render("·") +
			" A: accept ALL " + theme.DetailKey.Render("·") + " R: reject ALL (confirm) " + theme.DetailKey.Render("·") + " ←/→: pane · enter: detail · r: refresh")
	case srcRejected:
		return theme.HintText.Render("rejected history — the automation dedupe gate reads it before re-spawning · enter: detail")
	default:
		return theme.HintText.Render("enter: detail focus · ←/→: pane · f: more pages · r: refresh")
	}
}

// --- bulk idea triage -------------------------------------------------------
//
// Triage is the reason the Idea Cloud exists: an automation proposes, a human
// decides. One at a time is the wrong grain for that — a run that spawns eight
// ideas asks for eight keystrokes and eight confirmations.
//
// So the decision applies to the SAME set the list is showing. It is the scope the
// operator can SEE, which is the only scope they can reason about.

// acceptAllIdeas promotes every idea currently listed.
func (m *Model) acceptAllIdeas() tea.Cmd {
	items := m.visibleItems(srcIdeas)
	if len(items) == 0 {
		return m.refuseAutomation("no ideas awaiting triage")
	}
	return m.bulkIdeaDecision(items, true)
}

// rejectAllIdeas dismisses every idea currently listed. Confirmed, because a
// dismissal is durable rejection history — the dedupe gate will not re-propose
// them — so it is not the kind of thing to do by accident.
func (m *Model) rejectAllIdeas() tea.Cmd {
	items := m.visibleItems(srcIdeas)
	if len(items) == 0 {
		return m.refuseAutomation("no ideas awaiting triage")
	}
	d := kit2.Confirm("Reject all ideas",
		fmt.Sprintf("Reject %d idea(s)?\n\nEach is kept as REJECTED history and the "+
			"automation's dedupe gate will not propose them again.", len(items)),
		"reject all")
	d.Danger = true
	m.Open = d
	m.OnDialog = func(choice string) tea.Cmd {
		m.OnDialog = nil
		if choice == "" {
			m.notice = "cancelled"
			return nil
		}
		return m.bulkIdeaDecision(items, false)
	}
	return nil
}

// bulkIdeaDecision runs accept-or-reject over a set, ONE mutation per idea.
//
// Not a single batched request: PromoteIdea/DismissIdea are per-item RPCs, and a
// batch that reports only overall success would hide a partial failure — which is
// exactly the case that matters here, since the operator has to know WHICH ideas
// are still awaiting a decision.
func (m *Model) bulkIdeaDecision(items []screenkit.Item, accept bool) tea.Cmd {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	// Optimistic: the rows leave the list now, and a failure reloads the source.
	for _, id := range ids {
		m.RemoveRow(srcIdeas, id)
	}
	verb := "reject"
	if accept {
		verb = "accept"
	}
	m.notice = fmt.Sprintf("%s %d idea(s)…", verb, len(ids))
	promote, dismiss := m.rpcPromote, m.rpcDismiss
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		failed := make([]string, 0)
		for _, id := range ids {
			var err error
			if accept {
				err = promote(ctx, id)
			} else {
				err = dismiss(ctx, id)
			}
			if err != nil {
				failed = append(failed, id)
			}
		}
		return ideaBulkDoneMsg{accept: accept, total: len(ids), failed: failed}
	}
}

// ideaBulkDoneMsg reports a finished bulk triage.
type ideaBulkDoneMsg struct {
	accept bool
	total  int
	failed []string
}

// onIdeaBulkDone surfaces the outcome and reloads the sources: the optimistic
// removal is only safe if a failure puts the rows back, and the durable truth is
// the server's list.
func (m *Model) onIdeaBulkDone(msg ideaBulkDoneMsg) tea.Cmd {
	verb := "rejected"
	if msg.accept {
		verb = "accepted"
	}
	switch len(msg.failed) {
	case 0:
		m.notice = fmt.Sprintf("%s %d idea(s)", verb, msg.total)
		// Accepting CREATES work items, so the rejection history and every
		// work-item view can change too.
		return tea.Batch(m.Refresh(srcIdeas), m.Refresh(srcRejected), m.Refresh(srcSchedules))
	case msg.total:
		m.notice = fmt.Sprintf("%s failed — nothing changed", verb)
	default:
		m.notice = fmt.Sprintf("%s %d of %d — %d failed (still listed)",
			verb, msg.total-len(msg.failed), msg.total, len(msg.failed))
	}
	return tea.Batch(m.Refresh(srcIdeas), m.Refresh(srcRejected))
}

// refuseAutomation records a local refusal in the status line.
func (m *Model) refuseAutomation(why string) tea.Cmd {
	m.notice = why
	return nil
}

// visibleItems returns a source's currently loaded rows.
func (m *Model) visibleItems(src string) []screenkit.Item {
	rows := m.Base.SourceItems(src)
	out := make([]screenkit.Item, 0, len(rows))
	out = append(out, rows...)
	return out
}

// ---------------- helpers ----------------

func scheduleFromValues(v map[string]string, multi map[string][]string) *apiv1.RecurringSchedule {
	interval, _ := strconv.Atoi(strings.TrimSpace(v["interval"]))
	if interval < 1 {
		interval = 1
	}
	// Days come from the MULTI-SELECT, in the field's option order (Mon..Sun) —
	// deterministic, unlike the map iteration it replaced. The legacy typed form is
	// still parsed so a value already stored that way keeps working.
	days := multi["days"]
	if len(days) == 0 {
		days = splitDays(v["days"])
	}
	return &apiv1.RecurringSchedule{
		Frequency:   strings.TrimSpace(v["frequency"]),
		Interval:    int32(interval),
		Days:        days,
		StartDate:   strings.TrimSpace(v["start_date"]),
		StartTime:   strings.TrimSpace(v["start_time"]),
		OutputsMode: strings.TrimSpace(v["outputs"]),
		WindowStart: strings.TrimSpace(v["window_start"]),
		WindowEnd:   strings.TrimSpace(v["window_end"]),
	}
}

// weekdayOptions is the Mon..Sun option set, in calendar order. The VALUES are the
// wire spelling the schedule stores (and the server validates), so they are also
// what the toggle writes — no translation layer to drift.
func weekdayOptions() []kit2.Option {
	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	out := make([]kit2.Option, 0, len(names))
	for _, n := range names {
		out = append(out, kit2.Option{Value: n, Label: n})
	}
	return out
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

// splitDays normalizes a "mon,wed" list to ["Mon","Wed"] (canonical weekday
// spellings — the proto's days[] vocabulary).
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
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("use YYYY-MM-DD")
	}
	return nil
}

func validateClock(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if _, err := time.Parse("15:04", strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("use HH:MM (24h)")
	}
	return nil
}

// validateScheduleWindow mirrors the SERVER's rules (internal/workitem/validate.go:
// 555-587) so a window the plane would reject is caught at the field instead of
// coming back as a failed mutation. The rules are: both-or-neither, both HH:MM, end
// strictly after start on the SAME day (wrapping midnight is out of scope in v1),
// and for daily/weekly/monthly the anchor time must lie INSIDE the window.
func validateScheduleWindow(v map[string]string) error {
	ws := strings.TrimSpace(v["window_start"])
	we := strings.TrimSpace(v["window_end"])
	if (ws == "") != (we == "") {
		return errors.New("window start and end must be set together (leave BOTH empty for 24/7)")
	}
	if ws == "" {
		return nil
	}
	s, err := time.Parse("15:04", ws)
	if err != nil {
		return errors.New("window start must be HH:MM")
	}
	e, err := time.Parse("15:04", we)
	if err != nil {
		return errors.New("window end must be HH:MM")
	}
	sm := s.Hour()*60 + s.Minute()
	em := e.Hour()*60 + e.Minute()
	if em <= sm {
		return errors.New("window end must be after window start (wrapping midnight is not supported)")
	}
	switch strings.ToLower(strings.TrimSpace(v["frequency"])) {
	case "daily", "weekly", "monthly":
		st, err := time.Parse("15:04", strings.TrimSpace(v["start_time"]))
		if err != nil {
			return nil // start_time has its own validator
		}
		m := st.Hour()*60 + st.Minute()
		if m < sm || m >= em {
			return errors.New("start time must lie INSIDE the window for a daily/weekly/monthly schedule")
		}
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

func strPtr(s string) *string { return &s }

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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SelectSource focuses the named source (slash nav command support).
func (m *Model) SelectSource(name string) bool { return m.Base.SelectSource(name) }

// SelectItem selects the item by ID in the named source (slash arg jumps).
func (m *Model) SelectItem(src, id string) bool { return m.Base.SelectItem(src, id) }

// RequestDetail loads the detail view for (src, id) directly.
func (m *Model) RequestDetail(src, id string) tea.Cmd { return m.Base.RequestDetail(src, id) }

// ActiveSourceName / ActiveItem expose the Base focus state to the shell's
// context engine.
func (m *Model) ActiveSourceName() string           { return m.Base.ActiveSourceName() }
func (m *Model) ActiveItem() (screenkit.Item, bool) { return m.Base.ActiveItem() }

// --- the calendar modal ---------------------------------------------------------

// openDatePicker opens the host's calendar for a KDate field, seeded from the
// field's current value so editing a date starts where the operator left it.
func (m *Model) openDatePicker(field, current string) tea.Cmd {
	var initial time.Time
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(current)); err == nil {
		initial = t
	}
	m.dateField = field
	dp := kit2.NewDatePicker("Select date", initial)
	dp.SetScreen(m.w, m.h)
	m.datePicker = dp
	return nil
}

// finishDatePicker closes the calendar when it reports Done and writes the chosen
// date into the field that opened it. The SCREEN owns the close (see
// kit2.ModelPicker.Done for why a host callback cannot).
func (m *Model) finishDatePicker() tea.Cmd {
	if m.datePicker == nil || !m.datePicker.Done() {
		return nil
	}
	value, committed, field := m.datePicker.Value(), m.datePicker.Committed(), m.dateField
	m.datePicker, m.dateField = nil, ""
	if !committed {
		return nil
	}
	if f := m.ActiveForm(); f != nil && field != "" {
		f.Set(field, value)
	}
	return nil
}
