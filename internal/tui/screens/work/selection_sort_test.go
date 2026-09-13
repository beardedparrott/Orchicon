package work

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The operator: "+/- moves it properly but then jumps your focus up to the
// parent. It should keep selection/focus on the work item you just moved."
//
// The cause was general: LoadItems → SetItems reseated the cursor at the top on
// EVERY refresh, so any mutation threw the selection away.
func TestSelectionSurvivesAReload(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	for i, id := range []string{"wi-a", "wi-b", "wi-c"} {
		p.addItem(&apiv1.WorkItem{
			Id: id, Title: id, Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "wi-epic",
			ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: float64(i + 1),
		})
	}
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	// Select the LAST child, then reload — the selection must stick.
	if !m.SelectItem(srcWorkItems, "wi-c") {
		t.Fatal("could not select wi-c")
	}
	load(t, m, srcWorkItems)
	it, ok := m.ActiveItem()
	if !ok || it.ID != "wi-c" {
		t.Fatalf("the selection must survive a reload: got %q (ok=%v)", it.ID, ok)
	}

	// Moving it up keeps the focus on it.
	run(t, m, press(t, m, "+"))
	after, ok := m.ActiveItem()
	if !ok || after.ID != "wi-c" {
		t.Fatalf("after '+' the focus must stay on the moved item: got %q", after.ID)
	}
}

// The operator: "Sort is there and it goes through the different orderings but
// it doesn't actually apply the sort. Nothing currently changes."
//
// Root cause: RowAction.Do returned nothing, so the control's Refresh command
// was dropped — the label changed and the list never did.
func TestSortControlActuallyApplies(t *testing.T) {
	m, _ := seqPlane(t)
	// The epic's steps in SEQUENCE order are Bravo(1), Alpha(2), Charlie(3).
	childTitles := func() []string {
		var out []string
		for _, r := range m.Base.ActiveTable().VisibleRows() {
			if r.Parent == "wi-epic" {
				out = append(out, r.Cells[0])
			}
		}
		return out
	}
	before := childTitles()
	if len(before) != 3 {
		t.Fatalf("expected 3 steps, got %v", before)
	}

	// The control must produce a command — that is what re-fetches.
	var found bool
	for _, a := range m.Base.ActiveTableActions() {
		if a.Label() == "sort: sequence" {
			found = true
			if cmd := a.Do(); cmd == nil {
				t.Fatal("the sort control must return a re-fetch command")
			}
		}
	}
	if !found {
		t.Fatalf("no sort control registered: %v", actionLabels(m))
	}

	// End to end: cycling to the title order must REORDER the list.
	m.cycleSort() // -> title
	load(t, m, srcWorkItems)
	after := childTitles()
	if strings.Join(after, ",") == strings.Join(before, ",") {
		t.Fatalf("switching to the title sort must reorder the list (still %v)", after)
	}
	// Title order is alphabetical, and the step NUMBERS stay the run order.
	if !strings.Contains(after[0], "Alpha") || !strings.HasPrefix(strings.TrimSpace(after[0]), "2.") {
		t.Fatalf("title sort: expected Alpha (run step 2) first, got %v", after)
	}
}

func actionLabels(m *Model) []string {
	var out []string
	for _, a := range m.Base.ActiveTableActions() {
		out = append(out, a.Label())
	}
	return out
}

// A click on the sort control must reach the screen (the command is forwarded)
// — the same path the operator used with the mouse.
func TestSortControlClickForwardsItsCommand(t *testing.T) {
	m, _ := seqPlane(t)
	m.SetSize(120, 40)
	x, y := findText(t, m.View(), "sort: sequence")
	m.Update(tea.MouseMsg{
		X: x, Y: y + kit2.ShellChromeRows(),
		Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	})
	if got := m.SortMode(); got != sortTitle {
		t.Fatalf("clicking the sort control must advance the mode, got %q", got)
	}
}
