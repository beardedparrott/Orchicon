package work

// detail_names_test.go — Concern 2: the work-item DETAIL pane resolves the identifiers it carries
// into the NAMES an operator reads, with the raw id retained beside the name and the RAW ID as the
// fallback when nothing resolves.
//
// The operator: "in the TUI under work items, it is showing all guids in the display instead of the
// actual names. That won't mean anything to anyone. We need to resolve the real names in the cases
// where they have them."
//
// These tests drive the REAL detail fetch (Base.RequestDetail → the screen's DetailFn) and read what
// the pane would DRAW — asserting on the rendered field values, not on the index internals.

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// detailPlane is one project, one workflow-bound item and one parent/child pair — the smallest plane
// whose detail pane carries all five identifiers.
func detailPlane(t *testing.T) (*fakePlane, *Model) {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{
		Id: "wi-epic", Title: "Epic", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_EPIC,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1,
	})
	p.addItem(&apiv1.WorkItem{
		Id: "wi-child", Title: "Child", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", ParentId: "wi-epic", WorkflowId: "wf-1",
		WorkflowRunId: "01M0NAYG0PKJ7EB7ZKNAQSMF9T", AssignedWorkerRef: "w-3",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 1,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	return p, m
}

// fieldValue reads one rendered detail field back ("" when the field is absent).
func fieldValue(t *testing.T, m *Model, key string) string {
	t.Helper()
	_, fields, _ := m.Base.DetailForTest()
	for _, f := range fields {
		if f.Key == key {
			return f.Value
		}
	}
	t.Fatalf("the detail pane has no %q field", key)
	return ""
}

// The two identifiers whose names are ALREADY on the Model resolve with NO new request: the
// workflow from the form prep's workflow list, the project from the Projects page.
func TestDetailResolvesWorkflowAndProjectNames(t *testing.T) {
	p, m := detailPlane(t)
	load(t, m, srcWorkItems)

	// Both option lists land with a form prep — the same lists the pickers are built from.
	run(t, m, press(t, m, "n"))
	if m.ActiveForm() == nil {
		t.Fatal("n must open the create form")
	}
	press(t, m, "esc")

	before := p.listCallCount()
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))

	if got, want := fieldValue(t, m, "workflow"), "Fanout  (wf-1)"; got != want {
		t.Fatalf("workflow field = %q, want %q (the NAME, with the id kept beside it)", got, want)
	}
	if got, want := fieldValue(t, m, "project"), "Orchicon  (proj-1)"; got != want {
		t.Fatalf("project field = %q, want %q", got, want)
	}
	// No per-row RPC: rendering the pane asked the plane for NOTHING new.
	if got := p.listCallCount(); got != before {
		t.Fatalf("resolving the detail issued %d list request(s); it must use what the screen already holds", got-before)
	}
}

// `parent` resolves from the titles of the ALREADY-LOADED page, and falls back to the raw id when the
// parent is not on it.
func TestDetailResolvesParentFromTheLoadedPage(t *testing.T) {
	p, m := detailPlane(t)
	p.addItem(&apiv1.WorkItem{
		Id: "wi-orphan", Title: "Orphan", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", ParentId: "wi-long-gone",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 2,
	})
	load(t, m, srcWorkItems) // the page carries wi-epic's TITLE

	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))
	if got, want := fieldValue(t, m, "parent"), "Epic  (wi-epic)"; got != want {
		t.Fatalf("parent field = %q, want %q", got, want)
	}

	// A parent that is NOT on the loaded page renders as the RAW ID — not a placeholder, not blank.
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-orphan"))
	if got := fieldValue(t, m, "parent"); got != "wi-long-gone" {
		t.Fatalf("an off-page parent must render its raw id, got %q", got)
	}
}

// AC4: every unresolved identifier renders as its RAW ID — never "unknown", never empty, never a
// made-up name. This is the plane with NOTHING loaded: no page, no project list, no workflow list.
func TestDetailFallsBackToRawIds(t *testing.T) {
	p := newPlane()
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Unresolvable", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-missing", ParentId: "wi-missing", WorkflowId: "wf-missing",
		WorkflowRunId: "01M0NAYG0PKJ7EB7ZKNAQSMF9T", AssignedWorkerRef: "w-9",
		Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	// NO load, NO form prep: every index is empty.
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-1"))

	for _, tc := range []struct{ key, want string }{
		{"project", "proj-missing"},
		{"parent", "wi-missing"},
		{"workflow", "wf-missing"},
		{"worker", "w-9"},
	} {
		got := fieldValue(t, m, tc.key)
		if got != tc.want {
			t.Fatalf("%s = %q, want the raw id %q", tc.key, got, tc.want)
		}
		if got == "" {
			t.Fatalf("%s must never render empty", tc.key)
		}
		if strings.Contains(strings.ToLower(got), "unknown") || strings.Contains(got, "(") {
			t.Fatalf("%s = %q: an unresolved id is neither a placeholder nor a name+id pair", tc.key, got)
		}
	}
}

// A workflow RUN has no name of its own, so it gets the GUI's rendering — a shortened id rather than a
// bare 26-character ULID — and an item with no run yet renders nothing at all.
func TestDetailShortensTheWorkflowRunID(t *testing.T) {
	p, m := detailPlane(t)
	ulid := "01M0NAYG0PKJ7EB7ZKNAQSMF9T"
	load(t, m, srcWorkItems)

	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))
	got := fieldValue(t, m, "workflow run")
	if want := screenkit.TruncateRunes(ulid, 13); got != want {
		t.Fatalf("workflow run = %q, want %q (the GUI's shortening)", got, want)
	}
	if got == ulid {
		t.Fatal("the run field must not print the bare 26-character id")
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a shortened run id must announce the cut, got %q", got)
	}

	// No run yet → no run shown. "…" would be an invented value.
	p.addItem(&apiv1.WorkItem{
		Id: "wi-norun", Title: "Not running", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING, SortOrder: 2,
	})
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-norun"))
	if got := fieldValue(t, m, "workflow run"); got != "" {
		t.Fatalf("an item with no run must render an empty run field, got %q", got)
	}
}

// The worker ref is a REF, not a resolvable name: there is no worker list on this Model, and the value
// is what the operator quotes. It must survive verbatim.
func TestDetailKeepsTheWorkerRef(t *testing.T) {
	_, m := detailPlane(t)
	load(t, m, srcWorkItems)

	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))
	if got := fieldValue(t, m, "worker"); got != "w-3" {
		t.Fatalf("worker field = %q, want the ref verbatim", got)
	}
}

// The WORKFLOW name must resolve on a COLD screen — from the list fetch alone, with NO form ever
// opened. The detail pane is drawn the moment the items land, so a name that only exists after a
// form prep is a name the operator does not have in the flow they actually use (open the tab, look
// at the pane). The GUI resolves it from the workflow list its page loads on entry; this is the
// TUI's parity half, and it must stay ONE cached fetch — never one per row.
func TestDetailResolvesWorkflowNameOnAColdScreen(t *testing.T) {
	p, m := detailPlane(t)
	// Exactly the shell's own screen load: page 1 of every source, nothing else. No 'n', no 'e'.
	load(t, m, srcProjects)
	load(t, m, srcWorkItems)

	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))
	if got, want := fieldValue(t, m, "workflow"), "Fanout  (wf-1)"; got != want {
		t.Fatalf("workflow field = %q, want %q — the name must resolve with no form opened", got, want)
	}
	if got, want := fieldValue(t, m, "project"), "Orchicon  (proj-1)"; got != want {
		t.Fatalf("project field = %q, want %q", got, want)
	}

	// Re-reading the pane, and reloading the list, must not multiply the name lookup: the index is
	// TTL-cached, so a screen that renders N rows still pays for AT MOST one workflow list.
	for i := 0; i < 3; i++ {
		run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))
	}
	load(t, m, srcWorkItems)
	if got := p.wfListCallCount(); got != 1 {
		t.Fatalf("ListWorkflows calls = %d, want 1 (one cached fetch, never per row)", got)
	}
}
