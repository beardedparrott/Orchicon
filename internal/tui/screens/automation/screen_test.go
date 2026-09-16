package automation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/subs"
)

// ---------- fake plane ----------
//
// The automation screen is asserted against a REAL Connect client talking
// to a fake WorkItemService/ProjectService/WorkflowService over httptest:
// the requests the screen actually sends are recorded, and the fake holds
// the work-item model so "promoted → normal scope", "dismissed → leaves
// active views" are asserted against the model, not against the UI.

type fakePlane struct {
	apiv1connect.UnimplementedWorkItemServiceHandler

	mu       sync.Mutex
	items    map[string]*apiv1.WorkItem
	order    []string
	history  map[string][]*apiv1.RecurringRunHistoryEntry
	nextID   int
	created  []*apiv1.CreateWorkItemRequest
	updated  []*apiv1.UpdateWorkItemRequest
	deleted  []string
	promoted []string
	dismissd []string
}

func newPlane() *fakePlane {
	return &fakePlane{items: map[string]*apiv1.WorkItem{}, history: map[string][]*apiv1.RecurringRunHistoryEntry{}}
}

func (p *fakePlane) add(w *apiv1.WorkItem) *apiv1.WorkItem {
	p.items[w.GetId()] = w
	p.order = append(p.order, w.GetId())
	return w
}

func (p *fakePlane) seedRecurring(id, title string, enabled bool) *apiv1.WorkItem {
	return p.add(&apiv1.WorkItem{
		Id:                id,
		Title:             title,
		Status:            apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING,
		ProjectId:         "proj-1",
		RecurringEnabled:  enabled,
		RecurringSchedule: &apiv1.RecurringSchedule{Frequency: "daily", Interval: 1, StartDate: "2026-08-01", StartTime: "09:00", OutputsMode: "standard"},
		NextRunAt:         timestamppb.Now(),
		CreatedAt:         timestamppb.Now(),
	})
}

// seedIdea adds an automation-spawned work item in the given status.
func (p *fakePlane) seedIdea(id, title string, status apiv1.WorkItemStatus, spawner, runID string) *apiv1.WorkItem {
	return p.add(&apiv1.WorkItem{
		Id:             id,
		Title:          title,
		Status:         status,
		Kind:           apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId:      "proj-1",
		SpawnedBy:      spawner,
		SpawnedByRunId: runID,
		SpawnedByTitle: p.titleOf(spawner),
		CreatedAt:      timestamppb.Now(),
	})
}

func (p *fakePlane) titleOf(id string) string {
	if w, ok := p.items[id]; ok {
		return w.GetTitle()
	}
	return ""
}

// ideaItem is the read-time copy the idea surfaces return: provenance +
// the spawned_by_title badge the server resolves for display.
func (p *fakePlane) ideaItem(w *apiv1.WorkItem) *apiv1.WorkItem {
	cp := &apiv1.WorkItem{}
	cp.Id = w.GetId()
	cp.Title = w.GetTitle()
	cp.Status = w.GetStatus()
	cp.Kind = w.GetKind()
	cp.Priority = w.GetPriority()
	cp.ProjectId = w.GetProjectId()
	cp.Description = w.GetDescription()
	cp.SpawnedBy = w.GetSpawnedBy()
	cp.SpawnedByRunId = w.GetSpawnedByRunId()
	cp.SpawnedByTitle = p.titleOf(w.GetSpawnedBy())
	cp.CreatedAt = w.GetCreatedAt()
	return cp
}

func (p *fakePlane) CreateWorkItem(_ context.Context, req *connect.Request[apiv1.CreateWorkItemRequest]) (*connect.Response[apiv1.CreateWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, req.Msg)
	p.nextID++
	id := fmt.Sprintf("rec-new-%d", p.nextID)
	status := apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING
	if req.Msg.GetRecurringSchedule() != nil {
		status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING
	}
	w := p.add(&apiv1.WorkItem{
		Id:                id,
		Title:             req.Msg.GetTitle(),
		Kind:              req.Msg.GetKind(),
		Status:            status,
		ProjectId:         req.Msg.GetProjectId(),
		WorkflowId:        req.Msg.GetWorkflowId(),
		RecurringSchedule: req.Msg.GetRecurringSchedule(),
		RecurringEnabled:  true,
		CreatedAt:         timestamppb.Now(),
	})
	return connect.NewResponse(&apiv1.CreateWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) GetWorkItem(_ context.Context, req *connect.Request[apiv1.GetWorkItemRequest]) (*connect.Response[apiv1.GetWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	// The live item (recurrence included — the edit form reads it) plus the
	// read-time spawned_by_title badge every idea-surface read carries.
	w.SpawnedByTitle = p.titleOf(w.GetSpawnedBy())
	return connect.NewResponse(&apiv1.GetWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) UpdateWorkItem(_ context.Context, req *connect.Request[apiv1.UpdateWorkItemRequest]) (*connect.Response[apiv1.UpdateWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.updated = append(p.updated, req.Msg)
	if req.Msg.Title != nil {
		w.Title = req.Msg.GetTitle()
	}
	if req.Msg.RecurringSchedule != nil {
		w.RecurringSchedule = req.Msg.GetRecurringSchedule()
		w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING
	}
	if req.Msg.RecurringEnabled != nil {
		w.RecurringEnabled = req.Msg.GetRecurringEnabled()
	}
	if req.Msg.Status != nil {
		w.Status = req.Msg.GetStatus()
	}
	return connect.NewResponse(&apiv1.UpdateWorkItemResponse{WorkItem: w}), nil
}

// DeleteWorkItem soft-deletes (status → cancelled), exactly like the plane.
func (p *fakePlane) DeleteWorkItem(_ context.Context, req *connect.Request[apiv1.DeleteWorkItemRequest]) (*connect.Response[apiv1.DeleteWorkItemResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("work item not found"))
	}
	p.deleted = append(p.deleted, req.Msg.GetId())
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	return connect.NewResponse(&apiv1.DeleteWorkItemResponse{WorkItem: w}), nil
}

func (p *fakePlane) ListWorkItems(_ context.Context, req *connect.Request[apiv1.ListWorkItemsRequest]) (*connect.Response[apiv1.ListWorkItemsResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*apiv1.WorkItem
	for _, id := range p.order {
		w := p.items[id]
		// Normal lists exclude idea-state items and terminal cancellations
		// (dismissed/soft-deleted), and honour the recurring filter.
		if w.GetStatus() == apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA {
			if req.Msg.GetIdeaScope() != apiv1.IdeaScope_IDEA_SCOPE_ONLY_IDEA {
				continue
			}
		}
		if w.GetStatus() == apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
			continue
		}
		isRec := w.GetRecurringSchedule() != nil
		switch req.Msg.GetRecurringFilter() {
		case apiv1.RecurringFilter_RECURRING_FILTER_ONLY_RECURRING:
			if !isRec {
				continue
			}
		case apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING:
			if isRec {
				continue
			}
		}
		out = append(out, w)
	}
	return connect.NewResponse(&apiv1.ListWorkItemsResponse{WorkItems: out}), nil
}

func (p *fakePlane) ListIdeas(_ context.Context, req *connect.Request[apiv1.ListIdeasRequest]) (*connect.Response[apiv1.ListIdeasResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*apiv1.WorkItem
	for _, id := range p.order {
		w := p.items[id]
		rejected := w.GetStatus() == apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED && w.GetSpawnedBy() != ""
		isIdea := w.GetStatus() == apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA
		switch req.Msg.GetIdeaStateScope() {
		case apiv1.IdeaStateScope_IDEA_STATE_SCOPE_REJECTED:
			if !rejected {
				continue
			}
		default:
			if !isIdea {
				continue
			}
		}
		out = append(out, p.ideaItem(w))
	}
	return connect.NewResponse(&apiv1.ListIdeasResponse{Ideas: out}), nil
}

// PromoteIdea is the only sanctioned path out of idea state: → pending,
// which puts the item back in the normal Work Items scope.
func (p *fakePlane) PromoteIdea(_ context.Context, req *connect.Request[apiv1.PromoteIdeaRequest]) (*connect.Response[apiv1.PromoteIdeaResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("idea not found"))
	}
	p.promoted = append(p.promoted, req.Msg.GetId())
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING
	return connect.NewResponse(&apiv1.PromoteIdeaResponse{WorkItem: w}), nil
}

// DismissIdea maps to cancelled (the soft-delete terminal).
func (p *fakePlane) DismissIdea(_ context.Context, req *connect.Request[apiv1.DismissIdeaRequest]) (*connect.Response[apiv1.DismissIdeaResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.items[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("idea not found"))
	}
	p.dismissd = append(p.dismissd, req.Msg.GetId())
	w.Status = apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED
	return connect.NewResponse(&apiv1.DismissIdeaResponse{WorkItem: w}), nil
}

func (p *fakePlane) GetWorkItemRunHistory(_ context.Context, req *connect.Request[apiv1.GetWorkItemRunHistoryRequest]) (*connect.Response[apiv1.GetWorkItemRunHistoryResponse], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return connect.NewResponse(&apiv1.GetWorkItemRunHistoryResponse{Entries: p.history[req.Msg.GetId()]}), nil
}

func (p *fakePlane) lastCreated(t *testing.T) *apiv1.CreateWorkItemRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.created) == 0 {
		t.Fatal("no CreateWorkItem call was made")
	}
	return p.created[len(p.created)-1]
}

func (p *fakePlane) lastUpdated(t *testing.T) *apiv1.UpdateWorkItemRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.updated) == 0 {
		t.Fatal("no UpdateWorkItem call was made")
	}
	return p.updated[len(p.updated)-1]
}

func (p *fakePlane) itemStatus(id string) apiv1.WorkItemStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.items[id].GetStatus()
}

type fakeProjects struct {
	apiv1connect.UnimplementedProjectServiceHandler
}

func (fakeProjects) ListProjects(context.Context, *connect.Request[apiv1.ListProjectsRequest]) (*connect.Response[apiv1.ListProjectsResponse], error) {
	return connect.NewResponse(&apiv1.ListProjectsResponse{Projects: []*apiv1.Project{
		{Id: "proj-1", Name: "Orchicon"},
	}}), nil
}

type fakeWorkflows struct {
	apiv1connect.UnimplementedWorkflowServiceHandler
	workflows []*apiv1.Workflow
}

func (f fakeWorkflows) ListWorkflows(context.Context, *connect.Request[apiv1.ListWorkflowsRequest]) (*connect.Response[apiv1.ListWorkflowsResponse], error) {
	return connect.NewResponse(&apiv1.ListWorkflowsResponse{Workflows: f.workflows}), nil
}

// ---------- harness ----------

func newModel(t *testing.T, p *fakePlane) *Model {
	t.Helper()
	return newModelWith(t, p, []*apiv1.Workflow{{Id: "wf-1", Name: "Fanout sweep"}})
}

func newModelWith(t *testing.T, p *fakePlane, wfs []*apiv1.Workflow) *Model {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewWorkItemServiceHandler(p))
	mux.Handle(apiv1connect.NewProjectServiceHandler(fakeProjects{}))
	mux.Handle(apiv1connect.NewWorkflowServiceHandler(fakeWorkflows{workflows: wfs}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m := New(client.New(client.Options{BaseURL: srv.URL}), subs.NewRegistry(), "")
	m.SetSize(240, 40)
	return m
}

func kmsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "space":
		// bubbletea parses a space as KeySpace WITH the rune attached.
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press drives one key through the screen and returns the cmd it produced.
func press(t *testing.T, m *Model, key string) tea.Cmd {
	t.Helper()
	_, cmd := m.Update(kmsg(key))
	return cmd
}

// submit focuses the form's last field and presses enter — kit2's typed-form
// submit gesture (validation runs first; an invalid form stays open).
func submit(t *testing.T, m *Model, lastField string) tea.Cmd {
	t.Helper()
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("no open form to submit")
	}
	if !f.FocusName(lastField) {
		t.Fatalf("form has no field %q", lastField)
	}
	return press(t, m, "ctrl+s")
}

// run executes a cmd and feeds its message back into the screen (the
// UI-thread round trip).
func run(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	msg := cmd()
	if msg == nil {
		t.Fatal("command produced no message")
	}
	m.Update(msg)
}

// load drives one source's fetch through the real fetch func and feeds the
// result into the screen (the shell's load path).
func load(t *testing.T, m *Model, src string) {
	t.Helper()
	for _, s := range m.Base.SourcesForTest() {
		if s.Name != src {
			continue
		}
		items, next, err := s.Fetch(context.Background(), "")
		if err != nil {
			t.Fatalf("fetch %s: %v", src, err)
		}
		if !m.Base.LoadItems(src, items, next) {
			t.Fatalf("no source %q to load", src)
		}
		return
	}
	t.Fatalf("no source %q registered", src)
}

func itemsOf(m *Model, src string) []screenkit.Item {
	for _, s := range m.Base.SourcesForTest() {
		if s.Name == src {
			return s.Items
		}
	}
	return nil
}

func titles(items []screenkit.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}

func hasTitle(items []screenkit.Item, title string) bool {
	for _, it := range items {
		if it.Title == title {
			return true
		}
	}
	return false
}

// normalScope is the "normal Work Items scope": non-recurring, idea-excluded
// — i.e. what a promoted idea becomes queryable in.
func normalScope(t *testing.T, m *Model) []*apiv1.WorkItem {
	t.Helper()
	resp, err := m.cl.WorkItems.ListWorkItems(context.Background(), connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        100,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
	}))
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	return resp.Msg.GetWorkItems()
}

// ---------- recurring items ----------

func TestRecurringItemsCreateFromForm(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	m := newModel(t, p)
	m.SelectSource("recurring-items")

	// 'n' prepares the form: projects + workflows load first.
	if cmd := press(t, m, "n"); cmd == nil {
		t.Fatal("n on Recurring Items must load the create form's options")
	} else {
		run(t, m, cmd)
	}
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open form must claim keys (typed characters are never shell shortcuts)")
	}

	// Typing (including spaces) goes to the focused field, not the shell.
	f.FocusName("title")
	for _, ch := range "Nightly triage sweep" {
		if ch == ' ' {
			press(t, m, "space")
			continue
		}
		press(t, m, string(ch))
	}
	if got := f.Values["title"]; got != "Nightly triage sweep" {
		t.Fatalf("typed title = %q", got)
	}

	f.Set("project", "Orchicon")
	f.Set("workflow", "Fanout sweep")
	f.Set("frequency", "weekly")
	f.Set("interval", "2")
	f.Set("days", "Mon,Wed")
	f.Set("start_date", "2026-09-01")
	f.Set("start_time", "07:30")
	f.Set("outputs", "idea")

	run(t, m, submit(t, m, "outputs"))

	if m.ActiveForm() != nil {
		t.Fatal("the form must close after a successful create")
	}
	req := p.lastCreated(t)
	if req.GetTitle() != "Nightly triage sweep" || req.GetProjectId() != "proj-1" || req.GetWorkflowId() != "wf-1" {
		t.Fatalf("create request = %+v", req)
	}
	if req.GetKind() != apiv1.WorkItemKind_WORK_ITEM_KIND_TASK {
		t.Fatalf("create kind = %v", req.GetKind())
	}
	s := req.GetRecurringSchedule()
	if s == nil {
		t.Fatal("create must carry a recurring_schedule")
	}
	if s.GetFrequency() != "weekly" || s.GetInterval() != 2 || strings.Join(s.GetDays(), ",") != "Mon,Wed" ||
		s.GetStartDate() != "2026-09-01" || s.GetStartTime() != "07:30" || s.GetOutputsMode() != "idea" {
		t.Fatalf("recurring_schedule = %+v", s)
	}
	// The created item is a recurring work item → the Recurring Items pane.
	load(t, m, "recurring-items")
	if !hasTitle(itemsOf(m, "recurring-items"), "Nightly triage sweep") {
		t.Fatalf("created item missing from Recurring Items: %v", titles(itemsOf(m, "recurring-items")))
	}
}

func TestRecurringItemsEdit(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	load(t, m, "recurring-items")

	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form for the selected recurring item")
	}
	if f.Values["title"] != "Nightly sweep" || f.Values["frequency"] != "daily" {
		t.Fatalf("the edit form must be prefilled: title=%q frequency=%q", f.Values["title"], f.Values["frequency"])
	}
	f.Set("title", "Weekly sweep")
	f.Set("frequency", "weekly")
	f.Set("interval", "3")
	f.Set("start_time", "06:00")
	run(t, m, submit(t, m, "enabled"))

	req := p.lastUpdated(t)
	if req.GetId() != "rec-1" || req.GetTitle() != "Weekly sweep" {
		t.Fatalf("update request = %+v", req)
	}
	s := req.GetRecurringSchedule()
	if s == nil || s.GetFrequency() != "weekly" || s.GetInterval() != 3 || s.GetStartTime() != "06:00" {
		t.Fatalf("update recurring_schedule = %+v", s)
	}
	load(t, m, "recurring-items")
	if !hasTitle(itemsOf(m, "recurring-items"), "Weekly sweep") {
		t.Fatalf("edited title must render in the list: %v", titles(itemsOf(m, "recurring-items")))
	}
}

func TestRecurringItemsPauseResume(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	load(t, m, "recurring-items")

	run(t, m, press(t, m, "p"))
	first := p.lastUpdated(t)
	if first.GetId() != "rec-1" || first.RecurringEnabled == nil || first.GetRecurringEnabled() {
		t.Fatalf("first p must pause: %+v", first)
	}
	if p.itemStatus("rec-1") != apiv1.WorkItemStatus_WORK_ITEM_STATUS_RECURRING {
		t.Fatal("pausing must keep the item recurring (it resumes)")
	}

	run(t, m, press(t, m, "p"))
	second := p.lastUpdated(t)
	if second.RecurringEnabled == nil || !second.GetRecurringEnabled() {
		t.Fatalf("second p must resume: %+v", second)
	}
	load(t, m, "recurring-items")
	var meta string
	for _, it := range itemsOf(m, "recurring-items") {
		if it.ID == "rec-1" {
			meta = it.Meta
		}
	}
	if !strings.HasPrefix(meta, "active") {
		t.Fatalf("resumed item must render active, meta=%q", meta)
	}
}

func TestRecurringItemsDeleteIsConfirmed(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	load(t, m, "recurring-items")

	press(t, m, "x")
	if !m.DialogOpen() {
		t.Fatal("x must open the confirmation dialog before deleting")
	}
	if len(p.deleted) != 0 {
		t.Fatal("no delete may be sent before confirmation")
	}
	press(t, m, "esc")
	if m.DialogOpen() {
		t.Fatal("esc must dismiss the confirmation dialog")
	}
	if len(p.deleted) != 0 {
		t.Fatal("dismissed confirmation must not delete")
	}

	press(t, m, "x")
	run(t, m, press(t, m, "enter"))
	if len(p.deleted) != 1 || p.deleted[0] != "rec-1" {
		t.Fatalf("deleted = %v", p.deleted)
	}
	if st := p.itemStatus("rec-1"); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
		t.Fatalf("soft delete must cancel the item, got %v", st)
	}
	load(t, m, "recurring-items")
	if hasTitle(itemsOf(m, "recurring-items"), "Nightly sweep") {
		t.Fatal("a deleted recurring item must leave the Recurring Items pane")
	}
}

func TestRecurringRunHistoryRenders(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.history["rec-1"] = []*apiv1.RecurringRunHistoryEntry{
		{
			Id:            "fire-2",
			FireAt:        timestamppb.Now(),
			Status:        "fired",
			WorkflowRunId: "run-abc12345",
			RunStatus:     "succeeded",
			RunStartedAt:  timestamppb.Now(),
			RunEndedAt:    timestamppb.Now(),
			Executions: []*apiv1.RecurringRunExecution{
				{Id: "exec-1", Status: "succeeded", StepId: "step-1", Output: "triaged-12-items"},
				{Id: "exec-2", Status: "failed", StepId: "step-2", Output: "boom-no-budget"},
			},
		},
		{
			Id:     "fire-1",
			FireAt: timestamppb.Now(),
			Status: "failed",
			Error:  "dispatch-failed-no-workflow",
		},
	}
	m := newModel(t, p)
	// A wide region: the ledger lines are long, and the detail viewport
	// clips (never wraps) at the pane width.
	m.SetSize(400, 40)
	m.SelectSource("recurring-items")
	load(t, m, "recurring-items")

	cmd := m.RequestDetail("recurring-items", "rec-1")
	if cmd == nil {
		t.Fatal("RequestDetail must produce a cmd")
	}
	msg := cmd()
	m.Update(msg)

	view := m.View()
	for _, want := range []string{
		"run history", "fire=fired", "fire=failed",
		"run=run-abc1", "(succeeded)",
		"exec-1", "step=step-1", "triaged-12-items",
		"exec-2", "boom-no-budget",
		"dispatch-failed-no-workflow",
		"Nightly sweep", "cadence",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("run-history view missing %q", want)
		}
	}
}

// ---------- idea cloud ----------

func TestIdeaCloudShowsProvenanceAndRejectedSection(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-idea01")
	p.seedIdea("idea-old", "Already rejected idea", apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED, "rec-1", "run-old01")
	m := newModel(t, p)

	load(t, m, "ideas")
	ideas := itemsOf(m, "ideas")
	if !hasTitle(ideas, "Add retry to sweeper") {
		t.Fatalf("idea-state item missing from the Idea Cloud: %v", titles(ideas))
	}
	if hasTitle(ideas, "Already rejected idea") {
		t.Fatal("the active Idea Cloud must not include dismissed ideas")
	}
	for _, it := range ideas {
		if it.ID == "idea-1" && !strings.Contains(it.Meta, "from Nightly sweep") {
			t.Fatalf("idea row must carry the spawned-by badge, meta=%q", it.Meta)
		}
	}

	load(t, m, "rejected")
	rejected := itemsOf(m, "rejected")
	if !hasTitle(rejected, "Already rejected idea") {
		t.Fatalf("the rejected population must be reachable: %v", titles(rejected))
	}
	if hasTitle(rejected, "Add retry to sweeper") {
		t.Fatal("the rejected section must not list active ideas")
	}
	for _, it := range rejected {
		if it.ID == "idea-old" && !strings.HasPrefix(it.Meta, "dismissed") {
			t.Fatalf("rejected row meta = %q", it.Meta)
		}
	}

	// Two panes render at a time now, so assert each section's PANEL TITLE by
	// focusing it (the operator's left/right move between sources).
	m.SelectSource("ideas")
	if view := m.View(); !strings.Contains(view, "Idea Cloud") {
		t.Errorf("focused ideas pane missing its title")
	}
	m.SelectSource("rejected")
	if view := m.View(); !strings.Contains(view, "Rejected Ideas") {
		t.Errorf("focused rejected pane missing its title")
	}

	// Detail carries the full provenance (spawner + spawner title + run).
	msg := m.RequestDetail("ideas", "idea-1")()
	m.Update(msg)
	dv := m.View()
	for _, want := range []string{"spawned by", "Nightly sweep", "run-idea01"} {
		if !strings.Contains(dv, want) {
			t.Errorf("idea detail missing %q", want)
		}
	}
}

func TestPromoteIdeaEntersNormalWorkItemsScope(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-idea01")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")

	run(t, m, press(t, m, "p"))

	if len(p.promoted) != 1 || p.promoted[0] != "idea-1" {
		t.Fatalf("PromoteIdea calls = %v", p.promoted)
	}
	if st := p.itemStatus("idea-1"); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING {
		t.Fatalf("a promoted idea must become a normal pending item, got %v", st)
	}
	// It left the Idea Cloud…
	load(t, m, "ideas")
	if hasTitle(itemsOf(m, "ideas"), "Add retry to sweeper") {
		t.Fatal("a promoted idea must leave the Idea Cloud")
	}
	// …and is now queryable in the normal Work Items scope.
	if got := normalScope(t, m); len(got) != 1 || got[0].GetId() != "idea-1" {
		t.Fatalf("promoted item missing from the normal work-items scope: %+v", got)
	}
}

func TestDismissIdeaLeavesActiveViews(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-idea01")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")

	press(t, m, "x")
	if !m.DialogOpen() {
		t.Fatal("dismiss must be confirmed (it is consequential)")
	}
	run(t, m, press(t, m, "enter"))

	if len(p.dismissd) != 1 || p.dismissd[0] != "idea-1" {
		t.Fatalf("DismissIdea calls = %v", p.dismissd)
	}
	if st := p.itemStatus("idea-1"); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
		t.Fatalf("dismiss maps to cancelled, got %v", st)
	}
	load(t, m, "ideas")
	if hasTitle(itemsOf(m, "ideas"), "Add retry to sweeper") {
		t.Fatal("a dismissed idea must leave the Idea Cloud")
	}
	if got := normalScope(t, m); len(got) != 0 {
		t.Fatalf("a dismissed idea must leave the active work-items views: %+v", got)
	}
	// The dismissal is durable rejection history (what the dedupe gate reads).
	load(t, m, "rejected")
	if !hasTitle(itemsOf(m, "rejected"), "Add retry to sweeper") {
		t.Fatalf("the dismissal must land in the rejected history: %v", titles(itemsOf(m, "rejected")))
	}
}

// ---------- empty states + validation ----------

func TestAutomationEmptyStates(t *testing.T) {
	// The screen renders TWO panes (the focused source + detail), so each
	// source's empty state is asserted by focusing it — the same way an
	// operator reaches it (left/right).
	//
	// Workflows are NOT here any more: they moved to the Execution tab, which
	// owns the Execution domain (executions, runs, workflows, workers).
	m := newModelWith(t, newPlane(), nil)
	for _, src := range []string{"recurring-items", "ideas", "rejected"} {
		load(t, m, src)
	}
	for _, tc := range []struct{ src, want string }{
		{"recurring-items", "no recurring items yet"},
		{"ideas", "no ideas awaiting triage"},
		{"rejected", "no dismissed ideas"},
	} {
		if !m.SelectSource(tc.src) {
			t.Fatalf("source %q not selectable", tc.src)
		}
		if view := m.View(); !strings.Contains(view, tc.want) {
			t.Errorf("focused pane %q missing its empty state: %q", tc.src, tc.want)
		}
	}
	// Workflows must NOT be an Automation source any more.
	if m.SelectSource("workflows") {
		t.Fatal("workflows must not be an Automation source (it moved to Execution)")
	}
}

func TestCreateFormValidationBlocksSubmit(t *testing.T) {
	p := newPlane()
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	f.Set("title", "Broken sweep")
	f.Set("start_date", "09/01/2026")
	f.Set("interval", "0")
	f.Set("days", "Funday")

	if cmd := submit(t, m, "outputs"); cmd != nil {
		t.Fatal("an invalid form must not submit")
	}
	if m.ActiveForm() == nil {
		t.Fatal("the form must stay open on a validation failure")
	}
	if len(f.Errors) == 0 {
		t.Fatal("the form must name the validation failure")
	}
	if len(p.created) != 0 {
		t.Fatal("no RPC may be sent for an invalid form")
	}

	// esc cancels without any write.
	press(t, m, "esc")
	if m.ActiveForm() != nil || len(p.created) != 0 {
		t.Fatal("esc must cancel the form with no side effects")
	}
}

// ---------- the calendar modal (KDate) ----------

// A date field opens the CALENDAR, and the chosen date is written back into the
// field. Typing "YYYY-MM-DD" was the old gesture; the GUI has a date control
// (`type="date"`) and this is its TUI equivalent.
func TestRecurringStartDateOpensTheCalendar(t *testing.T) {
	p := newPlane()
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	if cmd := press(t, m, "n"); cmd == nil {
		t.Fatal("n must load the create form")
	} else {
		run(t, m, cmd)
	}
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	if !f.FocusName("start_date") {
		t.Fatal("fixture: no start_date field")
	}
	before := f.Values["start_date"]

	// Enter on the field opens the calendar rather than editing text.
	if cmd := press(t, m, "enter"); cmd != nil {
		run(t, m, cmd)
	}
	if m.datePicker == nil {
		t.Fatal("enter on start_date must open the calendar")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open calendar must claim the keys")
	}
	if !m.FormOpen() {
		t.Fatal("an open calendar must count as a form being open, so Tab reaches it")
	}

	// Step a day and choose it: the field takes the date.
	dp := m.datePicker
	want := dp.Value()
	press(t, m, "right")
	if dp.Value() == want {
		t.Fatal("right must move the calendar cursor")
	}
	chosen := dp.Value()
	press(t, m, "enter")
	if m.datePicker != nil {
		t.Fatal("enter must close the calendar")
	}
	if got := m.ActiveForm().Values["start_date"]; got != chosen {
		t.Fatalf("start_date = %q, want the chosen date %q", got, chosen)
	}
	if got := m.ActiveForm().Values["start_date"]; got == before {
		t.Fatal("the field did not change — the choice was not written back")
	}
}

// Esc backs out WITHOUT touching the field.
func TestRecurringCalendarEscLeavesTheFieldAlone(t *testing.T) {
	m := newModel(t, newPlane())
	m.SelectSource("recurring-items")
	if cmd := press(t, m, "n"); cmd == nil {
		t.Fatal("n must load the create form")
	} else {
		run(t, m, cmd)
	}
	f := m.ActiveForm()
	f.FocusName("start_date")
	before := f.Values["start_date"]
	if cmd := press(t, m, "enter"); cmd != nil {
		run(t, m, cmd)
	}
	if m.datePicker == nil {
		t.Fatal("fixture: the calendar did not open")
	}
	press(t, m, "right") // move, then abandon
	press(t, m, "esc")
	if m.datePicker != nil {
		t.Fatal("esc must close the calendar")
	}
	if got := m.ActiveForm().Values["start_date"]; got != before {
		t.Fatalf("start_date = %q, want it untouched (%q)", got, before)
	}
}

// The weekday field is a MULTI-SELECT: each day toggles independently, and the
// chosen set reaches the schedule in calendar order.
func TestRecurringDaysTogglesIndependently(t *testing.T) {
	m := newModel(t, newPlane())
	m.SelectSource("recurring-items")
	if cmd := press(t, m, "n"); cmd == nil {
		t.Fatal("n must load the create form")
	} else {
		run(t, m, cmd)
	}
	f := m.ActiveForm()
	if !f.FocusName("days") {
		t.Fatal("fixture: no days field")
	}

	// Toggle Mon (the cursor starts there), then walk to Wed and toggle that.
	press(t, m, "space")
	f = m.ActiveForm()
	if got := f.MultiValues("days"); len(got) != 1 || got[0] != "Mon" {
		t.Fatalf("days after one toggle = %v, want [Mon]", got)
	}
	press(t, m, "right") // Mon → Tue
	press(t, m, "right") // Tue → Wed
	press(t, m, "space")
	f = m.ActiveForm()
	got := f.MultiValues("days")
	if len(got) != 2 || got[0] != "Mon" || got[1] != "Wed" {
		t.Fatalf("days = %v, want [Mon Wed] in calendar order — each option must toggle independently", got)
	}
	// Toggling Mon off leaves Wed alone.
	press(t, m, "left")
	press(t, m, "left")
	press(t, m, "space")
	f = m.ActiveForm()
	if got := f.MultiValues("days"); len(got) != 1 || got[0] != "Wed" {
		t.Fatalf("days = %v, want [Wed] — toggling one day must not clear another", got)
	}
}

// The edit form shows the STORED days as selected. Without seeding, an edit form
// opened with nothing ticked and saving silently cleared the schedule.
func TestRecurringEditPrefillsSelectedDays(t *testing.T) {
	p := newPlane()
	// The shared fixture seeds a DAILY schedule with NO weekday selection, which is
	// legitimate (empty days = every day) and therefore proves nothing about
	// pre-fill. Give this one explicit days so the assertion is about the seeding.
	w := p.seedRecurring("rec-1", "Nightly sweep", true)
	w.RecurringSchedule.Days = []string{"Mon", "Wed"}
	m := newModel(t, p)
	m.SelectSource("recurring-items")
	load(t, m, "recurring-items")
	if len(itemsOf(m, "recurring-items")) == 0 {
		t.Fatal("fixture: no recurring item listed")
	}
	if cmd := press(t, m, "e"); cmd == nil {
		t.Fatal("e must load the edit form")
	} else {
		run(t, m, cmd)
	}
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the edit form")
	}
	if got := f.MultiValues("days"); len(got) == 0 {
		t.Fatalf("days = %v, want the stored selection pre-filled — otherwise saving clears it", got)
	}
}

// ---------- bulk idea triage ----------

// 'A' accepts EVERY idea the list is showing. Triage is the reason the cloud
// exists, and a run that spawned eight ideas should not ask for eight keystrokes
// and eight confirmations.
func TestAcceptAllIdeasPromotesEveryListed(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-a")
	p.seedIdea("idea-2", "Document the sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-b")
	p.seedIdea("idea-3", "Add sweeper metrics", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-c")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")
	if got := len(itemsOf(m, "ideas")); got != 3 {
		t.Fatalf("fixture: %d ideas listed, want 3", got)
	}

	// Accept takes no confirmation: promoting is reversible (a promoted item is a
	// normal pending work item, editable like any other).
	run(t, m, press(t, m, "A"))

	if len(p.promoted) != 3 {
		t.Fatalf("PromoteIdea calls = %v, want all three listed ideas", p.promoted)
	}
	for _, id := range []string{"idea-1", "idea-2", "idea-3"} {
		if st := p.itemStatus(id); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING {
			t.Errorf("%s status = %v, want pending after accept", id, st)
		}
	}
	if !strings.Contains(m.notice, "accepted 3") {
		t.Fatalf("notice = %q, want it to report all three accepted", m.notice)
	}
	// The cloud is empty afterwards.
	load(t, m, "ideas")
	if len(itemsOf(m, "ideas")) != 0 {
		t.Fatalf("ideas still listed after accept-all: %d", len(itemsOf(m, "ideas")))
	}
}

// 'R' rejects every listed idea, but it is CONFIRMED: a dismissal is durable
// rejection history that the automation's dedupe gate reads, so it is not done by
// accident.
func TestRejectAllIdeasIsConfirmed(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-a")
	p.seedIdea("idea-2", "Document the sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-b")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")

	press(t, m, "R")
	if !m.DialogOpen() {
		t.Fatal("reject-all must be confirmed — a dismissal is durable rejection history")
	}
	if m.Open == nil || !m.Open.Danger {
		t.Error("reject-all is a destructive action and must present as danger")
	}
	// Nothing has happened yet.
	if len(p.dismissd) != 0 {
		t.Fatalf("DismissIdea ran before confirmation: %v", p.dismissd)
	}

	run(t, m, press(t, m, "enter"))
	if len(p.dismissd) != 2 {
		t.Fatalf("DismissIdea calls = %v, want both listed ideas", p.dismissd)
	}
	for _, id := range []string{"idea-1", "idea-2"} {
		if st := p.itemStatus(id); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_CANCELLED {
			t.Errorf("%s status = %v, want cancelled after reject", id, st)
		}
	}
	// Both land in the REJECTED section (what the dedupe gate reads).
	load(t, m, "rejected")
	if got := len(itemsOf(m, "rejected")); got != 2 {
		t.Fatalf("rejected history has %d entries, want 2", got)
	}
}

// Cancelling the confirmation leaves every idea ALONE — the rows must come back,
// because the removal was optimistic.
func TestRejectAllIdeasCancelledChangesNothing(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-a")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")

	press(t, m, "R")
	if !m.DialogOpen() {
		t.Fatal("fixture: no confirmation")
	}
	press(t, m, "esc")
	if len(p.dismissd) != 0 {
		t.Fatalf("a cancelled reject-all still dismissed: %v", p.dismissd)
	}
	if st := p.itemStatus("idea-1"); st != apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA {
		t.Fatalf("idea status = %v, want it untouched (IDEA)", st)
	}
	load(t, m, "ideas")
	if len(itemsOf(m, "ideas")) != 1 {
		t.Fatal("the idea must still be listed after cancelling")
	}
}

// A PARTIAL failure is reported per-idea. This is the case a single batched RPC
// would hide, and the one that matters: the operator has to know WHICH ideas are
// still awaiting a decision.
func TestBulkAcceptReportsPerIdeaFailure(t *testing.T) {
	p := newPlane()
	p.seedRecurring("rec-1", "Nightly sweep", true)
	p.seedIdea("idea-1", "Add retry to sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-a")
	p.seedIdea("idea-2", "Document the sweeper", apiv1.WorkItemStatus_WORK_ITEM_STATUS_IDEA, "rec-1", "run-b")
	m := newModel(t, p)
	m.SelectSource("ideas")
	load(t, m, "ideas")

	// Make ONE of them fail, the way a server-side refusal would. The others go
	// through the PLANE, so the test exercises a real partial success rather than a
	// no-op that silently leaves every idea where it was.
	m.rpcPromote = func(ctx context.Context, id string) error {
		if id == "idea-2" {
			return errors.New("refused")
		}
		_, err := p.PromoteIdea(ctx, connect.NewRequest(&apiv1.PromoteIdeaRequest{Id: id}))
		return err
	}
	run(t, m, press(t, m, "A"))

	if !strings.Contains(m.notice, "1 of 2") || !strings.Contains(m.notice, "still listed") {
		t.Fatalf("notice = %q, want it to report 1 of 2 with the failure still listed", m.notice)
	}
	// The reload puts the FAILED row back: the optimistic removal is only safe
	// because the durable truth is the server's list.
	load(t, m, "ideas")
	if got := itemsOf(m, "ideas"); len(got) != 1 || got[0].ID != "idea-2" {
		t.Fatalf("ideas after a partial failure = %+v, want only idea-2 back", got)
	}
}

// With nothing awaiting triage, the bulk keys SAY so rather than no-op.
func TestBulkIdeaDecisionOnAnEmptyCloudRefuses(t *testing.T) {
	m := newModel(t, newPlane())
	m.SelectSource("ideas")
	load(t, m, "ideas")
	if len(itemsOf(m, "ideas")) != 0 {
		t.Fatal("fixture: expected an empty Idea Cloud")
	}
	press(t, m, "A")
	if !strings.Contains(m.notice, "no ideas") {
		t.Fatalf("notice = %q, want it to say there is nothing to triage", m.notice)
	}
}
