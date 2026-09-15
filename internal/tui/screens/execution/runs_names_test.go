package execution

// runs_names_test.go — the runs views show NAMES, not just ids.
//
// The operator: "Workflow Runs just show IDs right now. We should also show the name of the
// workflow and the name of the work item associated with it in the view."
//
// A WorkflowRun carries workflow_id / work_item_id and no names (the proto has no name
// fields), so the TUI resolves them client-side into a cached index — the same thing the GUI's
// schedules history view does with itemsById / workflowsById.

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// namePlane returns a fake plane carrying one work item, so a resolved title is distinguishable
// from a raw id.
func namePlane() *fakePlane {
	return &fakePlane{
		items: []*apiv1.WorkItem{{Id: "wi-1", Title: "Fix the parser", ProjectId: "prj-1",
			Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK, Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING}},
	}
}

// The LIST title carries both names.
func TestRunsListShowsWorkflowAndItemNames(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC (Human)"}}
	p.runs = []*apiv1.WorkflowRun{{
		Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1",
		Status: apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING,
	}}
	m := newModel(t, p)

	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchRuns: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	title := items[0].Title
	for _, want := range []string{"SDLC (Human)", "Fix the parser"} {
		if !strings.Contains(title, want) {
			t.Errorf("the run title %q does not carry %q — it must name the workflow AND the work item", title, want)
		}
	}
	// The id is still the row's identity (selection, jump-to-run, and the detail fetch all
	// key on it), even though the TITLE no longer shows it.
	if items[0].ID != "run-1" {
		t.Errorf("row id = %q, want run-1", items[0].ID)
	}
	// The status is still the dim right-hand context.
	if items[0].Meta != "workflow_run_status_running" {
		t.Errorf("meta = %q, want the status", items[0].Meta)
	}
}

// A run with NO bound item is a ONE-SHOT, and says so rather than showing a bare workflow name
// that looks truncated.
func TestRunsListMarksAOneShotRun(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "Quick Work"}}
	p.runs = []*apiv1.WorkflowRun{{Id: "run-1", WorkflowId: "wf-1"}}
	m := newModel(t, p)

	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchRuns: %v", err)
	}
	if !strings.Contains(items[0].Title, "Quick Work") {
		t.Errorf("title = %q, want the workflow name", items[0].Title)
	}
	if !strings.Contains(items[0].Title, "one-shot") {
		t.Errorf("title = %q — a run with no bound item must say it is a one-shot", items[0].Title)
	}
}

// An unresolvable name falls back to the ID: the pane never gets WORSE than it was before
// names were resolved, and it never renders an empty title.
func TestRunsListFallsBackToIDsWhenNamesAreUnknown(t *testing.T) {
	p := namePlane()
	// The workflow is NOT in the plane's list (deleted, or outside the index page) and the
	// item id is unknown.
	p.workflows = nil
	p.runs = []*apiv1.WorkflowRun{{Id: "run-1", WorkflowId: "wf-gone", WorkItemId: "wi-gone"}}
	m := newModel(t, p)

	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchRuns: %v", err)
	}
	if !strings.Contains(items[0].Title, "wf-gone") {
		t.Errorf("title = %q, want the workflow ID as the fallback", items[0].Title)
	}
	if !strings.Contains(items[0].Title, "wi-gone") {
		t.Errorf("title = %q, want the work item ID as the fallback", items[0].Title)
	}
	if strings.TrimSpace(items[0].Title) == "" {
		t.Error("the title is empty — a row must always render something")
	}
}

// The DETAIL carries the same names, with the id kept alongside so the row stays traceable to
// what the plane uses.
func TestRunDetailShowsNamesAndKeepsIDs(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC (Human)"}}
	p.run = &apiv1.WorkflowRun{
		Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1",
		Status:    apiv1.WorkflowRunStatus_WORKFLOW_RUN_STATUS_RUNNING,
		StartedAt: timestamppb.Now(),
	}
	m := newModel(t, p)
	m.Base.SelectSource(srcRuns)

	_, fields, _, err := m.detail(context.Background(), srcRuns, "run-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	got := map[string]string{}
	for _, f := range fields {
		got[f.Key] = f.Value
	}
	if !strings.Contains(got["workflow"], "SDLC (Human)") {
		t.Errorf("workflow field = %q, want the name", got["workflow"])
	}
	if !strings.Contains(got["workflow"], "wf-1") {
		t.Errorf("workflow field = %q, want the id kept for traceability", got["workflow"])
	}
	if !strings.Contains(got["work item"], "Fix the parser") {
		t.Errorf("work item field = %q, want the title", got["work item"])
	}
	if !strings.Contains(got["work item"], "wi-1") {
		t.Errorf("work item field = %q, want the id kept for traceability", got["work item"])
	}
}

// A one-shot run's detail says so rather than leaving the field blank.
func TestRunDetailMarksAOneShot(t *testing.T) {
	p := &fakePlane{}
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "Quick Work"}}
	p.run = &apiv1.WorkflowRun{Id: "run-1", WorkflowId: "wf-1"}
	m := newModel(t, p)
	m.Base.SelectSource(srcRuns)

	_, fields, _, err := m.detail(context.Background(), srcRuns, "run-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	for _, f := range fields {
		if f.Key == "work item" {
			if !strings.Contains(f.Value, "one-shot") {
				t.Errorf("work item field = %q, want it to name the one-shot shape", f.Value)
			}
			return
		}
	}
	t.Fatal("the detail has no work item field")
}

// A FAILED name lookup must not break the runs pane: the names are a display nicety, so the
// list still renders with ids rather than erroring.
func TestFailedNameLookupDoesNotBreakTheRunsPane(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}}
	p.runs = []*apiv1.WorkflowRun{{Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1"}}
	// The work-item list fails; the workflow list succeeds.
	p.failWorkItemList = connect.NewError(connect.CodeInternal, context.DeadlineExceeded)
	m := newModel(t, p)

	items, _, err := m.fetchRuns(context.Background(), "")
	if err != nil {
		t.Fatalf("a failed NAME lookup must not fail the runs fetch: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want the run row anyway", len(items))
	}
	// The workflow name still landed (the halves are independent).
	if !strings.Contains(items[0].Title, "SDLC") {
		t.Errorf("title = %q, want the workflow name that DID resolve", items[0].Title)
	}
	// And the item fell back to its id.
	if !strings.Contains(items[0].Title, "wi-1") {
		t.Errorf("title = %q, want the item id as the fallback", items[0].Title)
	}
}

// The index is CACHED: a second fetch within the TTL must not re-issue the name lookups.
func TestNameIndexIsCached(t *testing.T) {
	p := namePlane()
	p.workflows = []*apiv1.Workflow{{Id: "wf-1", Name: "SDLC"}}
	p.runs = []*apiv1.WorkflowRun{{Id: "run-1", WorkflowId: "wf-1", WorkItemId: "wi-1"}}
	m := newModel(t, p)

	if _, _, err := m.fetchRuns(context.Background(), ""); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	after := p.workflowListCalls
	if after == 0 {
		t.Fatal("fixture: the name index never loaded")
	}
	if _, _, err := m.fetchRuns(context.Background(), ""); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if p.workflowListCalls != after {
		t.Errorf("the second fetch re-issued the name lookups (%d -> %d) — the index is meant "+
			"to be cached within its TTL", after, p.workflowListCalls)
	}
}
