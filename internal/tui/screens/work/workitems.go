package work

// workitems.go — Work Items full CRUD + the collection semantics the
// platform contract requires:
//
//   - CreateWorkItem (title / kind / parent / description / acceptance
//     criteria / priority / budgets / context window / workflow / runtime
//     image / execution mode / context files / auto-start),
//   - UpdateWorkItem for every mutable field, including status + priority,
//     the workflow binding, the execution-mode OVERRIDE (set or cleared — see
//     itemExecutionModeInherit), the schedule (scheduled_start_at) and
//     auto_start_workflow,
//   - AssignWorker / UnassignWorker,
//   - ReorderWorkItems (the ONLY mutation of sequence order),
//   - HardDeleteWorkItem (permanent — the GUI's Delete, which the TUI's ctrl+x
//     now performs; DeleteWorkItem's soft delete is a STATUS CHANGE and lives in
//     the status editor), ArchiveWorkItem / RestoreWorkItem — all Confirm-gated.
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
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/mutate"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// form modes for the work-item surfaces.
const (
	formCreateItem = "item-create"
	formEditItem   = "item-edit"
	formStatusItem = "item-status"
)

// itemFormMsg lands the option lists (create) or the item being edited.
type itemFormMsg struct {
	mode      string
	item      *apiv1.WorkItem
	projects  []projectOpt
	workflows []workflowOpt
	// parents / images are the KPicker option lists: the work items that can be a parent and the
	// runtime images that can be selected (as options, never as ids the operator would have to know).
	parents []kit2.Option
	images  []kit2.Option
	// hiddenWorkflows is how many workflow templates the bulk-set picker EXCLUDED because they cannot
	// run (not PUBLISHED/DEPRECATED, or with no steps).
	hiddenWorkflows int
	// parentKinds maps each parent option's id to its kind, so the create form can derive the child
	// kind from the chosen parent; parentProjects maps it to its project.
	parentKinds    map[string]apiv1.WorkItemKind
	parentProjects map[string]string
	err            error
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

// statusOptions is the user-assignable status vocabulary.
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

// itemExecutionModeInherit is the THIRD choice an ITEM has and a project does not: "no override,
// follow the project".
const itemExecutionModeInherit = "inherit"

// itemExecutionModeOptions is the ITEM-side vocabulary: inherit / local / runtime.
func itemExecutionModeOptions(inherited apiv1.ExecutionMode) []kit2.Option {
	label := "inherit — from the project"
	if s := executionModeString(inherited); s != "" {
		// The inherited mode is STATED where the form knows it — the same shape the GUI's select uses
		// ("Inherit from project (<mode>)", ExecutionModeSelect.tsx:86-102).
		label = "inherit from project (" + s + ")"
	}
	opts := make([]kit2.Option, 0, 3)
	opts = append(opts, kit2.Option{Value: itemExecutionModeInherit, Label: label})
	descriptions := strings.NewReplacer(" (default)", "", " (opt-in)", "")
	for _, o := range executionModeOptions() {
		opts = append(opts, kit2.Option{Value: o.Value, Label: descriptions.Replace(o.Label)})
	}
	return opts
}

// itemExecutionModeField is the edit form's seed: the item's stored OVERRIDE, or "inherit" when it
// has none.
func itemExecutionModeField(w *apiv1.WorkItem) string {
	if s := executionModeString(w.GetExecutionMode()); s != "" {
		return s
	}
	return itemExecutionModeInherit
}

// executionModeDetail renders a work item's execution mode for the DETAIL pane.
func executionModeDetail(w *apiv1.WorkItem) string {
	eff := executionModeString(w.GetEffectiveExecutionMode())
	if eff == "" {
		eff = "local"
	}
	if ov := executionModeString(w.GetExecutionMode()); ov != "" {
		return eff + " (item override: " + ov + ")"
	}
	return eff + " (inherited from the project)"
}

// projectExecutionMode is a listed project's mode, for the inherit label.
func (m *Model) projectExecutionMode(id string) apiv1.ExecutionMode {
	for _, p := range m.projects {
		if p.ID == id {
			return p.Mode
		}
	}
	return apiv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED
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
	m.formLoading = true
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
			msg.projects = append(msg.projects, projectOpt{ID: p.GetId(), Name: p.GetName(), Mode: p.GetExecutionMode()})
		}
		if wr, err := cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{PageSize: 100})); err == nil {
			for _, w := range wr.Msg.GetWorkflows() {
				msg.workflows = append(msg.workflows, workflowOpt{ID: w.GetId(), Name: w.GetName()})
			}
		}
		// The parent picker lists the real work items, so a parent is CHOSEN by name instead of typed as an
		// id.
		msg.parents, msg.parentKinds, msg.parentProjects = loadParentOptions(ctx, cl)
		// The runtime-image picker lists the real images (value = the tag the
		// request carries).
		if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
			msg.images = append(msg.images, kit2.Option{Value: "", Label: "— none (base image) —"})
			for _, img := range lr.Msg.GetRuntimeImages() {
				msg.images = append(msg.images, kit2.Option{Value: img.GetTag(), Label: img.GetName() + " (" + img.GetTag() + ")"})
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
	m.formLoading = true
	id := it.ID
	cl := m.cl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		resp, err := cl.WorkItems.GetWorkItem(ctx, connect.NewRequest(&apiv1.GetWorkItemRequest{Id: id}))
		if err != nil {
			return itemFormMsg{mode: mode, err: err}
		}
		msg := itemFormMsg{mode: mode, item: resp.Msg.GetWorkItem()}
		// The EXECUTION-MODE field's inherit option names the mode it inherits FROM, and GetWorkItem does
		// not carry the project's mode — so the edit form needs the project list too.
		if mode == formEditItem {
			if pr, err := cl.Projects.ListProjects(ctx, connect.NewRequest(&apiv1.ListProjectsRequest{PageSize: 100})); err == nil {
				for _, p := range pr.Msg.GetProjects() {
					msg.projects = append(msg.projects, projectOpt{ID: p.GetId(), Name: p.GetName(), Mode: p.GetExecutionMode()})
				}
			}
		}
		// The edit form's pickers need the same option lists as the create form.
		if wr, err := cl.Workflows.ListWorkflows(ctx, connect.NewRequest(&apiv1.ListWorkflowsRequest{PageSize: 100})); err == nil {
			for _, w := range wr.Msg.GetWorkflows() {
				msg.workflows = append(msg.workflows, workflowOpt{ID: w.GetId(), Name: w.GetName()})
			}
		}
		if lr, err := cl.Images.ListRuntimeImages(ctx, connect.NewRequest(&apiv1.ListRuntimeImagesRequest{PageSize: 100})); err == nil {
			msg.images = append(msg.images, kit2.Option{Value: "", Label: "— none (base image) —"})
			for _, img := range lr.Msg.GetRuntimeImages() {
				msg.images = append(msg.images, kit2.Option{Value: img.GetTag(), Label: img.GetName() + " (" + img.GetTag() + ")"})
			}
		}
		return msg
	}
}

// workflowPickerOpts is the workflow picker's option list ("none" first).
func (m *Model) workflowPickerOpts() []kit2.Option {
	opts := []kit2.Option{{Value: "", Label: "— none —"}}
	for _, w := range m.workflows {
		opts = append(opts, kit2.Option{Value: w.ID, Label: w.Name})
	}
	return opts
}

// pickerOptsWithCurrent guarantees the option list contains the item's CURRENT value, so editing an
// entity never silently drops a reference the fetched list does not know about (a workflow.
func pickerOptsWithCurrent(opts []kit2.Option, cur, prefix string) []kit2.Option {
	if cur == "" {
		return opts
	}
	for _, o := range opts {
		if o.Value == cur {
			return opts
		}
	}
	return append([]kit2.Option{{Value: cur, Label: prefix + cur + " (current)"}}, opts...)
}

// kindForParent is the kind to CORRECT to under a parent of kind `parent`: the shallowest kind that
// is strictly deeper, i.e. the closest legal child.
func kindForParent(parent apiv1.WorkItemKind) string {
	switch parent {
	case apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC:
		return "feature"
	case apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE:
		return "task"
	case apiv1.WorkItemKind_WORK_ITEM_KIND_TASK:
		return "subtask"
	}
	return ""
}

// parentOptionsFor is the parent picker's option list scoped to a project.
func (m *Model) parentOptionsFor(projectID string) []kit2.Option {
	opts := []kit2.Option{{Value: "", Label: "— none (top level · epic) —"}}
	for _, o := range m.parents {
		if o.Value == "" {
			continue
		}
		if m.parentProject[o.Value] == projectID {
			opts = append(opts, o)
		}
	}
	return opts
}

func hasOption(opts []kit2.Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

// validateHierarchy mirrors the server's parent rules locally
// (internal/workitem/validate.go), so an illegal combination is caught in the
// form with a clear message instead of coming back as a rejected write:
//   - a parentless item must be an EPIC (only epics may be top-level),
//   - a parent must live in the SAME project as its child,
//   - a child must be strictly DEEPER than its parent — which means an epic may
//     parent a feature, a task or a subtask; a FEATURE may parent a task or a
//     subtask; and a TASK may parent a subtask.
func (m *Model) validateHierarchy(projectID, parentID, kind string) error {
	depth := map[string]int{"epic": 1, "feature": 2, "task": 3, "subtask": 4}
	if parentID == "" {
		if kind != "epic" {
			return fmt.Errorf("only an epic can be top-level — choose a parent, or set kind to epic")
		}
		return nil
	}
	if p, ok := m.parentProject[parentID]; ok && p != projectID {
		return fmt.Errorf("the parent is in a different project — a parent must be in the same project as its child")
	}
	pk, ok := m.parentKind[parentID]
	if !ok {
		return nil // an unknown parent is the server's call
	}
	if depth[kind] <= depth[kindBadge(pk)] {
		return fmt.Errorf("a %s must be deeper than its parent (the parent is a %s) — pick a deeper kind or a shallower parent", kind, kindBadge(pk))
	}
	return nil
}

// maxParentPages bounds the parent picker's pagination (the same shape as the list's own: the
// dropdown must show EVERY item that can be a parent, and the server orders NULL sort_order LAST.
const maxParentPages = 25

// loadParentOptions fetches every work item that can be a parent and returns the picker options
// plus the id→kind and id→project maps the form needs to derive and validate the child kind.
func loadParentOptions(ctx context.Context, cl *client.Clients) ([]kit2.Option, map[string]apiv1.WorkItemKind, map[string]string) {
	opts := []kit2.Option{{Value: "", Label: "— none (top level · epic) —"}}
	kinds := map[string]apiv1.WorkItemKind{}
	projects := map[string]string{}
	resp, err := cl.WorkItems.ListWorkItems(ctx, connect.NewRequest(&apiv1.ListWorkItemsRequest{
		PageSize:        workItemPageSize,
		RecurringFilter: apiv1.RecurringFilter_RECURRING_FILTER_EXCLUDE_RECURRING,
		IdeaScope:       apiv1.IdeaScope_IDEA_SCOPE_EXCLUDE_IDEA,
	}))
	if err != nil {
		return opts, kinds, projects // a partial list still beats no dropdown
	}
	for _, w := range resp.Msg.GetWorkItems() {
		// A repeated id is skipped: the option list is keyed by id, and two options with the same
		// value would make the picker ambiguous (the operator would see one parent listed twice).
		if _, seen := kinds[w.GetId()]; seen {
			continue
		}
		opts = append(opts, kit2.Option{
			Value: w.GetId(),
			Label: "[" + kindBadge(w.GetKind()) + "] " + w.GetTitle(),
		})
		kinds[w.GetId()] = w.GetKind()
		projects[w.GetId()] = w.GetProjectId()
	}
	return opts, kinds, projects
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
	initialProject := ""
	if len(projOpts) > 0 {
		initialProject = projOpts[0].Value
	}
	f := kit2.NewForm("New work item",
		kit2.FieldSpec{Name: "title", Label: "Title", Kind: kit2.KText, Required: true, Placeholder: "add retry to the sweeper"},
		kit2.FieldSpec{Name: "project", Label: "Project", Kind: kit2.KSelect, Options: projOpts, Required: true, Initial: initialProject},
		kit2.FieldSpec{Name: "kind", Label: "Kind", Kind: kit2.KSelect, Options: kindOptions(), Initial: "epic"},
		// The parent list is scoped to the CHOSEN project: a parent must be in the same project as its
		// child, so offering another project's items would offer a guaranteed rejection.
		kit2.FieldSpec{Name: "parent", Label: "Parent", Kind: kit2.KPicker,
			Options: m.parentOptionsFor(initialProject), Placeholder: "type to search work items"},
		kit2.FieldSpec{Name: "description", Label: "Description", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "acceptance", Label: "Acceptance criteria", Kind: kit2.KTextArea},
		kit2.FieldSpec{Name: "priority", Label: "Priority", Kind: kit2.KNumber, Initial: "0", Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "budgets", Label: "Budgets", Kind: kit2.KJSON, Placeholder: `{"tokens":100000}`, Validate: validateJSON},
		kit2.FieldSpec{Name: "context_window", Label: "Context window", Kind: kit2.KNumber, Initial: "0", Validate: validateNonNegativeInt},
		kit2.FieldSpec{Name: "workflow", Label: "Workflow", Kind: kit2.KPicker, Options: wfOpts, Initial: ""},
		kit2.FieldSpec{Name: "runtime_image", Label: "Runtime image", Kind: kit2.KPicker, Options: m.images, Placeholder: "type to search images"},
		// WHERE THIS ITEM'S EXECUTIONS RUN — the per-item override of the project's mode. Three
		// choices because an item can INHERIT (the default); see itemExecutionModeOptions.
		kit2.FieldSpec{Name: "execution_mode", Label: "Execution mode", Kind: kit2.KSelect,
			Options: itemExecutionModeOptions(m.projectExecutionMode(initialProject)),
			Initial: itemExecutionModeInherit},
		kit2.FieldSpec{Name: "context_files", Label: "Context files", Kind: kit2.KText, Placeholder: "/abs/path/a.go,/abs/dir"},
		kit2.FieldSpec{Name: "auto_start", Label: "Auto-start workflow", Kind: kit2.KCheckbox, Initial: "false"},
	)
	// Derived-field rules keep the form on legal ground:
	//
	//  1. Changing the PROJECT re-scopes the parent picker to that project and
	//     drops a parent that no longer belongs (the server requires the same
	//     project).
	//  2. Choosing a PARENT corrects the kind ONLY when the pairing is illegal.
	//     Any strictly deeper kind is legal, so a deliberate choice survives —
	//     an epic may parent a feature, a task or a subtask; a FEATURE may
	//     parent a task or a subtask; a TASK may parent a subtask.
	//
	// THERE IS NO AUTO-START COUPLING, deliberately. Emptying the workflow must NOT clear
	// auto-start, because a workflow-less item is exactly how a sequential workflow is kicked
	// off: the parent carries no binding of its own and its children run in chain order. The
	// operator: "Workflows need to be empty for a sequential workflow to kick off. We just need
	// to remove that validation."
	//
	// Auto-start with no workflow is legal at every status the operator can save (the server
	// only gates on ENTERING ready/assigned/scheduled/running, and it exempts a sequence parent
	// entirely), so the client must not refuse it, warn about it, or clear it.
	f.OnChange = func(name, value string) {
		switch name {
		case "project":
			opts := m.parentOptionsFor(value)
			for i := range f.Specs {
				if f.Specs[i].Name == "parent" {
					f.Specs[i].Options = opts
				}
				// The inherit option names the mode it inherits FROM, so it is re-read when the project changes —
				// otherwise the form would state one project's mode while the request lands the item in another.
				if f.Specs[i].Name == "execution_mode" {
					f.Specs[i].Options = itemExecutionModeOptions(m.projectExecutionMode(value))
				}
			}
			if cur := f.Values["parent"]; cur != "" && !hasOption(opts, cur) {
				// Direct assignment (not Set) so this correction cannot recurse.
				f.Values["parent"] = ""
				f.Values["kind"] = "epic"
			}
		case "parent":
			cur := f.Values["kind"]
			if m.validateHierarchy(f.Values["project"], value, cur) == nil {
				return // already legal for this parent
			}
			if k := kindForParent(m.parentKind[value]); k != "" {
				f.Values["kind"] = k
			}
		}
	}
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
		// Initial: is the ONLY thing that populates the form's value map (kit2.NewForm writes
		// f.Values[name] only when Initial != "").
		kit2.FieldSpec{Name: "workflow", Label: "Workflow", Kind: kit2.KPicker,
			Options: pickerOptsWithCurrent(m.workflowPickerOpts(), w.GetWorkflowId(), "workflow "),
			Initial: w.GetWorkflowId()},
		kit2.FieldSpec{Name: "runtime_image", Label: "Runtime image", Kind: kit2.KPicker,
			Options: pickerOptsWithCurrent(m.images, w.GetRuntimeImage(), "image "),
			Initial: w.GetRuntimeImage()},
		// THE EXECUTION-MODE OVERRIDE, SEEDED FROM THE ITEM — the same rule as the two reference pickers
		// above, and for the same reason: Initial: is the only thing that populates the value map.
		kit2.FieldSpec{Name: "execution_mode", Label: "Execution mode", Kind: kit2.KSelect,
			Options: itemExecutionModeOptions(m.projectExecutionMode(w.GetProjectId())),
			Initial: itemExecutionModeField(w)},
		kit2.FieldSpec{Name: "context_files", Label: "Context files", Kind: kit2.KText, Initial: strings.Join(w.GetContextFiles(), ",")},
		// A DAY AND A TIME, chosen from ONE modal calendar + clock — never typed, never a preset.
		kit2.FieldSpec{Name: "scheduled_start", Label: "Scheduled start", Kind: kit2.KDateTime,
			// SEEDED from the item's existing value, or editing a scheduled item would open on a BLANK field
			// and saving would clear the schedule the operator never touched.
			Initial:  rfc3339OrEmpty(w.GetScheduledStartAt()),
			Display:  displayScheduledStart,
			Validate: validateOptionalRFC3339},
		// THE BOX OPENS UNCHECKED, ALWAYS — never seeded from the stored flag.
		kit2.FieldSpec{Name: "auto_start", Label: "Auto-start workflow", Kind: kit2.KCheckbox, Initial: "false"},
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

// displayScheduledStart renders the STORED RFC3339 instant for the operator: in local time,
// labelled with the zone name AND its UTC offset.
func displayScheduledStart(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "— not scheduled —"
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		// A value that cannot be parsed is shown RAW rather than prettified: it is what would be sent (or
		// what the validator will reject).
		return v
	}
	return screenkit.FmtLocalFull(t)
}

// validateOptionalRFC3339 accepts an empty value (not scheduled) or a valid
// RFC3339 timestamp — the validator for a field the operator can also TYPE into.
func validateOptionalRFC3339(v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return validateRFC3339(v)
}

// wireItemForm installs the submit handler: it builds the RPC request from the collected values and
// hands it to the mutation executor.
func (m *Model) wireItemForm(f *kit2.Form, mode, id string) {
	f.Focused = true
	f.Width = 70
	// The combined calendar + clock, for the scheduled-start field.
	f.OnOpenDateTimePicker = m.openDateTimePicker
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		title := strings.TrimSpace(v["title"])
		switch mode {
		case formCreateItem:
			project := v["project"]
			if project == "" {
				return nil, fmt.Errorf("a work item belongs to a project")
			}
			kind := kindFromName(v["kind"])
			parentID := strings.TrimSpace(v["parent"])
			if err := m.validateHierarchy(project, parentID, kindBadge(kind)); err != nil {
				return nil, err
			}
			req := &apiv1.CreateWorkItemRequest{
				ProjectId:          project,
				ParentId:           parentID,
				Kind:               kind,
				Title:              title,
				Description:        v["description"],
				AcceptanceCriteria: v["acceptance"],
				Priority:           int32(atoiOr(v["priority"], 0)),
				Budgets:            strings.TrimSpace(v["budgets"]),
				ContextWindow:      int32(atoiOr(v["context_window"], 0)),
				WorkflowId:         strings.TrimSpace(v["workflow"]),
				RuntimeImage:       strings.TrimSpace(v["runtime_image"]),
				// The create field is NON-optional and UNSPECIFIED *is* inherit (work_item_service.proto:186-190),
				// so "inherit" needs no separate spelling here.
				ExecutionMode:     executionModeEnum(v["execution_mode"]),
				ContextFiles:      splitList(v["context_files"]),
				AutoStartWorkflow: boolPtr(v["auto_start"] == "true"),
			}
			name := "create work item " + strconv.Quote(req.GetTitle())
			return m.Mutate(mutate.Request{
				Name: name, Source: srcWorkItems,
				Rollback: func() { m.Refresh(srcWorkItems) },
				Do: func(ctx context.Context) error {
					resp, err := m.cl.WorkItems.CreateWorkItem(ctx, connect.NewRequest(req))
					if err != nil {
						return err
					}
					// Focus the new item as soon as the reload lands, so it is on screen and selected instead of
					// appearing somewhere below the fold while the cursor stays on the previously selected row.
					m.Base.SelectWhenLoaded(srcWorkItems, resp.Msg.GetWorkItem().GetId())
					return nil
				},
			}), nil

		case formEditItem:
			auto := v["auto_start"] == "true"
			prio := int32(atoiOr(v["priority"], 0))
			cw := int32(atoiOr(v["context_window"], 0))
			kind := kindFromName(v["kind"])
			status := statusFromName(v["status"])
			// THE SCHEDULE IS SENT AS EITHER A VALUE OR AN EXPLICIT CLEAR, never silently dropped.
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
			if req.ScheduledStartAt == nil {
				req.ClearScheduledStartAt = true
			}
			// THE EXECUTION-MODE OVERRIDE IS SENT AS EITHER A VALUE OR AN EXPLICIT CLEAR — both halves, exactly
			// as the GUI sends them (frontend/src/routes/work-items_.$id.tsx:388-389).
			if v["execution_mode"] == itemExecutionModeInherit {
				req.ClearExecutionMode = true
			} else {
				mode := executionModeEnum(v["execution_mode"])
				req.ExecutionMode = &mode
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
		// A GHOST ANCHOR IS NOT ARCHIVED.
		if strings.HasPrefix(it.Meta, archiveGhostPrefix+" ancestor") {
			return nil
		}
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
	// A RUNNING ITEM CAN BE STOPPED HERE, WITHOUT finding its execution first.
	return []kit2.Action{
		{
			Label: "stop execution", Key: keyStopRun, Danger: true, Source: srcWorkItems,
			Confirm: "Stop the running EXECUTION for " + title + "?\n" +
				"The worker stops and in-flight tool calls are abandoned (the reason is recorded as " +
				strconv.Quote(cancelReason) + ").\n" +
				"This does NOT end the workflow run — to abort the run itself, use the Runs pane (" + "S" + ").",
			Do: func(ctx context.Context) error { return m.rpcStopRun(ctx, id) },
		},
		{
			Label: "toggle auto-start", Key: "y", Source: srcWorkItems,
			Do: func(ctx context.Context) error { return m.rpcToggleAutoStart(ctx, id) },
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
			// THE GUI'S DELETE, WHICH IS PERMANENT.
			Label: "delete", Key: kit2.DeleteChord, Danger: true, Source: srcWorkItems,
			Confirm: "Permanently delete " + title + "?\n" +
				"This removes it and its dependencies for good — it cannot be undone.\n" +
				"An item with children is refused (delete the children first); to MOVE it out of\n" +
				"the way reversibly instead, set its status to cancelled (s or e).",
			Apply:    func() { m.RemoveRow(srcWorkItems, id) },
			Rollback: func() { m.Refresh(srcWorkItems) },
			Do: func(ctx context.Context) error {
				_, err := m.cl.WorkItems.HardDeleteWorkItem(ctx, connect.NewRequest(&apiv1.HardDeleteWorkItemRequest{Id: id}))
				return err
			},
		},
	}
}

// keyStopRun is the pane's stop-execution chord.
const keyStopRun = "x"

// cancelReason is the audit reason recorded on a TUI-initiated stop, matching the
// Executions pane's wording so one trail describes both entry points.
const cancelReason = "cancelled from the orch TUI"

// rpcStopRun cancels the item's LIVE execution.
func (m *Model) rpcStopRun(ctx context.Context, id string) error {
	if m.cl == nil || m.cl.Executions == nil {
		return fmt.Errorf("no execution client — cannot stop the run")
	}
	resp, err := m.cl.Executions.ListExecutions(ctx, connect.NewRequest(&apiv1.ListExecutionsRequest{
		TaskId: &id,
	}))
	if err != nil {
		return err
	}
	var live *apiv1.WorkerExecution
	for _, e := range resp.Msg.GetExecutions() {
		if e.GetStatus() == apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING {
			if live == nil || e.GetStartedAt().AsTime().After(live.GetStartedAt().AsTime()) {
				live = e
			}
		}
	}
	if live == nil {
		return fmt.Errorf("no running execution for this item — nothing to stop")
	}
	_, err = m.cl.Executions.CancelExecution(ctx, connect.NewRequest(&apiv1.CancelExecutionRequest{
		Id:     live.GetId(),
		Reason: cancelReason,
	}))
	return err
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

// reorderChildren moves the selected item one step within its SIBLING sequence and persists the new
// order with ReorderWorkItems — the only RPC that writes sort_order.
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
			// The swap is computed from the STORED sequence, never the display sort: reordering under a display
			// view would send the wrong sibling list and silently scramble the real order.
			sortSiblings(order, sortSequence)
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
	// LOCAL, with the offset — not a forced "...Z".
	return ts.AsTime().In(time.Local).Format(time.RFC3339)
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
