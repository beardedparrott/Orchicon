package work

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// The operator's report: "When I am in the new work item modal, the arrow keys
// still control the work item list versus moving through the New Work Item
// fields." This pins BOTH halves of that: Up/Down must move the FORM's field
// focus, and the work-item list behind the modal must not move at all.
func TestCreateFormArrowsWalkFieldsNotTheList(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	for _, id := range []string{"wi-1", "wi-2", "wi-3"} {
		p.addItem(&apiv1.WorkItem{
			Id:        id,
			Title:     "Item " + id,
			Kind:      apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
			Status:    apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
			ProjectId: "proj-1",
		})
	}
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	before, ok := m.ActiveItem()
	if !ok {
		t.Fatal("no list selection to start from")
	}

	// 'n' loads the option lists, then opens the create form.
	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	if !m.ClaimsKeys() {
		t.Fatal("an open form must claim the keys")
	}

	// Down walks forward to the next field (was: nothing happened).
	if got := f.CurrentName(); got != "title" {
		t.Fatalf("form opens on %q, want title", got)
	}
	press(t, m, "down")
	if got := f.CurrentName(); got == "title" {
		t.Fatalf("down did not move off the first field (still %q)", got)
	}
	next := f.CurrentName()

	// Up walks back again.
	press(t, m, "up")
	if got := f.CurrentName(); got != "title" {
		t.Fatalf("up did not return to the first field: %q (was %q)", got, next)
	}

	// And the list behind the modal never moved.
	after, ok := m.ActiveItem()
	if !ok {
		t.Fatal("list selection vanished behind the open form")
	}
	if after.ID != before.ID {
		t.Fatalf("the work-item list moved behind the open form: %q -> %q", before.ID, after.ID)
	}
}
