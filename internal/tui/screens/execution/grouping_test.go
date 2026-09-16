package execution

// grouping_test.go — the WIRING of category grouping into the Workers and Workflows panes.
//
// The arrangement itself (which row is a parent, which are children) is screenkit's job and is tested
// there. What can only be tested here is that these panes USE it, with the right target type, and that
// the write chords do not aim at a category row's synthetic id.
//
// The operator: "When an item (conversation, worker, workflow) is in a category, it should create a
// little arrow dropdown in their respective lists that can be collapsed or expanded."

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// groupSpy is a shell that answers the grouping read and manage hooks.
type groupSpy struct {
	*categorizeSpy
	// byEntity maps an entity id to (category id, category name).
	byEntity map[string][2]string
	// groups is the ORDERED folder list the shell hands back (empty categories included).
	groups []screenkit.GroupSpec
	// seenTargets records the target type each lookup was made with.
	seenTargets []apiv1.CategoryTargetType
	// renamed / deleted record the manage calls, so a test can assert the pane reached the shell with
	// the GROUPING BEHIND THE ROW rather than with a synthetic row id.
	renamed string
	deleted string
}

func (g *groupSpy) CategoryOf(target apiv1.CategoryTargetType, entityID string) (string, string) {
	g.seenTargets = append(g.seenTargets, target)
	if v, ok := g.byEntity[entityID]; ok {
		return v[0], v[1]
	}
	return "", ""
}

func (g *groupSpy) CategoryGroupsFor(target apiv1.CategoryTargetType) []screenkit.GroupSpec {
	g.seenTargets = append(g.seenTargets, target)
	return g.groups
}

func (g *groupSpy) OpenRenameCategory(categoryID string) { g.renamed = categoryID }
func (g *groupSpy) OpenDeleteCategory(categoryID string) { g.deleted = categoryID }

// TestWorkersPaneGroupsByCategory: the members must nest under a collapsible category row, and the
// lookup must be made with the WORKER target type — the wrong type would offer another kind's groups.
func TestWorkersPaneGroupsByCategory(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	spy := &groupSpy{
		categorizeSpy: &categorizeSpy{},
		groups:        []screenkit.GroupSpec{{ID: "catA", Name: "Sweepers"}},
		byEntity: map[string][2]string{
			"w1": {"catA", "Sweepers"},
			"w3": {"catA", "Sweepers"},
		},
	}
	m.SetShell(spy)

	flat := []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}
	out := m.grouped(flat, apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)

	if len(out) != 5 {
		t.Fatalf("want a folder + 2 members + an Uncategorized folder + its member, got %d: %+v", len(out), out)
	}
	if !screenkit.IsGroupRow(out[0].ID) || !out[0].HasChildren || out[0].Title != "Sweepers" {
		t.Fatalf("the first row must be the collapsible category row, got %+v", out[0])
	}
	for _, child := range out[1:3] {
		if child.Parent != out[0].ID || child.Depth != 1 {
			t.Fatalf("member %q must hang off the category row at depth 1, got %+v", child.ID, child)
		}
	}
	// Ungrouped items go into the trailing "Uncategorized" folder, exactly as the GUI renders them.
	unc := out[3]
	if screenkit.GroupCategoryID(unc.ID) != screenkit.UncategorizedGroupID || !unc.HasChildren {
		t.Fatalf("ungrouped items must land in an Uncategorized folder, got %+v", unc)
	}
	if out[4].ID != "w2" || out[4].Parent != unc.ID {
		t.Fatalf("the ungrouped worker must hang off Uncategorized, got %+v", out[4])
	}
	// THE TARGET TYPE IS THE PANE'S OWN, and every lookup used it.
	if len(spy.seenTargets) == 0 {
		t.Fatal("the pane must ask the shell about its own kind of grouping")
	}
	for _, got := range spy.seenTargets {
		if got != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER {
			t.Fatalf("the Workers pane looked up %v, want WORKER", got)
		}
	}
}

// TestWorkflowsPaneUsesTheWorkflowTargetType: the same wiring, the other kind.
func TestWorkflowsPaneUsesTheWorkflowTargetType(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	spy := &groupSpy{
		categorizeSpy: &categorizeSpy{},
		groups:        []screenkit.GroupSpec{{ID: "catW", Name: "Ingestion"}},
		byEntity:      map[string][2]string{"wf1": {"catW", "Ingestion"}},
	}
	m.SetShell(spy)

	out := m.grouped([]screenkit.Item{{ID: "wf1", Title: "Quick Work"}},
		apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW)

	if len(out) != 2 || out[0].Title != "Ingestion" {
		t.Fatalf("want one category row plus its member, got %+v", out)
	}
	for _, got := range spy.seenTargets {
		if got != apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKFLOW {
			t.Fatalf("the Workflows pane looked up %v, want WORKFLOW", got)
		}
	}
}

// TestGroupingIsInertWithoutAShell: with no hook at all the list must come back UNCHANGED, so a build
// or a fixture without the shell renders exactly the flat list it always did.
func TestGroupingIsInertWithoutAShell(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	m.SetShell(nil)
	flat := []screenkit.Item{{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}}
	out := m.grouped(flat, apiv1.CategoryTargetType_CATEGORY_TARGET_TYPE_WORKER)
	if len(out) != 2 || out[0].Parent != "" || out[1].Parent != "" {
		t.Fatalf("without a shell the list must be unchanged, got %+v", out)
	}
}

// TestCategorizeRefusesACategoryRow: a category row's id is SYNTHETIC, so a write aimed at it would hit
// the server with an id it has never heard of and fail with a message unrelated to what the operator
// did. It must refuse with a reason instead.
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
// EXEMPT because they legitimately apply to the FOLDER the cursor is on (rename it, delete it) — that
// is the GUI's own placement — while publish / set-active / version have no meaning for a grouping and
// would otherwise ask the server to publish one.
func TestWorkerWriteChordsRefuseACategoryRow(t *testing.T) {
	m, writes, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	// The cursor sits on the synthesized category row.
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
// GUI's per-folder affordances, reachable from the pane instead of from a special section. The shell
// must be handed the CATEGORY ID, not the synthetic row id, or the write would go to an unknown entity.
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

// TestCollapseSurvivesAReload pins the reason collapse has to persist across a reload at all: the
// rolling refresh reloads every list every few seconds, so a folder that re-opened on each load would
// spring open under the operator's cursor and collapsing would be unusable. It asserts the VISIBLE ROW
// COUNT, which is what "collapsed" actually means — the children are not drawn.
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

// TestANewFolderStartsOpen: only rows whose id SURVIVES a reload keep their state. A folder appearing
// for the first time must be open, or a grouping would arrive already hidden.
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
