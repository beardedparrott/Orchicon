package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func editPlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Original", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, ProjectId: "proj-1",
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m
}

// The operator: "When editing a work item, we should be able to edit it in the
// details pane as opposed some small work item edit modal that pops up. That is
// more natural and honestly much more usable."
//
// So 'e' must NOT open a modal: no floating form, and the DETAIL pane carries
// the editor.
func TestEditOpensInTheDetailsPaneNotAModal(t *testing.T) {
	m := editPlane(t)
	run(t, m, press(t, m, "e"))

	if m.form != nil {
		t.Fatal("'e' must not open a modal form")
	}
	if !m.Base.EditingDetail() {
		t.Fatal("'e' must open the editor in the detail pane")
	}
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("the detail editor must be reachable through ActiveForm")
	}
	if f.Values["title"] != "Original" {
		t.Fatalf("the editor must be prefilled from the item, got %q", f.Values["title"])
	}
	// The rendered frame shows the editor inside the pane, with a save hint.
	v := m.View()
	for _, want := range []string{"Edit work item", "Original", "ctrl+s"} {
		if !strings.Contains(v, want) {
			t.Errorf("the detail editor must render %q", want)
		}
	}
	// Keys are claimed while editing, so nothing leaks to the shell/composer.
	if !m.ClaimsKeys() {
		t.Fatal("the detail editor must claim every key")
	}
}

// Editing in the pane really edits: typing lands in the focused field at the
// caret, and ctrl+s submits.
func TestDetailEditTypesAndSaves(t *testing.T) {
	m := editPlane(t)
	run(t, m, press(t, m, "e"))
	f := m.ActiveForm()
	if f == nil || !f.FocusName("title") {
		t.Fatal("no title field in the editor")
	}
	// Clear the prefilled value and type a new title.
	f.Set("title", "")
	for _, ch := range "Renamed" {
		press(t, m, string(ch))
	}
	if got := f.Values["title"]; got != "Renamed" {
		t.Fatalf("typed title = %q, want Renamed", got)
	}
	// ctrl+s submits through the form's validation.
	press(t, m, "ctrl+s")
	if m.Base.EditingDetail() {
		t.Fatal("ctrl+s must close the editor")
	}
	if m.form != nil {
		t.Fatal("ctrl+s must not leave a modal behind")
	}
}

// esc abandons the edit and returns to the ordinary detail view.
func TestDetailEditEscCancels(t *testing.T) {
	m := editPlane(t)
	run(t, m, press(t, m, "e"))
	if !m.Base.EditingDetail() {
		t.Fatal("editor must be open")
	}
	press(t, m, "esc")
	if m.Base.EditingDetail() {
		t.Fatal("esc must close the editor")
	}
	if m.form != nil {
		t.Fatal("esc must not leave a modal behind")
	}
	// The pane is back to the read-only detail, not blank.
	if v := m.View(); !strings.Contains(v, "Detail") {
		t.Fatalf("the pane must return to the detail view:\n%s", v)
	}
}

// The other work-item edit gestures use the same host, so there is ONE
// editing surface rather than a mix of modals. Assign is deliberately GONE:
// a worker ref does not belong on a work item (workflows bind the work).
func TestStatusAndScheduleEditInThePane(t *testing.T) {
	for _, key := range []string{"s", "t"} {
		m := editPlane(t)
		run(t, m, press(t, m, key))
		if m.form != nil {
			t.Fatalf("%q opened a modal; every edit must use the detail pane", key)
		}
		if !m.Base.EditingDetail() {
			t.Fatalf("%q must open the detail-pane editor", key)
		}
		press(t, m, "esc")
	}
}

// Assigning a worker ref directly to a work item is REMOVED: the operator's
// "assign makes no sense on work items and is dangerous. That is old left over
// from earlier versions ... Workflows handle this." 'w'/'W' must be inert and
// no assign action may be offered.
func TestAssignIsRemovedFromWorkItems(t *testing.T) {
	m := editPlane(t)
	for _, key := range []string{"w", "W"} {
		press(t, m, key)
	}
	if m.form != nil || m.Base.EditingDetail() {
		t.Fatal("w/W must not open any editor")
	}
	for _, a := range m.itemActions() {
		if strings.Contains(strings.ToLower(a.Label), "assign") {
			t.Fatalf("the assign action must be gone, still offered: %q", a.Label)
		}
	}
	if v := strings.ToLower(m.HintLine()); strings.Contains(v, "assign") {
		t.Fatalf("the hint must not advertise assign:\n%s", m.HintLine())
	}
}
