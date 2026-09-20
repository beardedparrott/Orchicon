package work

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// featTaskPlane seeds two projects, each with a parent chain, so the picker can
// be checked for cross-project leakage.
func featTaskPlane(t *testing.T) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.seedProject("proj-2", "Other")
	// proj-1: epic -> feature -> task, and a standalone task.
	p.addItem(&apiv1.WorkItem{Id: "e1", Title: "Epic one", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "f1", Title: "Feature one", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_FEATURE, ParentId: "e1", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.addItem(&apiv1.WorkItem{Id: "t1", Title: "Task one", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, ParentId: "f1", ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	// proj-2: a parent that must NEVER be offered while proj-1 is chosen.
	p.addItem(&apiv1.WorkItem{Id: "e2", Title: "Epic two", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC, ProjectId: "proj-2", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})

	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	run(t, m, press(t, m, "n"))
	return m
}

// FEATURES AND TASKS ARE PARENTS TOO. The server rule is "a child must be
// strictly deeper than its parent" (internal/workitem/validate.go), which makes
// feature -> task|subtask and task -> subtask legal. The form must accept them,
// and must only CORRECT a genuinely illegal pairing.
func TestFeaturesAndTasksCanBeParents(t *testing.T) {
	m := featTaskPlane(t)
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("n must open the create form")
	}

	// A FEATURE parent accepts a task (legal) and must not be overridden.
	f.Set("kind", "task")
	f.Set("parent", "f1")
	if got := f.Values["kind"]; got != "task" {
		t.Fatalf("a task under a feature parent is legal and must be preserved, got %q", got)
	}
	// ...and accepts a subtask.
	f.Set("kind", "subtask")
	f.Set("parent", "f1")
	if got := f.Values["kind"]; got != "subtask" {
		t.Fatalf("a subtask under a feature parent is legal, got %q", got)
	}
	// A TASK parent accepts a subtask.
	f.Set("kind", "subtask")
	f.Set("parent", "t1")
	if got := f.Values["kind"]; got != "subtask" {
		t.Fatalf("a subtask under a task parent is legal, got %q", got)
	}
	// But an EPIC under a feature parent is illegal, so it is corrected.
	f.Set("kind", "epic")
	f.Set("parent", "f1")
	if got := f.Values["kind"]; got == "epic" {
		t.Fatalf("an epic under a feature parent is illegal and must be corrected, got %q", got)
	}
}

// The picker only offers parents from the CHOSEN project — the server rejects a
// cross-project parent, so offering one would be offering a guaranteed failure.
func TestParentPickerIsScopedToTheProject(t *testing.T) {
	m := featTaskPlane(t)
	f := m.ActiveForm()
	if f == nil {
		t.Fatal("no create form")
	}

	parents := func() []string {
		for i := range f.Specs {
			if f.Specs[i].Name == "parent" {
				out := make([]string, 0, len(f.Specs[i].Options))
				for _, o := range f.Specs[i].Options {
					out = append(out, o.Value)
				}
				return out
			}
		}
		t.Fatal("no parent field")
		return nil
	}

	// proj-1 is the initial project: only its items are offered.
	f.Set("project", "proj-1")
	for _, v := range parents() {
		if v == "e2" {
			t.Fatalf("a parent from another project must not be offered: %v", parents())
		}
	}
	// Switching to proj-2 re-scopes the list, and drops a now-foreign parent.
	f.Set("parent", "e1")
	f.Set("project", "proj-2")
	list := parents()
	if strings.Contains(strings.Join(list, ","), "e1") {
		t.Fatalf("the picker must not offer proj-1 items while proj-2 is chosen: %v", list)
	}
	if f.Values["parent"] != "" {
		t.Fatalf("a parent from the previous project must be dropped, got %q", f.Values["parent"])
	}
	if !hasOption([]kit2.Option{{Value: "e2"}}, "e2") {
		t.Fatal("sanity: the seeded proj-2 parent should exist")
	}
	// The local mirror refuses a cross-project parent outright.
	if err := m.validateHierarchy("proj-1", "e2", "feature"); err == nil {
		t.Fatal("a cross-project parent must be rejected locally")
	}
}
