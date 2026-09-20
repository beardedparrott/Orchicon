package work

// bulk_set_test.go — the BULK workflow/runtime-image set.
//
// The bulk counterpart of the single-item form, and the remedy for "an item with no workflow
// binding cannot run": it strands a whole backlog that is bound to nothing, and fixing that one
// modal at a time is the tedium this removes.
//
// The load-bearing property, proved rather than assumed, is INDEPENDENCE (AC2): an untouched field
// is left as a NIL POINTER, so it is omitted from the request entirely. `WorkflowId` and
// `RuntimeImage` are both `optional` on UpdateWorkItemRequest — unset means unchanged, empty means
// clear. A zero value for an untouched field would silently unbind it.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// bulkSetModel puts three work items on the Work Items pane, with a real plane behind the screen
// (so the writes are the calls the screen really makes).
func bulkSetModel(t *testing.T, p *fakePlane) *Model {
	t.Helper()
	m := newModel(t, p)
	if !m.Base.SelectSource(srcWorkItems) {
		t.Fatal("fixture: could not focus the Work Items pane")
	}
	if !m.Base.LoadItems(srcWorkItems, []kit2.Item{
		{ID: "w1", Title: "First", Meta: "pending"},
		{ID: "w2", Title: "Second", Meta: "pending"},
		{ID: "w3", Title: "Third", Meta: "pending"},
	}, "") {
		t.Fatal("fixture: could not load the rows")
	}
	m.refreshActionBar()
	return m
}

// seedItems puts the same three ids in the PLANE, so an UpdateWorkItem against one actually lands.
func seedItems(p *fakePlane, ids ...string) {
	for _, id := range ids {
		p.addItem(&apiv1.WorkItem{Id: id, Title: id, ProjectId: "proj-1",
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	}
}

// openBulkSet marks n rows and opens the bulk picker through the real entry point (`W`), feeding
// the option-list message back in. The returned form is the open modal.
func openBulkSet(t *testing.T, m *Model, n int) *kit2.Form {
	t.Helper()
	mark(t, m, n)
	cmd := press(t, m, keyBulkSet)
	if cmd == nil {
		t.Fatal("pressing W produced no command — the picker never opened")
	}
	run(t, m, cmd)
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("no form open after the option-list message landed")
	}
	return f
}

// pendingAction returns the action the open confirmation dialog will run.
func pendingAction(t *testing.T, m *Model) kit2.Action {
	t.Helper()
	if m.pending == nil {
		t.Fatal("no pending action — the confirm was never raised")
	}
	return *m.pending
}

// 1. THE ENTRY POINT. The bulk set appears exactly ONCE above the shared bulk threshold (>1 marked),
// matching the archive/delete rule; one marked row is the cursor's row, not a selection.
func TestBulkSetActionPresentAboveThresholdOnly(t *testing.T) {
	m := bulkSetModel(t, newPlane())
	ids := []string{"w1", "w2", "w3"}

	acts := m.bulkItemActions(ids)
	if len(acts) != 4 {
		t.Fatalf("bulk actions = %d, want 4 (archive, delete, set workflow & image, clear)", len(acts))
	}
	found := 0
	for _, a := range acts {
		if a.Key == keyBulkSet {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("bulk set appears %d times, want exactly 1 (key %q)", found, keyBulkSet)
	}

	// ONE marked row: the bulk set must NOT be offered — the single-row actions are.
	mark(t, m, 1)
	for _, a := range m.actionsForSelection() {
		if a.Key == keyBulkSet {
			t.Fatal("the bulk set is offered with only ONE row marked — one row is the cursor's row, not a selection")
		}
	}

	// TWO marked rows: it is.
	mark(t, m, 1)
	offered := false
	for _, a := range m.actionsForSelection() {
		if a.Key == keyBulkSet {
			offered = true
		}
	}
	if !offered {
		t.Fatal("the bulk set is not offered with two rows marked")
	}
}

// 2. INDEPENDENCE, direction one: a WORKFLOW-ONLY set leaves every item's runtime_image untouched.
func TestBulkSetWorkflowOnlyLeavesRuntimeImageUnset(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1", "w2", "w3")
	m := bulkSetModel(t, p)
	f := openBulkSet(t, m, 3)

	f.Set("workflow", "wf-1")
	f.Set("runtime_image", bulkSetSkip)
	// Submit through the form's OWN handler (not the ctrl+s key, which a focused KPicker
	// swallows as a query) — this is the real submit path, OnSubmit included.
	if _, err := f.Submit(); err != nil {
		t.Fatalf("submit refused: %v", err)
	}

	a := pendingAction(t, m)
	if err := a.Do(context.Background()); err != nil {
		t.Fatalf("workflow-only set failed: %v", err)
	}
	if len(p.updated) != 3 {
		t.Fatalf("wrote %d items, want 3", len(p.updated))
	}
	for _, req := range p.updated {
		if req.WorkflowId == nil || req.GetWorkflowId() != "wf-1" {
			t.Errorf("%s: WorkflowId = %v, want \"wf-1\"", req.GetId(), req.WorkflowId)
		}
		// THE INDEPENDENCE PROOF: nil, not "" — an unset field is OMITTED from the request, which
		// is what leaves the item's runtime image alone.
		if req.RuntimeImage != nil {
			t.Errorf("%s: RuntimeImage = %q, want it UNSET (nil) — a workflow-only set must not "+
				"touch the runtime image", req.GetId(), req.GetRuntimeImage())
		}
	}
}

// 3. INDEPENDENCE, direction two: an IMAGE-ONLY set leaves every item's workflow untouched.
func TestBulkSetImageOnlyLeavesWorkflowUnset(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1", "w2")
	m := bulkSetModel(t, p)
	f := openBulkSet(t, m, 2)

	f.Set("runtime_image", "orchicon/go:1.2")
	f.Set("workflow", bulkSetSkip)
	// Submit through the form's OWN handler (not the ctrl+s key, which a focused KPicker
	// swallows as a query) — this is the real submit path, OnSubmit included.
	if _, err := f.Submit(); err != nil {
		t.Fatalf("submit refused: %v", err)
	}

	a := pendingAction(t, m)
	if err := a.Do(context.Background()); err != nil {
		t.Fatalf("image-only set failed: %v", err)
	}
	if len(p.updated) != 2 {
		t.Fatalf("wrote %d items, want 2", len(p.updated))
	}
	for _, req := range p.updated {
		if req.RuntimeImage == nil || req.GetRuntimeImage() != "orchicon/go:1.2" {
			t.Errorf("%s: RuntimeImage = %v, want \"orchicon/go:1.2\"", req.GetId(), req.RuntimeImage)
		}
		if req.WorkflowId != nil {
			t.Errorf("%s: WorkflowId = %q, want it UNSET (nil) — an image-only set must not "+
				"re-bind the workflow", req.GetId(), req.GetWorkflowId())
		}
	}
}

// 4. CLEARING works for BOTH fields from the same control, using the documented `empty` semantics
// (empty workflow_id = unbind; empty runtime_image = base image).
func TestBulkSetClearSendsEmpty(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1", "w2")
	m := bulkSetModel(t, p)
	f := openBulkSet(t, m, 2)

	f.Set("workflow", bulkSetClear)
	f.Set("runtime_image", bulkSetClear)
	// Submit through the form's OWN handler (not the ctrl+s key, which a focused KPicker
	// swallows as a query) — this is the real submit path, OnSubmit included.
	if _, err := f.Submit(); err != nil {
		t.Fatalf("submit refused: %v", err)
	}

	a := pendingAction(t, m)
	if err := a.Do(context.Background()); err != nil {
		t.Fatalf("clear failed: %v", err)
	}
	if len(p.updated) != 2 {
		t.Fatalf("wrote %d items, want 2", len(p.updated))
	}
	for _, req := range p.updated {
		if req.WorkflowId == nil || req.GetWorkflowId() != "" {
			t.Errorf("%s: WorkflowId = %v, want non-nil empty (the documented unbind)", req.GetId(), req.WorkflowId)
		}
		if req.RuntimeImage == nil || req.GetRuntimeImage() != "" {
			t.Errorf("%s: RuntimeImage = %v, want non-nil empty (base image)", req.GetId(), req.RuntimeImage)
		}
	}
}

// 5. A set that sets NOTHING is refused with a reason and writes nothing.
func TestBulkSetBothSkippedRefused(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1", "w2")
	m := bulkSetModel(t, p)
	f := openBulkSet(t, m, 2)

	// Both pickers default to bulkSetSkip.
	if _, err := f.Submit(); err == nil {
		t.Fatal("a set that changes nothing was submitted")
	}

	if m.pending != nil {
		t.Fatal("a confirm was raised for a set that changes nothing")
	}
	if len(p.updated) != 0 {
		t.Fatalf("%d writes landed for a set that changes nothing", len(p.updated))
	}
	if f.SubmitErr == "" {
		t.Fatal("the submit was refused silently — the operator must be told why")
	}
}

// 6. PARTIAL FAILURE IS REPORTED with a count (AC5). Every id is attempted; a rejection is counted,
// never hidden behind a clean-sweep claim.
func TestBulkSetPartialFailureIsCounted(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1") // w2 is NOT in the plane: its UpdateWorkItem is rejected (not found).
	m := bulkSetModel(t, p)

	a := m.bulkSetAction([]string{"w1", "w2"}, "wf-1", bulkSetSkip, "Fanout")
	err := a.Do(context.Background())
	if err == nil {
		t.Fatal("a run that failed on one item reported success")
	}
	if !strings.Contains(err.Error(), "set 1 of 2 — 1 rejected") {
		t.Fatalf("error = %q, want it to count the failures (\"set 1 of 2 — 1 rejected\")", err.Error())
	}
}

// 7. THE CONFIRM NAMES THE COUNT AND THE VALUES, and calls out that a sequence parent's own binding
// is INERT.
func TestBulkSetConfirmNamesCountValuesAndInertParent(t *testing.T) {
	p := newPlane()
	seedItems(p, "w1", "w2", "w3")
	m := bulkSetModel(t, p)
	m.parentIDs = map[string]bool{"w1": true}

	a := m.bulkSetAction([]string{"w1", "w2", "w3"}, "wf-1", "orchicon/go:1.2", "Fanout")
	for _, want := range []string{"3 items", "Fanout", "orchicon/go:1.2"} {
		if !strings.Contains(a.Confirm, want) {
			t.Errorf("confirm %q does not name %q", a.Confirm, want)
		}
	}
	if !strings.Contains(a.Confirm, "SEQUENCE PARENT") || !strings.Contains(a.Confirm, "INERT") {
		t.Errorf("confirm %q does not call out that a sequence parent's own binding is inert", a.Confirm)
	}

	// A clearing confirm names the clearing, not a value.
	clear := m.bulkSetAction([]string{"w1"}, bulkSetClear, bulkSetClear, "")
	for _, want := range []string{"1 item", "cleared", "base image"} {
		if !strings.Contains(clear.Confirm, want) {
			t.Errorf("clear confirm %q does not name %q", clear.Confirm, want)
		}
	}
}

// 8. THE PICKER MUST NOT OFFER A TARGET THAT CANNOT RUN (AC6). UpdateWorkItem performs NO bind-time
// validation of workflow_id (the enforcement point is schedule-time only), so a bulk bind of a
// non-runnable template would land silently and fail later. The picker therefore applies the
// schedule-time predicate itself: PUBLISHED|DEPRECATED AND at least one step.
func TestBulkSetPickerHidesNonRunnableWorkflows(t *testing.T) {
	p := newPlane()
	p.workflows = []*apiv1.Workflow{
		{Id: "wf-draft", Name: "Draft", Status: apiv1.WorkflowStatus_WORKFLOW_STATUS_DRAFT},
		{Id: "wf-empty", Name: "Stepless", Status: apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED},
		{Id: "wf-pub", Name: "Ready", Status: apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED},
		{Id: "wf-dep", Name: "Deprecated", Status: apiv1.WorkflowStatus_WORKFLOW_STATUS_DEPRECATED},
	}
	p.workflowVersions = map[string]*apiv1.WorkflowVersion{
		"wf-draft": {Id: "v1", WorkflowId: "wf-draft", Steps: `[{"id":"s1"}]`},
		"wf-empty": {Id: "v1", WorkflowId: "wf-empty", Steps: `[]`},
		"wf-pub":   {Id: "v1", WorkflowId: "wf-pub", Steps: `[{"id":"s1"}]`},
		"wf-dep":   {Id: "v1", WorkflowId: "wf-dep", Steps: `[{"id":"s1"}]`},
	}
	m := bulkSetModel(t, p)
	f := openBulkSet(t, m, 2)

	opts := f.Spec("workflow").Options
	var values []string
	for _, o := range opts {
		values = append(values, o.Value)
	}
	for _, hidden := range []string{"wf-draft", "wf-empty"} {
		if containsStr(values, hidden) {
			t.Errorf("the picker offers %q, which cannot run (draft, or no steps)", hidden)
		}
	}
	for _, ok := range []string{"wf-pub", "wf-dep", bulkSetSkip, bulkSetClear} {
		if !containsStr(values, ok) {
			t.Errorf("the picker is missing %q", ok)
		}
	}
	// The hidden count is reported rather than silently swallowed.
	if !strings.Contains(m.notice, "2 workflow(s) hidden") {
		t.Errorf("notice = %q, want it to report the 2 hidden workflows", m.notice)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// 9. EXISTING BULK BEHAVIOUR IS UNMODIFIED (AC9): archive and delete are still there, with the same
// keys, and the cleared/chord dispatch still routes the selection to the bulk list.
func TestBulkSetDoesNotDisturbArchiveAndDelete(t *testing.T) {
	m := bulkSetModel(t, newPlane())
	acts := m.bulkItemActions([]string{"w1", "w2"})
	keys := map[string]string{}
	for _, a := range acts {
		keys[a.Key] = a.Label
	}
	if _, ok := keys["a"]; !ok {
		t.Error("bulk archive lost its `a` binding")
	}
	if _, ok := keys[kit2.DeleteChord]; !ok {
		t.Error("bulk delete lost its delete chord")
	}
	if _, ok := keys["esc"]; !ok {
		t.Error("clear selection lost its `esc` binding")
	}
	// Every bulk action still performs a write (the set's own write is reached through openAction).
	for _, a := range acts {
		if a.Do == nil {
			t.Errorf("%q has no Do", a.Label)
		}
	}
}

// 10. A CLIENT WITHOUT THE IMAGE SERVICE MUST NOT BE DEREFERENCED. prepBulkSet's own guard promises
// this path "never PANICs on a missing client", and the image fetch was the one dereference it did
// not cover — so a screen wired without `Images` would take the whole TUI down the moment the
// operator pressed W. The picker must still open, offering the workflow it CAN set.
func TestBulkSetPickerSurvivesAMissingImageClient(t *testing.T) {
	p := newPlane()
	p.workflows = []*apiv1.Workflow{{
		Id: "wf-1", Name: "Fanout", Status: apiv1.WorkflowStatus_WORKFLOW_STATUS_PUBLISHED,
	}}
	p.workflowVersions = map[string]*apiv1.WorkflowVersion{
		"wf-1": {Id: "v1", WorkflowId: "wf-1", Steps: `[{"id":"s1"}]`},
	}
	m := bulkSetModel(t, p)
	m.cl.Images = nil

	f := openBulkSet(t, m, 2) // panics here without the guard

	if got := optionValuesOf(f.Spec("workflow").Options); !containsStr(got, "wf-1") {
		t.Errorf("workflow options = %v, want the runnable wf-1 offered", got)
	}
	// The image picker still carries its two sentinels: a workflow-only set is still possible.
	got := optionValuesOf(f.Spec("runtime_image").Options)
	for _, want := range []string{bulkSetSkip, bulkSetClear} {
		if !containsStr(got, want) {
			t.Errorf("image options = %v, want the %q sentinel", got, want)
		}
	}
}

func optionValuesOf(opts []kit2.Option) []string {
	values := make([]string, 0, len(opts))
	for _, o := range opts {
		values = append(values, o.Value)
	}
	return values
}
