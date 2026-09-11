package work

// workitems.go — Work Items full CRUD + the collection semantics the
// platform contract requires:
//
//   - CreateWorkItem (title / kind / parent / description / acceptance
//     criteria / priority / budgets / context window / workflow / runtime
//     image / context files / auto-start),
//   - UpdateWorkItem for every mutable field, including status + priority,
//     the workflow binding, the schedule (scheduled_start_at) and
//     auto_start_workflow,
//   - AssignWorker / UnassignWorker,
//   - ReorderWorkItems (the ONLY mutation of sequence order),
//   - DeleteWorkItem (soft delete → cancelled), ArchiveWorkItem /
//     RestoreWorkItem — the last two Confirm-gated.
//
// Switching kind re-resolves the hierarchy and epic/feature are
// non-schedulable (worker/schedule cleared) — both are enforced
// server-side; the screen offers kind on create and edit and surfaces the
// server's answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// form modes for the work-item surfaces.
const (
	formCreateItem   = "item-create"
	formEditItem     = "item-edit"
	formStatusItem   = "item-status"
	formAssignItem   = "item-assign"
	formScheduleItem = "item-schedule"
)

// itemFormMsg lands the option lists (create) or the item being edited.
type itemFormMsg struct {
	mode      string
	item      *apiv1.WorkItem
	projects  []projectOpt
	workflows []workflowOpt
	err       error
}

// kindOptions is the create form's kind vocabulary (max 4 levels; the
// hierarchy is re-resolved server-side when the kind changes).
func kindOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "epic", Label: "epic"},
		{Value: "feature", Label: "feature"},
		{Value: "task", Label: "task"},
		{Value: "subtask", Label: "subtask"},
	}
}

// statusOptions is the user-assignable status vocabulary. The
// system-managed states (running/blocked/idea/archived/…) are deliberately
// absent — a human cannot assign them.
func statusOptions() []kit2.Option {
	return []kit2.Option{
		{Value: "pending", Label: "pending"},
		{Value: "ready", Label: "ready"},
		{Value: "scheduled", Label: "scheduled"},
		{Value: "assigned", Label: "assigned"},
		{Value: "succeeded", Label: "succeeded"},
		{Value: "failed", Label: "failed"},
		{Value: "cancelled", Label: "cancelled"},
		{Value: "skipped", Label: "skipped"},
	}
}

func kindFromName(v string) apiv1.WorkItemKind {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "epic":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC
	case "feature":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE
	case "subtask":
		return apiv1.WorkItemKind_WORK_ITEM_KIND_SUBTASK
	default:
		return apiv1.WorkItemKind_WORK_ITEM_KIND_TASK
	}
}

func statusFromName(v string) apiv1.WorkItemStatus {
	for _, o := range statusOptions() {
		if o.Value == strings.ToLower(strings.TrimSpace(v)) {
			return apiv1.WorkItemStatus(apiv1.WorkItemStatus_value["WORK_ITEM_STATUS_"+strings.ToUpper(o.Value)])
		}
	}
	return apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING
}

// statusForEdit is the status the edit form offers: the item's real status
// when it is user-assignable, "pending" otherwise.
func statusForEdit(s apiv1.WorkItemStatus) string {
	cur := statusPill(s)
	for _, o := range statusOptions() {
		if o.Value == cur {
			return cur
		}
	}
	return "pending"
}

// validateJSON accepts an empty value or a well-formed JSON document.
func validateJSON(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if !json.Valid([]byte(v)) {
		return fmt.Errorf("must be valid JSON")
	}
	return nil
}

func validateNonNegativeInt(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fmt.Errorf("must be a whole number")
	}
	if n < 0 {
		return fmt.Errorf("must be >= 0")
	}
	return nil
}

func validateRFC3339(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err != nil {
		return fmt.Errorf("use RFC3339 (2026-09-01T09:00:00Z)")
	}
	return nil
}

// prepCreateItem loads the form's option lists (projects, workflows) — the
// create form cannot be built without them.
func (m *Model) prepCreateItem() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		msg := itemFormMsg{mode: formCreateItem}
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

// prepEditItem fetches the selected item so every mutable field is
// prefilled from real values.
func (m *Model) prepEditItem(mode string) tea.Cmd {
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
			return itemFormMsg{mode: mode, err: err}
		}
		return itemFormMsg{mode: mode, item: resp.Msg.GetWorkItem()}
	}
}

// newItemCreateForm builds the typed create form.
func (m *Model) newItemCreateForm() *kit2.Form {
	projOpts := make([]kit2.Option, 0, len(m.projects))
	for _, p := range m.projects {
		projOpts = append(projOpts, kit2.Option{Value: p.ID, Label: p.Name})
	}
	wfOpts := append([]kit2.Option{{Value: "", Label: "none"}}, make([]kit2.Option, 0, len(m.workflows))...)
	for _, w := range m.workflows {
		wfOpts = append(wfOpts, kit2.Option{Value: w.ID, Label: w.Name})
	}
	f := kit2.NewForm("New work item",
		kit2.FieldSpec{Name: "title", Label: "Title", Kind: kit2.KText, Required: true, Placeholder: "add retry to the sweeper"},
		kit2.FieldSpec{Name: "project", Label: "Project", Kind: kit2.KSelect, Options: projOpts, Required: true, Initial: projOpts[0].Value},
		kit2.FieldSpec{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Options: kindOptions(), Initial: "task"},
		kit2.FieldSpec{Name: "parent", Label: "Parent id", Kind: kit2.KText, Placeholder: "empty = top level (epic)"},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "acceptance", Label: "Acceptance criteria", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "priority", Label: "Priority", Kind: kit2.KNumber, Initial: "0", Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "budgets", Label: "Budgets", Kind: kit2.KJSON, Placeholder: `{"tokens":100000}`, Validate: validateJSON},
		kit2.FieldSpec{Name: "context_window", Label: "Context window", Kind: kit2.KNumber, Initial: "0", Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "workflow", Label: "Workflow", Kind: kit2.KSelect, Options: wfOpts, Initial: ""},
		kit2.FieldSpec{Name: "runtime_image", Label: "Runtime image", Kind: kit2.KText, Placeholder: "empty = base image"},
		kit2.FieldSpec{Name: "context_files", Label: "Context files", Kind: kit2.KText, Placeholder: "/abs/path/a.go,/abs/dir"},
		kit2.FieldSpec{Name: "auto_start", Label: "Auto-start workflow", Kind: kit2.KCheckbox, Initial: "false"},
	)
	m.wireItemForm(f, formCreateItem, "")
	return f
}

// newItemEditForm builds the edit form for an existing item: every mutable
// field, prefilled from the item's real values.
func (m *Model) newItemEditForm(w *apiv1.WorkItem) *kit2.Form {
	projOpts := make([]kit2.Option, 0, len(m.projects))
	for _, p := range m.projects {
		projOpts = append(projOpts, kit2.Option{Value: p.ID, Label: p.Name})
	}
	return m.editFormFor(w, projOpts)
}

func (m *Model) editFormFor(w *apiv1.WorkItem, projOpts []kit2.Option) *kit2.Form {
	f := kit2.NewForm("Edit work item",
		kit2.FieldSpec{Name: "title", Label: "Title", Kind: kit2.KText, Required: true, Initial: w.GetTitle()},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KTextArea, Initial: w.GetDescription()},
		kit2.FieldSpec{Name: "acceptance", Label: "Acceptance criteria", Kind: kit2.KTextArea, Initial: w.GetAcceptanceCriteria()},
		kit2.FieldSpec{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Options: kindOptions(), Initial: kindBadge(w.GetKind())},
		kit2.FieldSpec{Name: "status", Label: "Status", Kind: kit2.KSelect, Options: statusOptions(), Initial: statusForEdit(w.GetStatus())},
		kit2.FieldSpec{Name: "priority", Label: "Priority", Kind: kit2.KNumber, Initial: strconv.Itoa(int(w.GetPriority())), Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "budgets", Label: "Budgets", Kind: kit2.KJSON, Initial: w.GetBudgets(), Validate: validateJSON},
		kit2.FieldSpec{Name: "context_window", Label: "Context window", Kind: kit2.KNumber, Initial: strconv.Itoa(int(w.GetContextWindow())), Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "workflow", Label: "Workflow id", Kind: kit2.KText, Initial: w.GetWorkflowId()},
		kit2.FieldSpec{Name: "runtime_image", Label: "Runtime image", Kind: kit2.KText, Initial: w.GetRuntimeImage()},
		kit2.FieldSpec{Name: "context_files", Label: "Context files", Kind: kit2.KText, Initial: strings.Join(w.GetContextFiles(), ",")},
		kit2.FieldSpec{Name: "scheduled_start", Label: "Scheduled start", Kind: kit2.KText, Initial: rfc3339OrEmpty(w.GetScheduledStartAt()), Validate: validateRFC3339},
		kit2.FieldSpec{Name: "auto_start", Label: "Auto-start workflow", Kind: kit2.KCheckbox, Initial: boolStr(w.GetAutoStartWorkflow())},
	)
	m.wireItemForm(f, formEditItem, w.GetId())
	return f
}

// newItemStatusForm is the quick status/priority editor (the change-status
// gesture without walking the whole edit form).
func (m *Model) newItemStatusForm(w *apiv1.WorkItem) *kit2.Form {
	f := kit2.NewForm("Change status",
		kit2.FieldSpec{Name: "status", Label: "Status", Kind: kit2.KSelect, Options: statusOptions(), Initial: statusForEdit(w.GetStatus())},
		kit2.FieldSpec{Name: "priority", Label: "Priority", Kind: kit2.KNumber, Initial: strconv.Itoa(int(w.GetPriority())), Validate: validateNonNegativeInt},
	)
	m.wireItemForm(f, formStatusItem, w.GetId())
	return f
}

// newItemAssignForm assigns a worker (worker_ref is the JSON
// {worker_id, version} binding the service expects).
func (m *Model) newItemAssignForm(w *apiv1.WorkItem) *kit2.Form {
	f := kit2.NewForm("Assign worker",
		kit2.FieldSpec{Name: "worker_ref", Label: "Worker ref", Kind: kit2.KJSON, Required: true, Initial: w.GetAssignedWorkerRef(), Placeholder: `{"worker_id":"wrk_…","version":1}`, Validate: validateJSON},
	)
	m.wireItemForm(f, formAssignItem, w.GetId())
	return f
}

// newItemScheduleForm sets scheduled_start_at (the ScheduleWorkItem path:
// the schedule is an UpdateWorkItem field, opt-in via auto_start_workflow).
func (m *Model) newItemScheduleForm(w *apiv1.WorkItem) *kit2.Form {
	f := kit2.NewForm("Schedule work item",
		kit2.FieldSpec{Name: "scheduled_start", Label: "Start at", Kind: kit2.KText, Required: true, Initial: rfc3339OrEmpty(w.GetScheduledStartAt()), Placeholder: "2026-09-01T09:00:00Z", Validate: validateRFC3339},
		kit2.FieldSpec{Name: "auto_start", Label: "Auto-start workflow", Kind: kit2.KCheckbox, Initial: boolStr(w.GetAutoStartWorkflow())},
	)
	m.wireItemForm(f, formScheduleItem, w.GetId())
	return f
}

// wireItemForm installs the submit handler: it builds the RPC request from
// the collected values and hands it to the mutation executor. No screen
// calls a write RPC from the update loop.
func (m *Model) wireItemForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		title := strings.TrimSpace(v["title"])
		switch mode {
		case formCreateItem:
			if v["project"] == "" {
				return nil, fmt.Errorf("a work item belongs to a project")
			}
			req := &apiv1.CreateWorkItemRequest{
				ProjectId:          v["project"],
				ParentId:           strings.TrimSpace(v["parent"]),
				Kind:               kindFromName(v["kind"]),
				Title:              title,
				Description:        v["description"],
				AcceptanceCriteria: v["acceptance"],
				Priority:           int32(atoiOr(v["priority"], 0)),
				Budgets:            strings.TrimSpace(v["budgets"]),
				ContextWindow:      int32(atoiOr(v["context_window"], 0)),
				WorkflowId:         strings.TrimSpace(v["workflow"]),
				RuntimeImage:       strings.TrimSpace(v["runtime_image"]),
				ContextFiles:       splitList(v["context_files"]),
				AutoStartWorkflow:  boolPtr(v["auto_start"] == "true"),
			}
			name := "create work item " + strconv.Quote(req.GetTitle())
			return m.Mutate(mutate.Request{
				Name: name, Source: srcWorkItems,
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.CreateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formEditItem:
			auto := v["auto_start"] == "true"
			prio := int32(atoiOr(v["priority"], 0))
			cw := int32(atoiOr(v["context_window"], 0))
			kind := kindFromName(v["kind"])
			status := statusFromName(v["status"])
			req := &apiv1.UpdateWorkItemRequest{
				Id:                 id,
				Title:              strPtr(title),
				Description:        strPtr(v["description"]),
				AcceptanceCriteria: strPtr(v["acceptance"]),
				Kind:               &kind,
				Status:             &status,
				Priority:           &prio,
				Budgets:            strPtr(strings.TrimSpace(v["budgets"])),
				ContextWindow:      &cw,
				WorkflowId:         strPtr(strings.TrimSpace(v["workflow"])),
				RuntimeImage:       strPtr(strings.TrimSpace(v["runtime_image"])),
				AutoStartWorkflow:  &auto,
				ContextFiles:       &apiv1.ContextFiles{Files: splitList(v["context_files"])},
				ScheduledStartAt:   tsOrNil(v["scheduled_start"]),
			}
			name := "save work item " + strconv.Quote(title)
			return m.Mutate(mutate.Request{
				Name: name, Source: srcWorkItems,
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formStatusItem:
			status := statusFromName(v["status"])
			prio := int32(atoiOr(v["priority"], 0))
			req := &apiv1.UpdateWorkItemRequest{Id: id, Status: &status, Priority: &prio}
			name := "set status " + statusPill(status)
			return m.Mutate(mutate.Request{
				Name: name, Source: srcWorkItems,
				Apply:    func() { m.setRowMeta(srcWorkItems, id, statusPill(status)) },
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil

		case formAssignItem:
			ref := strings.TrimSpace(v["worker_ref"])
			return m.Mutate(mutate.Request{
				Name: "assign worker", Source: srcWorkItems,
				Apply:    func() { m.setRowMeta(srcWorkItems, id, "assigned") },
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.AssignWorker(ctx, connect.NewRequest(&apiv1.AssignWorkerRequest{Id: id, WorkerRef: ref}))
					return err
				},
			}), nil

		case formScheduleItem:
			auto := v["auto_start"] == "true"
			req := &apiv1.UpdateWorkItemRequest{
				Id:                id,
				ScheduledStartAt:  tsOrNil(v["scheduled_start"]),
				AutoStartWorkflow: &auto,
			}
			return m.Mutate(mutate.Request{
				Name: "schedule work item", Source: srcWorkItems,
				Apply:    func() { m.setRowMeta(srcWorkItems, id, "scheduled") },
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					_, err := m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(req))
					return err
				},
			}), nil
		}
		return nil, nil
	}
}

// itemActions builds the entity-bound actions for the focused row.
// Destructive actions (delete/archive/restore) carry a Confirm prompt.
func (m *Model) itemActions() []kit2.Action {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id, title := it.ID, it.Title
	if id == "" || strings.HasPrefix(id, "col:") {
		return nil
	}
	if m.ViewMode() == viewArchive {
		return []kit2.Action{{
			Label: "restore", Key: "R", Source: srcWorkItems,
			Confirm:  "Restore " + title + "?\nIt returns to the active views at the status it was archived from.",
			Apply:    func() { m.RemoveRow(srcWorkItems, id) },
			Rollback: func() { m.Refresh(srcWorkItems) },
			Do: func(ctx context.Context) error {
				_, err := m.cl.WorkItems.RestoreWorkItem(ctx, connect.NewRequest(&apiv1.RestoreWorkItemRequest{Id: id}))
				return err
			},
		}}
	}
	return []kit2.Action{
		{
			Label: "toggle auto-start", Key: "y", Source: srcWorkItems,
			Do: func(ctx context.Context) error { return m.rpcToggleAutoStart(ctx, id) },
		},
		{
			Label: "unassign worker", Key: "W", Source: srcWorkItems,
			Do: func(ctx context.Context) error {
				_, err := m.cl.WorkItems.UnassignWorker(ctx, connect.NewRequest(&apiv1.UnassignWorkerRequest{Id: id}))
				return err
			},
		},
		{
			Label: "archive", Key: "a", Danger: true, Source: srcWorkItems,
			Confirm:  "Archive " + title + "?\nTerminal items with children cannot be archived — the server rejects it. Reversible with restore.",
			Apply:    func() { m.RemoveRow(srcWorkItems, id) },
			Rollback: func() { m.Refresh(srcWorkItems) },
			Do: func(ctx context.Context) error {
				_, err := m.cl.WorkItems.ArchiveWorkItem(ctx, connect.NewRequest(&apiv1.ArchiveWorkItemRequest{Id: id}))
				return err
			},
		},
		{
			Label: "delete", Key: "x", Danger: true, Source: srcWorkItems,
			Confirm:  "Delete " + title + "?\nThis soft-deletes the item (status → cancelled) and it leaves every active view.",
			Apply:    func() { m.setRowMeta(srcWorkItems, id, "cancelled") },
			Rollback: func() { m.Refresh(srcWorkItems) },
			Do: func(ctx context.Context) error {
				_, err := m.cl.WorkItems.DeleteWorkItem(ctx, connect.NewRequest(&apiv1.DeleteWorkItemRequest{Id: id}))
				return err
			},
		},
	}
}

// rpcToggleAutoStart flips auto_start_workflow from the item's real value.
func (m *Model) rpcToggleAutoStart(ctx context.Context, id string) error {
	cur, err := m.cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
	if err != nil {
		return err
	}
	next := !cur.Msg.GetWorkItem().GetAutoStartWorkflow()
	_, err = m.cl.WorkItems.UpdateWorkItem(ctx, connect.NewRequest(&apiv1.UpdateWorkItemRequest{
		Id:                id,
		AutoStartWorkflow: &next,
	}))
	return err
}

// reorderChildren moves the selected item within its sibling sequence and
// persists the new order with ReorderWorkItems — the only RPC that writes
// sort_order. The siblings are re-read from the plane so the order sent is
// the server's current one, not a stale display grouping.
func (m *Model) reorderChildren(delta int) tea.Cmd {
	it, ok := m.ActiveItem()
	if !ok {
		return nil
	}
	id := it.ID
	cl := m.cl
	return m.Mutate(mutate.Request{
		Name: "reorder children", Source: srcWorkItems,
		Rollback: func() { m.Refresh(srcWorkItems) },
		Do: func(ctx context.Context) error {
			cur, err := cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
			if err != nil {
				return err
			}
			item := cur.Msg.GetWorkItem()
			sibs, err := cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
				ProjectId: item.GetProjectId(),
				ParentId:  strPtr(item.GetParentId()),
				PageSize:  200,
				IdeaScope: apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
			}))
			if err != nil {
				return err
			}
			order := make([]*apiv1.WorkItem, 0, len(sibs.Msg.GetWorkItems()))
			order = append(order, sibs.Msg.GetWorkItems()...)
			sortSiblings(order)
			ids := make([]string, 0, len(order))
			from := -1
			for i, s := range order {
				if s.GetId() == id {
					from = i
				}
				ids = append(ids, s.GetId())
			}
			if from < 0 {
				return fmt.Errorf("work item is not in its sibling sequence")
			}
			to := from + delta
			if to < 0 || to >= len(ids) {
				return fmt.Errorf("already at the end of the sequence")
			}
			ids[from], ids[to] = ids[to], ids[from]
			_, err = cl.WorkItems.ReorderWorkItems(ctx, connect.NewRequest(&apiv1.ReorderWorkItemsRequest{
				ProjectId: item.GetProjectId(),
				ParentId:  item.GetParentId(),
				ChildIds:  ids,
			}))
			return err
		},
	})
}

// setRowMeta applies an optimistic local state change to a row.
func (m *Model) setRowMeta(src, id, meta string) {
	m.MutateRow(src, id, func(r *kit2.Row) { r.Meta = meta })
}

// ---------------- helpers ----------------

func splitList(v string) []string {
	var out []string
	for _, raw := range strings.Split(v, ",") {
		if s := strings.TrimSpace(raw); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func atoiOr(v string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

func tsOrNil(v string) *timestamppb.Timestamp {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

func rfc3339OrEmpty(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

// detailBody renders the item's description + acceptance criteria (the
// acceptance-criteria body the GUI shows).
func detailBody(w *apiv1.WorkItem) string {
	var b strings.Builder
	if d := w.GetDescription(); d != "" {
		b.WriteString(d + "\n")
	}
	if ac := w.GetAcceptanceCriteria(); ac != "" {
		b.WriteString("\nacceptance criteria:\n" + ac + "\n")
	}
	if ar := w.GetAcceptanceReview(); ar != "" {
		b.WriteString("\nacceptance review:\n" + ar + "\n")
	}
	if len(w.GetBlockedBy()) > 0 {
		b.WriteString("\nblocked by:\n")
		for _, bl := range w.GetBlockedBy() {
			b.WriteString("  " + bl.GetId() + " " + bl.GetTitle() + " (" + bl.GetStatus() + ")\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

var _ = screenkit.FmtInt
