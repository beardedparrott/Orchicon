package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// fakeShell records what the screen pushes to the composer's dock strips.
type fakeShell struct {
	notices []string
	errors  []string
}

func (f *fakeShell) DockNotice(msg string) { f.notices = append(f.notices, msg) }
func (f *fakeShell) DockError(msg string)  { f.errors = append(f.errors, msg) }

func createPlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	// Enough items that a new one lands below the fold.
	for i := 0; i < 12; i++ {
		p.addItem(&apiv1.WorkItem{
			Id: "wi-" + string(rune('a'+i)), Title: "Item " + string(rune('A'+i)),
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1",
			Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: float64(i + 1),
		})
	}
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)
	return m
}

// The operator: "New work items don't seem to be actually saving and creating
// them." The create DID work; two things hid it:
//
//  1. the screen's notice is appended to the body and then CUT OFF by FitLines
//     whenever the panes fill the height, so success reported nothing visible;
//  2. the cursor stayed on the previously selected row (selection is preserved
//     across a reload now), so a new item below the fold changed nothing on
//     screen.
//
// So: every outcome goes to the composer's dock strip, and a created row is
// FOCUSED as soon as it appears.
func TestCreateIsVisibleAndFocusesTheNewItem(t *testing.T) {
	m := createPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)

	run(t, m, press(t, m, "n"))
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}
	f.Set("title", "Brand new item")
	run(t, m, press(t, m, "ctrl+s"))

	// The success is reported where it cannot be truncated.
	if len(shell.notices) == 0 {
		t.Fatal("a successful create must be reported on the dock, not only in the screen's truncated notice")
	}
	if !strings.Contains(strings.Join(shell.notices, " "), "succeeded") {
		t.Fatalf("dock notices = %v", shell.notices)
	}

	// And the new row is focused once the reload lands.
	load(t, m, srcWorkItems)
	it, ok := m.ActiveItem()
	if !ok {
		t.Fatal("no selection after create")
	}
	if !strings.Contains(it.Title, "Brand new item") {
		t.Fatalf("the created item must be focused, cursor is on %q", it.Title)
	}
}

// A rejection must be LOUD on the dock (the error strip), not merely in the
// truncated notice line.
func TestRejectedWriteReportsOnTheDock(t *testing.T) {
	m := createPlane(t)
	shell := &fakeShell{}
	m.Base.SetShell(shell)

	workSink{m}.Fail("create work item: a task must have a parent")
	if len(shell.errors) == 0 {
		t.Fatal("a failed write must reach the dock error strip")
	}
	if !strings.Contains(shell.errors[0], "must have a parent") {
		t.Fatalf("dock error = %v", shell.errors)
	}
}

// The operator: "Sort is working but jumping to the bottom of the screen." The
// reload after a sort must start at the TOP, not restore the old row's new
// position and scroll to keep it visible.
func TestSortResetsTheCursorToTheTop(t *testing.T) {
	m := createPlane(t)
	tbl := m.Base.ActiveTable()

	// Select something low so a naive restore would scroll.
	if !m.SelectItem(srcWorkItems, "wi-l") {
		t.Fatal("could not select the last row")
	}
	m.cycleSort()
	load(t, m, srcWorkItems)

	if got := tbl.Cursor; got != 0 {
		t.Fatalf("after a sort the cursor must be at the top, got index %d", got)
	}
	if got := tbl.Offset; got != 0 {
		t.Fatalf("after a sort the window must start at the top, got offset %d", got)
	}
}
