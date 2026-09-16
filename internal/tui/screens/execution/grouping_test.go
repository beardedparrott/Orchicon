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
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// groupSpy is a shell that answers the grouping read hook.
type groupSpy struct {
	*categorizeSpy
	// byEntity maps an entity id to (category id, category name).
	byEntity map[string][2]string
	// seenTargets records the target type each lookup was made with.
	seenTargets []apiv1.CategoryTargetType
}

func (g *groupSpy) CategoryOf(target apiv1.CategoryTargetType, entityID string) (string, string) {
	g.seenTargets = append(g.seenTargets, target)
	if v, ok := g.byEntity[entityID]; ok {
		return v[0], v[1]
	}
	return "", ""
}

// TestWorkersPaneGroupsByCategory: the members must nest under a collapsible category row, and the
// lookup must be made with the WORKER target type — the wrong type would offer another kind's groups.
func TestWorkersPaneGroupsByCategory(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	spy := &groupSpy{
		categorizeSpy: &categorizeSpy{},
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

	if len(out) != 4 {
		t.Fatalf("want a parent + 2 members + 1 ungrouped, got %d: %+v", len(out), out)
	}
	if !screenkit.IsGroupRow(out[0].ID) || !out[0].HasChildren || out[0].Title != "Sweepers" {
		t.Fatalf("the first row must be the collapsible category row, got %+v", out[0])
	}
	for _, child := range out[1:3] {
		if child.Parent != out[0].ID || child.Depth != 1 {
			t.Fatalf("member %q must hang off the category row at depth 1, got %+v", child.ID, child)
		}
	}
	if out[3].ID != "w2" || out[3].Parent != "" {
		t.Fatalf("the ungrouped worker must stay flat and last, got %+v", out[3])
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

// TestWorkerWriteChordsRefuseACategoryRow: the same guard for the worker ops, which would otherwise ask
// the server to publish or deprecate a grouping.
func TestWorkerWriteChordsRefuseACategoryRow(t *testing.T) {
	m, writes, _ := crudExec(t, &apiv1.Worker{Id: "w1"}, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	// The cursor sits on the synthesized category row.
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: screenkit.GroupRowID("catA"), Title: "Sweepers", HasChildren: true},
	}, "")
	m.SetShell(&groupSpy{categorizeSpy: &categorizeSpy{}})

	for _, k := range []string{keyEditWorker, keyEditVersion, keyPublish, keySetActive} {
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
