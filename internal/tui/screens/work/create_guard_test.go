package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The operator: "When I created a new work item (twice) and chose a parent, it
// is not showing up under the parent. It is nowhere to be found."
//
// Root cause (source truth, internal/workitem/validate.go): only an EPIC may be
// top-level, and a child must be strictly DEEPER than its parent. The form
// defaulted kind to "task" with no parent, and offered every kind against every
// parent — so the server rejected the write. These tests pin the fix: the form
// cannot offer a pairing the server rejects, and a rejection is LOUD.
func TestCreateFormKindFollowsParent(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})
	p.addItem(&apiv1.WorkItem{Id: "wi-task", Title: "Task", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1"})

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}

	// With no parent the item must be an epic (the only legal top-level kind).
	if got := f.Values["kind"]; got != "epic" {
		t.Fatalf("a parentless create must default to epic, got %q", got)
	}

	// Choosing a TASK parent while the kind is epic is illegal (an epic is not
	// deeper than a task) — the form corrects it to the only legal child.
	f.Set("parent", "wi-task")
	if got := f.Values["kind"]; got != "subtask" {
		t.Fatalf("kind under a task parent = %q, want subtask", got)
	}

	// Choosing an EPIC parent with a legal kind PRESERVES the choice: task is
	// deeper than epic, so it is allowed and must not be second-guessed.
	f.Set("kind", "task")
	f.Set("parent", "wi-epic")
	if got := f.Values["kind"]; got != "task" {
		t.Fatalf("a legal kind must be preserved, got %q", got)
	}

	// And an illegal pairing is refused locally, WITH A MESSAGE, before any RPC.
	f.Set("kind", "epic")
	f.Set("title", "Bad")
	if err := m.validateHierarchy("proj-1", "wi-epic", "epic"); err == nil {
		t.Fatal("an epic under an epic parent must be rejected")
	}
	if err := m.validateHierarchy("proj-1", "", "task"); err == nil {
		t.Fatal("a parentless task must be rejected")
	}

	// FEATURES AND TASKS ARE LEGAL PARENTS — the rule is "strictly deeper", not
	// "only epics may have children". These must NOT be rejected.
	legal := []struct{ parent, kind string }{
		{"wi-epic", "feature"}, {"wi-epic", "task"}, {"wi-epic", "subtask"},
		{"wi-task", "subtask"},
	}
	for _, c := range legal {
		if err := m.validateHierarchy("proj-1", c.parent, c.kind); err != nil {
			t.Errorf("%s under %s must be legal, got %v", c.kind, c.parent, err)
		}
	}
}

// The work screen must install a mutation executor with a Sink. Without one,
// Base.Mutate fell back to a zero executor whose Fail/Notice were no-ops — so a
// REJECTED write rolled back in silence and the operator only saw an item that
// never appeared. This is the visibility half of the same bug.
func TestRejectedWriteIsLoud(t *testing.T) {
	m := editPlane(t)
	if m.Base.Exec == nil {
		t.Fatal("the work screen must install an executor (otherwise failures are silent)")
	}
	sink := workSink{m}
	sink.Fail("create work item: a task must have a parent; only epics can be top-level")
	if !strings.Contains(m.notice, "must have a parent") {
		t.Fatalf("a failed write must be visible in the notice, got %q", m.notice)
	}
	if !strings.HasPrefix(m.notice, "✗") {
		t.Fatalf("a failure must be marked as a failure, got %q", m.notice)
	}
}

// ctrl+s saves a form; enter never does (it is the choose gesture on the
// pickers and selectors, so overloading it made selecting commit the form).
func TestCtrlSSavesAndEnterDoesNot(t *testing.T) {
	m := editPlane(t)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil || !f.FocusName("title") {
		t.Fatal("no title field")
	}
	// Enter must not submit: the editor stays open.
	press(t, m, "enter")
	if !m.Base.EditingDetail() {
		t.Fatal("enter must not save the form")
	}
	if f.Submitted {
		t.Fatal("enter must not mark the form submitted")
	}
	// ctrl+s saves.
	press(t, m, "ctrl+s")
	if m.Base.EditingDetail() {
		t.Fatal("ctrl+s must save and close the editor")
	}
}

// Scheduling lives in the DETAILS pane: the Scheduled-start field IS a picker
// over ready-made times, and auto-start is OFF by default (the operator called
// the previous default "dangerous").
func TestSchedulePickerLivesInTheDetailsEditor(t *testing.T) {
	m := editPlane(t)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("e must open the details editor")
	}
	// Auto-start must not be pre-enabled.
	if got := f.Values["auto_start"]; got == "true" {
		t.Fatal("auto-start must not default to enabled")
	}
	// The scheduled-start field is a PICKER carrying ready-made times.
	var spec *kit2.FieldSpec
	for i := range f.Specs {
		if f.Specs[i].Name == "scheduled_start" {
			spec = &f.Specs[i]
		}
	}
	if spec == nil {
		t.Fatal("the editor must carry a scheduled-start field")
	}
	if spec.Kind != kit2.KPicker {
		t.Fatalf("scheduled start must be a picker, got %q", spec.Kind)
	}
	if len(spec.Options) < 3 {
		t.Fatalf("expected ready-made times, got %d", len(spec.Options))
	}
	// Choosing a preset sets the timestamp the request will carry.
	var preset string
	for _, o := range spec.Options {
		if o.Value != "" {
			preset = o.Value
			break
		}
	}
	f.Set("scheduled_start", preset)
	if got := f.Values["scheduled_start"]; got != preset {
		t.Fatalf("choosing a preset must set the timestamp: got %q, want %q", got, preset)
	}
}
