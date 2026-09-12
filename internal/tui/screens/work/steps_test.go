package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

func seqPlane(t *testing.T) (*Model, *fakePlane) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	// Three steps in a deliberate stored order: B runs first, then A, then C.
	p.addItem(&apiv1.WorkItem{Id: "wi-a", Title: "Alpha", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 2})
	p.addItem(&apiv1.WorkItem{Id: "wi-b", Title: "Bravo", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1})
	p.addItem(&apiv1.WorkItem{Id: "wi-c", Title: "Charlie", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 3})
	// A lone child: it has no sibling order, so it must NOT be numbered.
	p.addItem(&apiv1.WorkItem{Id: "wi-lone", Title: "Only", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "wi-only-kid", Title: "Solo kid", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, ParentId: "wi-lone", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m, p
}

// The operator: "ACTUALLY show the number next to the work item under the
// parent to show the exact order it is in." Step numbers come from the STORED
// sequence, so the list states the execution order.
func TestTreeRowsShowStepNumbers(t *testing.T) {
	m, _ := seqPlane(t)
	rows := itemsOf(m, srcWorkItems)
	byID := map[string]string{}
	for _, r := range rows {
		byID[r.ID] = r.Title
	}

	if got := byID["wi-b"]; !strings.HasPrefix(got, "1. ") {
		t.Fatalf("the first step must be numbered 1: %q", got)
	}
	if got := byID["wi-a"]; !strings.HasPrefix(got, "2. ") {
		t.Fatalf("the second step must be numbered 2: %q", got)
	}
	if got := byID["wi-c"]; !strings.HasPrefix(got, "3. ") {
		t.Fatalf("the third step must be numbered 3: %q", got)
	}
	// A lone child has no order to show.
	if got := byID["wi-only-kid"]; strings.Contains(got, ". ") {
		t.Fatalf("a lone child must not be numbered: %q", got)
	}
	// The default (sequence) view lists the epic's steps in numeric order.
	var steps []string
	for _, r := range rows {
		if r.Parent != "wi-epic" {
			continue
		}
		steps = append(steps, string(r.Title[0]))
	}
	if strings.Join(steps, "") != "123" {
		t.Fatalf("the sequence view must list the epic's steps in order, got %v", steps)
	}
	// The ROOTS are siblings too, so they carry their own numbering — the top
	// level is a sequence as much as any other sibling group.
	if got := byID["wi-epic"]; !strings.HasPrefix(got, "1. ") {
		t.Fatalf("the first root must be numbered: %q", got)
	}
}

// The number is the EXECUTION order, not the display order: under a different
// sort the rows move but the numbers do not, which is the distinction the
// operator could not see before.
func TestStepNumbersAreIndependentOfTheDisplaySort(t *testing.T) {
	m, _ := seqPlane(t)
	m.cycleSort() // sequence -> title
	if got := m.SortMode(); got != sortTitle {
		t.Fatalf("expected the title sort, got %q", got)
	}
	load(t, m, srcWorkItems)

	var titles []string
	for _, r := range itemsOf(m, srcWorkItems) {
		if strings.Contains(r.Title, "Alpha") || strings.Contains(r.Title, "Bravo") || strings.Contains(r.Title, "Charlie") {
			titles = append(titles, r.Title)
		}
	}
	// Alphabetical DISPLAY order...
	if !strings.HasPrefix(titles[0], "2. ") || !strings.Contains(titles[0], "Alpha") {
		t.Fatalf("title sort must list Alpha first, got %v", titles)
	}
	// ...carrying the STORED step numbers (Alpha is step 2, Bravo step 1).
	if !strings.HasPrefix(titles[1], "1. ") || !strings.Contains(titles[1], "Bravo") {
		t.Fatalf("step numbers must survive a display sort, got %v", titles)
	}
	if !strings.HasPrefix(titles[2], "3. ") || !strings.Contains(titles[2], "Charlie") {
		t.Fatalf("step numbers must survive a display sort, got %v", titles)
	}
}

// +/- move the selected step within its siblings; the chords are inert outside
// the Tree view (a flat list has no sibling order to move within).
func TestMoveStepChords(t *testing.T) {
	m, p := seqPlane(t)
	// Select AFTER the load: LoadItems reseats the cursor at the top, so a
	// selection made before it would be silently thrown away.
	load(t, m, srcWorkItems)
	if !m.SelectItem(srcWorkItems, "wi-a") {
		t.Fatal("could not select wi-a")
	}

	last := func() string {
		if len(p.reorders) == 0 {
			t.Fatal("no ReorderWorkItems call")
		}
		return strings.Join(p.reorders[len(p.reorders)-1].GetChildIds(), ",")
	}

	// "+" moves it one step earlier in the sequence.
	run(t, m, press(t, m, "+"))
	if got := last(); got != "wi-a,wi-b,wi-c" {
		t.Fatalf("after '+': child order = %s, want wi-a,wi-b,wi-c", got)
	}
	// "-" moves it back.
	run(t, m, press(t, m, "-"))
	if got := last(); got != "wi-b,wi-a,wi-c" {
		t.Fatalf("after '-': child order = %s, want wi-b,wi-a,wi-c", got)
	}

	// The old J/K chords must be inert.
	before := len(p.reorders)
	press(t, m, "J")
	press(t, m, "K")
	if len(p.reorders) != before {
		t.Fatal("J/K must no longer reorder")
	}
}

// The detail pane no longer shows the meaningless sort_order float.
func TestDetailOmitsTheSortOrderFloat(t *testing.T) {
	m, _ := seqPlane(t)
	load(t, m, srcWorkItems)
	view := m.View()
	if strings.Contains(view, "sort order") {
		t.Fatalf("the detail pane must not show a raw sort_order:\n%s", view)
	}
}
