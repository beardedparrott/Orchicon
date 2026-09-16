package execution

// grouping_test.go — category grouping in the Workers and Workflows panes.
//
// The operator: "None of the GUI categories are coming up for Workers or Workflows in the TUI" … "When an
// item (conversation, worker, workflow) is in a category, it should create a little arrow dropdown in
// their respective lists that can be collapsed or expanded."
//
// THE ARRANGEMENT (which row is a folder, which are members, how they nest) is screenkit's job and is
// tested there. What can only be tested here is the WIRING, and the wiring is where the operator's bug
// lived: the panes used to read the SHELL'S CACHE, which is loaded once at startup, so a grouping created
// in the GUI while the TUI was running never reached them. They now group from THE RESPONSE'S OWN
// categories — which is why the test below fetches with the shell's cache deliberately EMPTY.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// groupSpy is a shell that answers the MANAGE hooks (rename/delete a grouping) and records the categorize
// hook. It deliberately offers NO read side any more: the panes build their folders from the response, so
// a spy that could serve them would be testing a path that no longer exists.
type groupSpy struct {
	*categorizeSpy
	// renamed / deleted record the manage calls, so a test can assert the pane reached the shell with
	// the GROUPING BEHIND THE ROW rather than with a synthetic row id.
	renamed string
	deleted string
}

func (g *groupSpy) OpenRenameCategory(categoryID string) { g.renamed = categoryID }
func (g *groupSpy) OpenDeleteCategory(categoryID string) { g.deleted = categoryID }

// catSpec / assignSpec build the response payload a pane receives.
func catSpec(id, name string, order int) *apiv1.Category {
	return &apiv1.Category{Id: id, Name: name, SortOrder: int32(order)}
}

func assignSpec(entity, cat string) *apiv1.CategoryAssignment {
	return &apiv1.CategoryAssignment{EntityId: entity, CategoryId: cat}
}

// TestGroupedBuildsFoldersFromTheResponsesCategories is the unit half: given a response's categories and
// assignments, the pane nests members under their folder and leaves the rest to Uncategorized.
func TestGroupedBuildsFoldersFromTheResponsesCategories(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)

	flat := []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}
	out := m.grouped(flat,
		[]*apiv1.Category{catSpec("catA", "Sweepers", 0)},
		[]*apiv1.CategoryAssignment{assignSpec("w1", "catA"), assignSpec("w3", "catA")},
	)

	if len(out) != 5 {
		t.Fatalf("want a folder + 2 members + an Uncategorized folder + its member, got %d: %+v", len(out), out)
	}
	if !screenkit.IsGroupRow(out[0].ID) || !out[0].HasChildren || out[0].Title != "Sweepers" {
		t.Fatalf("the first row must be the collapsible folder, got %+v", out[0])
	}
	for _, child := range out[1:3] {
		if child.Parent != out[0].ID || child.Depth != 1 {
			t.Fatalf("member %q must hang off the folder at depth 1, got %+v", child.ID, child)
		}
	}
	unc := out[3]
	if screenkit.GroupCategoryID(unc.ID) != screenkit.UncategorizedGroupID || !unc.HasChildren {
		t.Fatalf("ungrouped items must land in an Uncategorized folder, got %+v", unc)
	}
	if out[4].ID != "w2" || out[4].Parent != unc.ID {
		t.Fatalf("the ungrouped worker must hang off Uncategorized, got %+v", out[4])
	}
}

// TestGroupedIsInertWithNoCategories: a plane with no groupings renders exactly the flat list it always
// did — the property that let this land on the panes without disturbing them.
func TestGroupedIsInertWithNoCategories(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	flat := []screenkit.Item{{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}}
	out := m.grouped(flat, nil, nil)
	if len(out) != 2 || out[0].Parent != "" || out[1].Parent != "" {
		t.Fatalf("without categories the list must be unchanged, got %+v", out)
	}
}

// TestWorkerPaneGroupsFromItsOwnResponse is the WIRING, driven end to end through the real fetch.
//
// The shell's category cache is DELIBERATELY EMPTY (`crudExec` never loads one) while the response carries
// a grouping. Before the fix the pane consulted the cache and rendered a flat list; now the folders come
// from the response, so they appear regardless of what the shell happens to hold — which is what makes a
// grouping created in the GUI show up without restarting the TUI.
func TestWorkerPaneGroupsFromItsOwnResponse(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	m.SetShell(&groupSpy{categorizeSpy: &categorizeSpy{}})
	// The shell holds nothing: this is the stale-cache case, not a prepared one.
	if len(m.Base.SourcesForTest()) == 0 {
		t.Fatal("fixture: the screen must have its sources registered")
	}

	fp := &fakePlane{
		workerItems: []*apiv1.WorkerListItem{
			{Worker: &apiv1.Worker{Id: "w1", Name: "one"}},
			{Worker: &apiv1.Worker{Id: "w2", Name: "two"}},
		},
		workerCategories:  []*apiv1.Category{catSpec("catA", "Sweepers", 0)},
		workerAssignments: []*apiv1.CategoryAssignment{assignSpec("w1", "catA")},
	}
	m = newModel(t, fp)

	items, _, err := m.fetchWorkers(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchWorkers: %v", err)
	}

	var folder, member, ungrouped bool
	for _, it := range items {
		switch {
		case screenkit.GroupCategoryID(it.ID) == "catA":
			folder = it.HasChildren && it.Title == "Sweepers"
		case it.ID == "w1":
			member = it.Parent != "" && it.Depth == 1
		case it.ID == "w2":
			ungrouped = it.Parent != ""
		}
	}
	if !folder {
		t.Fatalf("the response's grouping must render as a folder: %+v", items)
	}
	if !member {
		t.Fatalf("the assigned worker must be nested under it: %+v", items)
	}
	if !ungrouped {
		t.Fatalf("the unassigned worker must land in Uncategorized: %+v", items)
	}
}

// TestCategorizeRefusesACategoryRow: a category row's id is SYNTHETIC, so a write aimed at it would hit
// the server with an id it has never heard of and fail with a message unrelated to what the operator did.
func TestCategorizeRefusesACategoryRow(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: screenkit.GroupRowID("catA"), Title: "Sweepers", HasChildren: true},
	}, "")
	spy := &groupSpy{categorizeSpy: &categorizeSpy{}}
	m.SetShell(spy)

	m.notice = ""
	if _, handled := m.handleActionKey(keyCategorize); !handled {
		t.Fatal("C on a category row must be handled, to refuse")
	}
	if spy.calls != 0 {
		t.Fatal("a write must NOT be opened against a category row's synthetic id")
	}
	if m.notice == "" {
		t.Fatal("the refusal must say something")
	}
}

// TestWorkerWriteChordsRefuseACategoryRow: the item ops must not act on a grouping. `e` and `x` are
// EXEMPT because they legitimately apply to the FOLDER the cursor is on (rename it, delete it) — that is
// the GUI's own placement — while publish / set-active / version have no meaning for a grouping and would
// otherwise ask the server to publish one.
func TestWorkerWriteChordsRefuseACategoryRow(t *testing.T) {
	m, writes, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: screenkit.GroupRowID("catA"), Title: "Sweepers", HasChildren: true},
	}, "")
	spy := &groupSpy{categorizeSpy: &categorizeSpy{}}
	m.SetShell(spy)

	for _, k := range []string{keyEditVersion, keyPublish, keySetActive} {
		m.notice = ""
		if _, handled := m.handleActionKey(k); !handled {
			t.Fatalf("%q on a category row must be handled, to refuse", k)
		}
		if m.notice == "" {
			t.Fatalf("%q must refuse with a reason rather than act on a grouping", k)
		}
	}
	if len(*writes) != 0 {
		t.Fatalf("no write may be issued for a category row, got %v", *writes)
	}
}

// TestCategoryRowChordsManageTheGrouping: `e` renames and `x` deletes the GROUPING behind the row — the
// GUI's per-folder affordances, reachable from the pane instead of from a special section. The shell must
// be handed the CATEGORY ID, not the synthetic row id, or the write would go to an unknown entity.
func TestCategoryRowChordsManageTheGrouping(t *testing.T) {
	m, writes, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: screenkit.GroupRowID("cat-77"), Title: "Sweepers", HasChildren: true},
	}, "")
	spy := &groupSpy{categorizeSpy: &categorizeSpy{}}
	m.SetShell(spy)

	if _, handled := m.handleActionKey(keyRenameCategory); !handled {
		t.Fatalf("%q on a category row must be handled", keyRenameCategory)
	}
	if spy.renamed != "cat-77" {
		t.Fatalf("rename must target the CATEGORY id behind the row, got %q", spy.renamed)
	}
	if _, handled := m.handleActionKey(keyDeleteCategory); !handled {
		t.Fatalf("%q on a category row must be handled", keyDeleteCategory)
	}
	if spy.deleted != "cat-77" {
		t.Fatalf("delete must target the CATEGORY id behind the row, got %q", spy.deleted)
	}
	// And neither key may touch a worker: they are layout ops, not item ops.
	if len(*writes) != 0 {
		t.Fatalf("managing a grouping must not issue a worker write, got %v", *writes)
	}
}

// TestCategoryRowChordsDoNotFireOnAnItem: the same keys on a WORKER row keep their item meaning. The
// folder chords are contextual, so this is the half that would regress silently.
func TestCategoryRowChordsDoNotFireOnAnItem(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "Sweeper"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{{ID: "w1", Title: "Sweeper"}}, "")
	spy := &groupSpy{categorizeSpy: &categorizeSpy{}}
	m.SetShell(spy)

	if _, handled := m.handleActionKey(keyRenameCategory); !handled {
		t.Fatal("e on a worker row must still be handled (it edits the worker)")
	}
	if spy.renamed != "" || spy.deleted != "" {
		t.Fatalf("a worker row must not manage a grouping, got rename=%q delete=%q", spy.renamed, spy.deleted)
	}
}

// TestCollapseSurvivesAReload pins the reason collapse has to persist across a reload at all: the rolling
// refresh reloads every list every few seconds, so a folder that re-opened on each load would spring open
// under the operator's cursor and collapsing would be unusable. It asserts the VISIBLE ROW COUNT, which is
// what "collapsed" actually means — the children are not drawn.
func TestCollapseSurvivesAReload(t *testing.T) {
	folderID := screenkit.GroupRowID("catA")
	rows := func() []screenkit.Item {
		return []screenkit.Item{
			{ID: folderID, Title: "Sweepers", HasChildren: true},
			{ID: "w1", Title: "one", Parent: folderID, Depth: 1},
			{ID: "w2", Title: "two", Parent: folderID, Depth: 1},
		}
	}

	tbl := kit2.NewTable("workers")
	tbl.SetItems(rows(), "")
	if got := len(tbl.VisibleRows()); got != 3 {
		t.Fatalf("a fresh list renders fully expanded, want 3 visible rows, got %d", got)
	}
	// Collapse the folder the way the operator does.
	tbl.Cursor = 0
	if !tbl.Toggle() {
		t.Fatal("the folder must be toggleable")
	}
	if got := len(tbl.VisibleRows()); got != 1 {
		t.Fatalf("collapsing must hide the members, want 1 visible row, got %d", got)
	}

	// THE REFRESH LANDS with the same rows — this is the case that used to re-open everything.
	tbl.SetItems(rows(), "")

	if got := len(tbl.VisibleRows()); got != 1 {
		t.Fatalf("the collapsed folder must STAY collapsed across a reload, got %d visible rows "+
			"(the rolling refresh reloads every few seconds, so a re-open makes collapsing unusable)", got)
	}
}

// TestANewFolderStartsOpen: only rows whose id SURVIVES a reload keep their state. A folder appearing for
// the first time must be open, or a grouping would arrive already hidden.
func TestANewFolderStartsOpen(t *testing.T) {
	tbl := kit2.NewTable("workers")
	folderID := screenkit.GroupRowID("catNEW")
	tbl.SetItems([]screenkit.Item{
		{ID: folderID, Title: "Fresh", HasChildren: true},
		{ID: "w1", Title: "one", Parent: folderID, Depth: 1},
	}, "")
	if got := len(tbl.VisibleRows()); got != 2 {
		t.Fatalf("a newly-appeared folder must be open, want 2 visible rows, got %d", got)
	}
}

// --- bulk delete on the Workers and Workflows panes ----------------------------------------------

// TestMarkingWorkersOffersABulkDelete is the operator's report: "There doesn't seem to be a bulk delete on
// workflows or workers. We should have the ctrl+x there as well."
//
// The mechanism was already shared (SPACE marks, BulkIDs reports a selection, actionsForSelection lets a
// source replace its single-row actions); only the branch was missing, so marking rows highlighted them
// and offered nothing.
func TestMarkingWorkersOffersABulkDelete(t *testing.T) {
	m, _, _ := crudExec(t, &apiv1.Worker{Id: "w1", Name: "one"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}, "")

	// ONE mark is NOT a selection (the shared threshold), so the single-row actions still stand.
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	if acts := m.actionsForSelection(); hasAction(acts, "delete 1 selected") {
		t.Fatalf("one mark must not offer a bulk delete: %+v", acts)
	}
	// TWO marks is a selection, and the bulk delete replaces them. The marks are made with the REAL
	// gesture (space on the cursor row, which then advances), so this pins the wiring end to end rather
	// than handing the screen a prepared set.
	press(t, m, " ")
	acts := m.actionsForSelection()
	if !hasAction(acts, "delete 2 selected") {
		t.Fatalf("two marks must offer a bulk delete: %+v", acts)
	}
	// IT ANSWERS THE PANE'S OWN DELETE KEY, so the operator does not learn a second chord.
	var key string
	for _, a := range acts {
		if a.Label == "delete 2 selected" {
			key = a.Key
		}
	}
	if key != keyDelete {
		t.Fatalf("the bulk delete must answer %q (the pane's own), got %q", keyDelete, key)
	}
}

// TestBulkDeleteWorkersWritesEachAndCountsRefusals: one write per id, and a partial failure is reported as
// one instead of the first error hiding the rest.
func TestBulkDeleteWorkersWritesEachAndCountsRefusals(t *testing.T) {
	m, _, writes := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}, "")
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")

	var del kit2.Action
	for _, a := range m.actionsForSelection() {
		if a.Label == "delete 2 selected" {
			del = a
		}
	}
	if del.Do == nil {
		t.Fatal("the bulk delete must carry a write")
	}
	if !del.NeedsConfirm() {
		t.Fatal("deleting workers removes every version and cannot be undone, so it must confirm")
	}
	if !strings.Contains(del.Confirm, "2 workers") {
		t.Fatalf("the confirm must state the count: %q", del.Confirm)
	}
	if err := del.Do(context.Background()); err != nil {
		t.Fatalf("a clean bulk delete must succeed: %v", err)
	}
	got := *writes
	if len(got) != 2 || got[0] != "delete" || got[1] != "delete" {
		t.Fatalf("want one delete per marked worker, got %v", got)
	}
}

// TestBulkDeleteSkipsACategoryFolder: a folder is a ROW and can be marked like any other, but its id is
// synthetic — so it must not be written, and the confirm's count must not include it.
//
// THREE rows are marked (the folder and BOTH of its members) because of the shared threshold: after the
// folder is filtered out, two real entities remain. Marking the folder plus ONE member would leave a
// single entity, which is correctly NOT a bulk selection — one row is not a selection, it is the row the
// cursor is on.
func TestBulkDeleteSkipsACategoryFolder(t *testing.T) {
	m, _, writes := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	folderID := screenkit.GroupRowID("catA")
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: folderID, Title: "Sweepers", HasChildren: true},
		{ID: "w1", Title: "one", Parent: folderID, Depth: 1},
		{ID: "w2", Title: "two", Parent: folderID, Depth: 1},
	}, "")
	m.Base.SelectItem(srcWorkers, folderID)
	press(t, m, " ")
	press(t, m, " ")
	press(t, m, " ")

	acts := m.actionsForSelection()
	if !hasAction(acts, "delete 2 selected") {
		t.Fatalf("the folder must be excluded from the count, leaving 2 real workers: %+v", acts)
	}
	for _, a := range acts {
		if a.Label == "delete 2 selected" {
			if err := a.Do(context.Background()); err != nil {
				t.Fatalf("bulk delete: %v", err)
			}
		}
	}
	if got := *writes; len(got) != 2 {
		t.Fatalf("the folder must be skipped, so exactly two deletes for three marks: got %v", got)
	}
}

// TestWorkflowsBulkDeleteUsesItsOwnKey: the Workflows pane deletes on `X` (lowercase `x` belongs to the
// step editor), so its BULK delete must answer the same key its single delete does.
func TestWorkflowsBulkDeleteUsesItsOwnKey(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkflows) {
		t.Fatal("fixture: could not focus the Workflows pane")
	}
	m.Base.LoadItems(srcWorkflows, []screenkit.Item{
		{ID: "wf1", Title: "one"}, {ID: "wf2", Title: "two"},
	}, "")
	m.Base.SelectItem(srcWorkflows, "wf1")
	press(t, m, " ")
	press(t, m, " ")

	acts := m.actionsForSelection()
	if !hasAction(acts, "delete 2 selected") {
		t.Fatalf("two marked workflows must offer a bulk delete: %+v", acts)
	}
	for _, a := range acts {
		if a.Label == "delete 2 selected" && a.Key != keyDeleteWorkflow {
			t.Fatalf("the bulk delete must answer %q, got %q", keyDeleteWorkflow, a.Key)
		}
	}
}

// hasAction reports whether an action set contains a label.
func hasAction(acts []kit2.Action, label string) bool {
	for _, a := range acts {
		if a.Label == label {
			return true
		}
	}
	return false
}
