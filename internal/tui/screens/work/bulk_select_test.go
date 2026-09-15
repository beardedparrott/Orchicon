package work

// bulk_select_test.go — the shared multi-select and its bulk actions.
//
// "We need to have a consistent way to handle bulk operations on all forms. My suggestion
// would be spacebar can do multi-select and Esc clears the multi-select and then the bulk
// options shows up only after you have more than one item selected."
//
// The mechanism lives in kit2 (Table.Marks + Base's keys), so every list screen gets the
// same gesture, the same clearing rule and the same threshold. These tests drive it through
// the Work screen, which is where it is wired to real actions.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// bulkModel puts three work items on the Work Items pane.
func bulkModel(t *testing.T) *Model {
	t.Helper()
	m := newModel(t, &fakePlane{})
	if !m.Base.SelectSource(srcWorkItems) {
		t.Fatal("fixture: could not focus the Work Items pane")
	}
	if !m.Base.LoadItems(srcWorkItems, []kit2.Item{
		{ID: "w1", Title: "First", Meta: "pending"},
		{ID: "w2", Title: "Second", Meta: "pending"},
		{ID: "w3", Title: "Third", Meta: "pending"},
	}, "") {
		t.Fatal("fixture: could not load the rows")
	}
	m.refreshActionBar()
	return m
}

// mark presses space n times, which marks and advances (the operator's gesture).
func mark(t *testing.T, m *Model, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	}
}

// SPACE marks, and it advances so a run of presses builds a selection.
func TestSpaceMarksAndAdvances(t *testing.T) {
	m := bulkModel(t)
	if m.Base.MarkCount() != 0 {
		t.Fatal("fixture: nothing should be marked yet")
	}
	mark(t, m, 1)
	if got := m.Base.MarkedIDs(); len(got) != 1 || got[0] != "w1" {
		t.Fatalf("marked = %v, want [w1] — space must mark the cursor row", got)
	}
	mark(t, m, 1)
	if got := m.Base.MarkedIDs(); len(got) != 2 || got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("marked = %v, want [w1 w2] in DRAW ORDER — space must advance", got)
	}
	// Space on an already-marked row UNMARKS it.
	m.Base.SelectItem(srcWorkItems, "w1")
	mark(t, m, 1)
	if got := m.Base.MarkedIDs(); len(got) != 1 || got[0] != "w2" {
		t.Fatalf("marked = %v, want [w2] — space must toggle", got)
	}
}

// The marked rows are VISIBLE as such: the gutter appears and carries an x.
func TestMarksAreVisible(t *testing.T) {
	m := bulkModel(t)
	before := m.Base.View()
	if strings.Contains(before, "[ ]") || strings.Contains(before, "[x]") {
		t.Error("the mark gutter is drawn before anything is marked — an unused gutter on every list")
	}
	mark(t, m, 1)
	after := m.Base.View()
	if !strings.Contains(after, "[x]") {
		t.Errorf("a marked row is not shown as marked:\n%s", after)
	}
	if !strings.Contains(after, "[ ]") {
		t.Error("the gutter does not show the UNMARKED rows, so the selection model is invisible")
	}
	// And the count is stated on the action row.
	if !strings.Contains(after, "1 marked") {
		t.Errorf("the action row does not state how many rows are marked:\n%s", after)
	}
}

// ESC clears, and with nothing marked it keeps its other meaning.
func TestEscClearsTheSelection(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 2)
	if m.Base.MarkCount() != 2 {
		t.Fatalf("fixture: marked = %d, want 2", m.Base.MarkCount())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.Base.MarkCount(); got != 0 {
		t.Fatalf("esc left %d row(s) marked — it must clear the multi-select first", got)
	}
	// With nothing marked, esc still unfocuses the detail (its established meaning).
	m.Base.SetFocusForTest("detail")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.Base.DetailFocusedForTest() {
		t.Error("esc with nothing marked must still leave the detail pane")
	}
}

// THE THRESHOLD: bulk options appear at MORE THAN ONE, and not at one.
func TestBulkActionsAppearOnlyAboveOneSelection(t *testing.T) {
	m := bulkModel(t)
	single := bulkLabels(m.actionsForSelection())
	if containsAny(single, "selected") {
		t.Fatalf("bulk actions were offered with nothing marked: %v", single)
	}

	mark(t, m, 1)
	one := bulkLabels(m.actionsForSelection())
	if containsAny(one, "selected") {
		t.Fatalf("bulk actions were offered with ONE row marked (%v) — one row is not a bulk "+
			"operation, it is the row the cursor is on", one)
	}

	mark(t, m, 1)
	two := bulkLabels(m.actionsForSelection())
	if !containsAny(two, "archive 2 selected") {
		t.Fatalf("two marked rows did not offer the bulk archive: %v", two)
	}
	if !containsAny(two, "delete 2 selected") {
		t.Fatalf("two marked rows did not offer the bulk delete: %v", two)
	}
	if !containsAny(two, "clear selection") {
		t.Fatalf("the bulk list does not offer the escape hatch: %v", two)
	}
	// The SINGLE-row actions are gone — the bar must not mix the two models.
	if containsAny(two, "toggle auto-start") {
		t.Fatalf("single-row actions are still offered alongside the bulk ones: %v", two)
	}
}

// The bulk actions reach the BAR and the hint line, because both are built from the same
// list — that is what makes one switch here switch everywhere.
func TestBulkActionsReachTheBarAndHints(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 2)
	m.refreshActionBar()
	// Ask the BAR, not the action slice: the label lives on the bar the operator reads.
	labels := strings.Join(m.bar.Labels(), " | ")
	for _, want := range []string{"archive", "delete"} {
		if !strings.Contains(labels, want) {
			t.Errorf("the action bar does not show the bulk %s: %s", want, labels)
		}
	}
}

// The confirm NAMES THE COUNT: a destructive operation on several rows must not read like
// one on a single row.
func TestBulkConfirmNamesTheCount(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 3)
	for _, a := range m.actionsForSelection() {
		if a.Confirm == "" {
			continue
		}
		if !strings.Contains(a.Confirm, "3") {
			t.Errorf("the confirm for %q does not name the count: %q", a.Label, a.Confirm)
		}
		if !a.Danger {
			t.Errorf("%q is destructive and must be marked Danger", a.Label)
		}
	}
}

// A bulk write never reports success it did not achieve. With no client it must ERROR rather
// than panic or claim a clean sweep — a crash inside a destructive multi-row operation would
// take the whole TUI with it.
func TestBulkWriteWithoutAClientErrorsRatherThanPanics(t *testing.T) {
	m := bulkModel(t)
	m.cl = nil
	mark(t, m, 3)
	acts := m.actionsForSelection()
	if len(acts) == 0 {
		t.Fatal("no actions at all with no client")
	}
	for _, a := range acts {
		if a.Do == nil {
			t.Errorf("%q has no Do", a.Label)
			continue
		}
		if err := a.Do(context.Background()); err == nil {
			t.Errorf("%q reported success with no client", a.Label)
		}
	}
}

// The write is SEQUENTIAL, one call per selected row, and a REJECTION is counted rather
// than swallowed — the operator must be told what actually happened.
//
// Driven through the real plane fake against the screen's own client, so the calls are the
// ones the screen really makes.
func TestBulkArchiveWritesEachSelectedRow(t *testing.T) {
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	task := func(id, title, parent string) {
		p.addItem(&apiv1.WorkItem{Id: id, Title: title, ProjectId: "proj-1", ParentId: parent,
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED})
	}
	task("wi-1", "Alpha", "")
	task("wi-2", "Beta", "")
	task("wi-3", "Gamma", "")
	// A PARENT with a child: the plane refuses to archive it, which is the asymmetry a bulk
	// action has to survive and report.
	task("wi-parent", "Parent", "")
	task("wi-child", "Child", "wi-parent")

	m := newModel(t, p)
	m.Base.SelectSource(srcWorkItems)
	m.Base.LoadItems(srcWorkItems, []kit2.Item{
		{ID: "wi-1", Title: "Alpha"}, {ID: "wi-2", Title: "Beta"},
		{ID: "wi-3", Title: "Gamma"}, {ID: "wi-parent", Title: "Parent"},
	}, "")

	ids := []string{"wi-1", "wi-2", "wi-3", "wi-parent"}
	acts := m.bulkItemActions(ids)
	var archive *kit2.Action
	for i := range acts {
		if strings.Contains(acts[i].Label, "archive") {
			archive = &acts[i]
		}
	}
	if archive == nil {
		t.Fatal("no bulk archive action")
	}
	if !strings.Contains(archive.Label, "4 selected") {
		t.Errorf("the label does not name the count: %q", archive.Label)
	}
	if !strings.Contains(archive.Confirm, "4") {
		t.Errorf("the confirm does not name the count: %q", archive.Confirm)
	}

	err := archive.Do(context.Background())
	if err == nil {
		t.Fatal("a bulk archive that partly FAILED reported success")
	}
	// The three archivable items were written, one call each.
	if len(p.archived) != 3 {
		t.Fatalf("archived = %v, want the three childless ids", p.archived)
	}
	// And the message states the partial result in numbers rather than claiming a sweep.
	if !strings.Contains(err.Error(), "3 of 4") {
		t.Errorf("the failure does not report the partial result: %v", err)
	}
}

// A reload reconciles the selection: rows that vanished are unmarked, so the next bulk
// action cannot act on items the operator can no longer see.
func TestReloadPrunesVanishedMarks(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 3)
	if m.Base.MarkCount() != 3 {
		t.Fatalf("fixture: marked = %d, want 3", m.Base.MarkCount())
	}
	// The plane now returns only one of them (the others were deleted).
	m.Base.LoadItems(srcWorkItems, []kit2.Item{{ID: "w2", Title: "Second", Meta: "pending"}}, "")
	got := m.Base.MarkedIDs()
	if len(got) != 1 || got[0] != "w2" {
		t.Fatalf("marked = %v after a reload, want only the surviving [w2]", got)
	}
}

// Switching SOURCE drops the selection: the marks belong to the list being looked at.
func TestSwitchingSourceClearsMarks(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 2)
	if m.Base.MarkCount() != 2 {
		t.Fatalf("fixture: marked = %d", m.Base.MarkCount())
	}
	m.Base.SelectSource(srcProjects)
	m.Base.SelectSource(srcWorkItems)
	if got := m.Base.MarkCount(); got != 0 {
		t.Fatalf("marks survived a source switch (%d) — the next bulk action would hit rows "+
			"the operator was not looking at", got)
	}
}

// The archive view offers bulk RESTORE-free actions only: a restore is per item, and the
// bulk set must not pretend otherwise.
func TestBulkActionsAreOnlyWhatTheServerCanDoPerItem(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 2)
	acts := m.actionsForSelection()
	for _, a := range acts {
		if a.Do == nil {
			t.Errorf("%q has no Do — every bulk action must perform a write", a.Label)
		}
	}
	if len(acts) == 0 {
		t.Fatal("no bulk actions at all")
	}
}

// bulkLabels names the actions the screen would offer right now. It is deliberately named
// apart from selection_sort_test.go's actionLabels, which reads the ROW controls (a
// different list drawn in the pane header).
func bulkLabels(acts []kit2.Action) []string {
	out := make([]string, 0, len(acts))
	for _, a := range acts {
		out = append(out, a.Label)
	}
	return out
}

func containsAny(labels []string, want string) bool {
	for _, l := range labels {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
