package work

// prindex_test.go — THE PR SURFACE: the list mark, the details URL, and the rules between them.
//
// The operator: "Work items in the TUI need to have some kind of status mark on them showing if a PR was merged
// similar to the GUI. I don't really care about viewing the priority levels in the work item list. Let's remove
// the priority level in list and put PR Merged if a PR has merged and add a field at the top of the details pane
// that shows the URL." — and, on the multi-run case: "For PR I want the PR Merged if any has it."

import (
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// --- the index rules, without a model -------------------------------------------------------

// ANY merged run marks the item, even when the MOST RECENT run did not merge.
//
// This is the operator's rule and the reason the index is a list rather than a "latest" value: a re-run after a
// failed PR leaves the earlier merge standing, and an indicator that went dark on the next run would be lying
// about work that had landed.
func TestPRIndexMergedIsTrueWhenAnyRunMerged(t *testing.T) {
	idx := prIndex{
		"wi-1": []prRun{
			{url: "https://example.test/pr/2", state: "open"},   // newest: a follow-up, not merged
			{url: "https://example.test/pr/1", state: "merged"}, // older: the change that landed
		},
	}
	if !idx.merged("wi-1") {
		t.Error("an older merged run must mark the item — the operator asked for 'if ANY has it', and a re-run " +
			"must not erase a merge that already happened")
	}
	if idx.merged("wi-absent") {
		t.Error("an item with no runs must not be marked merged")
	}
}

// NOT MERGED IS NOT MARKED. An open or closed PR is not a landed change, and the mark is the one signal the list
// gives — so it must mean exactly what it says.
func TestPRIndexMergedIgnoresUnmergedStates(t *testing.T) {
	for _, state := range []string{"open", "draft", "closed", ""} {
		idx := prIndex{"wi-1": []prRun{{url: "https://example.test/pr/1", state: state}}}
		if idx.merged("wi-1") {
			t.Errorf("state %q must not render as PR merged", state)
		}
	}
}

// THE DRESS LINK IS THE MERGED ONE when a merged run exists, and the newest otherwise. An operator opening the
// pane is looking for the change that LANDED in the first case, and for whatever PR exists in the second.
func TestPRIndexLinkPrefersTheMergedRun(t *testing.T) {
	idx := prIndex{
		"wi-1": []prRun{
			{url: "https://example.test/pr/2", state: "open"},
			{url: "https://example.test/pr/1", state: "merged"},
		},
	}
	if url, state := idx.link("wi-1"); url != "https://example.test/pr/1" || state != "merged" {
		t.Fatalf("link = (%q, %q), want the merged run's PR", url, state)
	}

	// No merge: fall back to the newest run that has a PR at all.
	open := prIndex{
		"wi-2": []prRun{
			{url: "https://example.test/pr/9", state: "open"},
			{url: "https://example.test/pr/8", state: "closed"},
		},
	}
	if url, state := open.link("wi-2"); url != "https://example.test/pr/9" || state != "open" {
		t.Fatalf("link = (%q, %q), want the newest run's PR", url, state)
	}

	// Nothing on record: empty, so the caller renders NO field.
	var none prIndex
	if url, state := none.link("wi-3"); url != "" || state != "" {
		t.Fatalf("a missing PR must resolve empty, got (%q, %q)", url, state)
	}
}

// The state rides beside the URL, because a bare link does not say whether the change landed.
func TestPRDisplayCarriesTheState(t *testing.T) {
	if got := prDisplay("https://example.test/pr/1", "merged"); got != "https://example.test/pr/1 (merged)" {
		t.Errorf("prDisplay = %q", got)
	}
	// An absent or meaningless state must not render empty parentheses.
	for _, state := range []string{"", "none"} {
		if got := prDisplay("https://example.test/pr/1", state); got != "https://example.test/pr/1" {
			t.Errorf("prDisplay with state %q = %q, want the bare URL", state, got)
		}
	}
}

// --- the rendered surface --------------------------------------------------------------------

// mergedPlane is detailPlane plus one merged run for its child item.
func mergedPlane(t *testing.T) (*fakePlane, *Model) {
	t.Helper()
	p, m := detailPlane(t)
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-1", TaskId: "wi-child", PrUrl: "https://example.test/pr/1", PrState: "merged",
		Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
	})
	return p, m
}

// THE LIST ROW SAYS PR MERGED, AND THE PRIORITY TOKEN IS GONE.
func TestTreeRowShowsPRMergedInsteadOfPriority(t *testing.T) {
	p := newPlane()
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Shipped", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_SUCCEEDED, Priority: 3,
	})
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-1", TaskId: "wi-1", PrUrl: "https://example.test/pr/1", PrState: "merged",
		Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	row := metaFor(t, m, "wi-1")
	if !strings.Contains(row, "PR merged") {
		t.Fatalf("the row does not say PR merged: %q", row)
	}
	if strings.Contains(row, "p3") {
		t.Fatalf("the row still shows priority (%q) — the operator traded the priority token for the PR mark", row)
	}
	if !strings.Contains(row, "succeeded") {
		t.Fatalf("the row lost its state pill: %q", row)
	}
}

// AN UNMERGED (OR ABSENT) PR LEAVES THE ROW ALONE — no mark, no placeholder.
func TestTreeRowWithoutAMergedPRHasNoMark(t *testing.T) {
	p := newPlane()
	p.addItem(&apiv1.WorkItem{
		Id: "wi-open", Title: "Open PR", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
	})
	p.addItem(&apiv1.WorkItem{
		Id: "wi-none", Title: "No PR", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING,
	})
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-1", TaskId: "wi-open", PrUrl: "https://example.test/pr/2", PrState: "open",
		Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	for _, id := range []string{"wi-open", "wi-none"} {
		if row := metaFor(t, m, id); strings.Contains(row, "PR merged") {
			t.Errorf("row %s claims a merge it does not have: %q", id, row)
		}
	}
}

// A RE-RUN THAT DID NOT MERGE MUST NOT ERASE AN EARLIER MERGE — the operator's "if any has it", on the rendered
// row rather than in the index.
func TestTreeRowKeepsTheMarkAfterALaterUnmergedRun(t *testing.T) {
	p := newPlane()
	p.addItem(&apiv1.WorkItem{
		Id: "wi-1", Title: "Rerun", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_RUNNING,
	})
	// Newest first, as the API returns them: an open follow-up on top of the merge that landed.
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-2", TaskId: "wi-1", PrUrl: "https://example.test/pr/2", PrState: "open",
		Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
	})
	p.addExec(&apiv1.WorkerExecution{
		Id: "exec-1", TaskId: "wi-1", PrUrl: "https://example.test/pr/1", PrState: "merged",
		Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
	})
	m := newModel(t, p)
	m.SelectSource(srcWorkItems)
	load(t, m, srcWorkItems)

	if row := metaFor(t, m, "wi-1"); !strings.Contains(row, "PR merged") {
		t.Fatalf("a later unmerged run erased an earlier merge: %q", row)
	}
}

// THE DETAILS PANE CARRIES THE URL, IN ITS TOP BLOCK, ABOVE THE IDENTIFIER FIELDS.
func TestDetailShowsThePRURLAtTheTop(t *testing.T) {
	_, m := mergedPlane(t)
	load(t, m, srcWorkItems)
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))

	got := fieldValue(t, m, "pr url")
	if !strings.HasPrefix(got, "https://example.test/pr/1") {
		t.Fatalf("pr url = %q, want the merged run's URL", got)
	}
	// The top block is id/title/kind/status, and the URL belongs in it — before `parent`, which is the first
	// field of the identifier block.
	_, fields, _ := m.Base.DetailForTest()
	var prAt, parentAt = -1, -1
	for i, f := range fields {
		switch f.Key {
		case "pr url":
			prAt = i
		case "parent":
			parentAt = i
		}
	}
	if prAt == -1 || parentAt == -1 {
		t.Fatalf("expected both fields; pr=%d parent=%d", prAt, parentAt)
	}
	if prAt > parentAt {
		t.Fatalf("pr url is at %d, below parent at %d — the operator asked for it at the TOP of the pane", prAt, parentAt)
	}
	if prAt > 4 {
		t.Fatalf("pr url is at position %d, below the identity block", prAt)
	}
}

// NO PR ON RECORD IS NO FIELD: never a blank row, never an invented value.
func TestDetailOmitsThePRURLWhenThereIsNoPR(t *testing.T) {
	_, m := detailPlane(t) // no executions seeded at all
	load(t, m, srcWorkItems)
	run(t, m, m.Base.RequestDetail(srcWorkItems, "wi-child"))

	_, fields, _ := m.Base.DetailForTest()
	for _, f := range fields {
		if f.Key == "pr url" {
			t.Fatalf("an item with no PR rendered a pr url field: %q", f.Value)
		}
	}
}
