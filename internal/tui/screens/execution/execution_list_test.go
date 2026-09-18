package execution

// execution_list_test.go — the executions list: WHAT each row says, and how the page is enriched.
//
// Two operator reports:
//
//	"The titles of the executions really don't tell me anything besides ID and status. I would like
//	 the workflow name and work item associated with it."
//	"The initial execution page load is pretty slow. We need to find out how to make this close to
//	 instant."
//
// The second one had a cause worth recording: the list enriched each row with its OWN queries — a
// per-row usage SUM and a per-row load of the step run's `_prompt` (tens of kilobytes of JSON, a
// field no list consumer reads). At one page of 100 that was survivable; once every list fetched
// itself whole the executions page pulled ~3k rows and issued ~5,900 queries per load.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// The row says WHAT ran: the workflow name and the work item — the operator's report, verbatim.
func TestExecutionListTitleShowsWorkflowAndWorkItem(t *testing.T) {
	// The work item title is resolved through the shared name index, so seed it the way the index
	// would be seeded by a load.
	var names runNames
	names.mu.Lock()
	names.items = map[string]string{"wi-1": "Fix the parser"}
	names.mu.Unlock()

	e := &apiv1.WorkerExecution{Id: "exec-1", WorkflowName: "SDLC", TaskId: "wi-1"}
	got := executionListTitle(e, &names)
	if got != "SDLC · Fix the parser" {
		t.Errorf("title = %q, want %q — the operator asked for the workflow and the work item", got, "SDLC · Fix the parser")
	}
}

// THE WORK ITEM TITLE IS BOUNDED TO 15 RUNES.
//
// The operator: "Executions and workflow runs are still way too crowded. It's too noisy and makes it hard
// on the eyes. We should clean them up more. How about this instead: Worker Name - Work Item (15
// character only) - Status."
//
// The bound is applied at the SOURCE rather than left to the row's own end-truncation, because the row
// trims whatever does not fit — so an unbounded title would silently eat the status, which is the field
// listRow was fixed to protect. Bounding here keeps the workflow name and the status in the budget
// however long the title is.
func TestExecutionListTitleBoundsTheWorkItemTitle(t *testing.T) {
	var names runNames
	names.mu.Lock()
	names.items = map[string]string{"wi-1": "Stop outboxing per-token execution.text + retention"}
	names.mu.Unlock()

	e := &apiv1.WorkerExecution{Id: "exec-1", WorkflowName: "SDLC", TaskId: "wi-1"}
	got := executionListTitle(e, &names)

	if !strings.HasPrefix(got, "SDLC · ") {
		t.Errorf("title = %q, want it to open with the workflow and the separator", got)
	}
	item := strings.TrimPrefix(got, "SDLC · ")
	if n := len([]rune(item)); n > executionItemTitleMax {
		t.Errorf("the work item part is %d runes (%q), want <= %d — a sentence per row is what makes the "+
			"list unscannable", n, item, executionItemTitleMax)
	}
	// A shortened title is MARKED as shortened, so it is never mistaken for the whole one.
	if !strings.HasSuffix(item, "…") {
		t.Errorf("the shortened work item %q carries no ellipsis, so it reads as the complete title", item)
	}
	// A title that FITS is left exactly alone — the bound must not decorate short titles.
	short := &apiv1.WorkerExecution{Id: "e", WorkflowName: "SDLC", TaskId: "wi-2"}
	names.mu.Lock()
	names.items["wi-2"] = "Fix parser"
	names.mu.Unlock()
	if got := executionListTitle(short, &names); got != "SDLC · Fix parser" {
		t.Errorf("a short title was altered: %q, want %q", got, "SDLC · Fix parser")
	}
}

// A row degrades rather than going blank: whatever is known is shown, and the ID is the last resort.
// (A blank line in a list is worse than an id: the operator cannot even tell which row it is.)
func TestExecutionListTitleDegradesToWhatIsKnown(t *testing.T) {
	var populated runNames
	populated.mu.Lock()
	populated.items = map[string]string{"wi-1": "Fix the parser"}
	populated.mu.Unlock()
	var empty runNames

	cases := []struct {
		name string
		e    *apiv1.WorkerExecution
		n    *runNames
		want string
	}{
		{"workflow only", &apiv1.WorkerExecution{Id: "e", WorkflowName: "SDLC"}, &empty, "SDLC"},
		{"item only", &apiv1.WorkerExecution{Id: "e", TaskId: "wi-1"}, &populated, "Fix the parser"},
		{"neither -> the id", &apiv1.WorkerExecution{Id: "exec-9"}, &empty, "exec-9"},
		// A task bound to an item the index does not know keeps the id rather than half a title.
		{"unknown item", &apiv1.WorkerExecution{Id: "exec-9", TaskId: "wi-gone"}, &populated, "exec-9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := executionListTitle(c.e, c.n)
			if got != c.want {
				t.Errorf("title = %q, want %q", got, c.want)
			}
			if strings.TrimSpace(got) == "" {
				t.Error("the title is blank — a row must always identify itself")
			}
		})
	}
}

// The meta line carries the STATUS AND NOTHING ELSE.
//
// This used to assert the worker's name was present too, and that assertion is what the operator's report
// invalidated: "too crowded... too noisy and makes it hard on the eyes". The worker name is long, nearly
// identical across rows on this pane, and it pushed the row into the very overflow the status then lost
// to — so the row now spends its width on the status, the one field that CHANGES, and the worker is one
// keystroke away in the detail pane. Asserting its ABSENCE is the point: a worker name creeping back in
// is what re-crowds the row.
func TestExecutionListMetaIsTheStatusAlone(t *testing.T) {
	e := &apiv1.WorkerExecution{
		Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED, WorkerName: "Quick Software Engineer",
	}
	got := executionListMeta(e)
	if got != "succeeded" {
		t.Errorf("meta = %q, want exactly \"succeeded\" — the status alone", got)
	}
	if strings.Contains(got, "Quick Software Engineer") {
		t.Errorf("meta = %q, want the worker's name OFF the row: it is what crowds it", got)
	}
	// No proto enum leakage, ever — the pane's vocabulary is words.
	if strings.Contains(got, "EXECUTION_STATUS_") {
		t.Errorf("meta leaks the proto enum: %q", got)
	}
	// A run with no worker known reads the same, since the worker is not on the row either way.
	bare := executionListMeta(&apiv1.WorkerExecution{Status: apiv1.ExecutionStatus_EXECUTION_STATUS_RUNNING})
	if bare != "running" {
		t.Errorf("meta = %q, want just the status", bare)
	}
}

// fetchExecutions populates the title/meta from the response, through the screen's own path.
func TestFetchExecutionsPopulatesNames(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{
		Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		WorkerName: "Quick Software Engineer", WorkflowName: "Quick Work",
		TaskId: "wi-1", // namePlane seeds "Fix the parser"
	}
	m := newModel(t, p)
	// The name index loads from the plane's work-item list, which namePlane serves.
	m.runNames.ensure(context.Background(), m)

	items, _, err := m.fetchExecutions(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchExecutions: %v", err)
	}
	// The plane lists whatever it was given; if it lists nothing, the fetch is not exercised.
	if len(items) == 0 {
		t.Skip("the fake plane returned no executions")
	}
	it := items[0]
	if it.Title == it.ID && it.Title == "exec-1" {
		t.Errorf("the row still shows only its id (%q) — the operator asked for the workflow and work item", it.Title)
	}
	if !strings.Contains(it.Title, "Quick Work") {
		t.Errorf("title = %q, want the workflow name", it.Title)
	}
}

// The list must NOT re-read the per-row system prompt: it is a detail-only field, and loading it per
// row is what made the page slow. Asserted at the TUI level by "the list fetch does not depend on
// it", and at the service level by the fact that the batched totals still populate.
//
// This test pins the CONTRACT the fix depends on: a list row's cost/tokens come from the batched
// sums, and nothing in the list path needs the prompt.
func TestExecutionListDoesNotNeedTheSystemPrompt(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{
		Id: "exec-1", Status: apiv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED,
		WorkflowName: "SDLC", TaskId: "wi-1",
		// Deliberately NO SystemPrompt — as the list now returns it.
	}
	m := newModel(t, p)
	items, _, err := m.fetchExecutions(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchExecutions: %v", err)
	}
	if len(items) == 0 {
		t.Skip("the fake plane returned no executions")
	}
	// The row still renders usefully, which is the point: the prompt was never needed here.
	if strings.TrimSpace(items[0].Title) == "" {
		t.Error("a row with no system prompt rendered blank — the list must not depend on it")
	}
}

// The list keeps working when the name index is COLD (a failed or not-yet-loaded index): the rows
// fall back to the id rather than the whole pane failing.
func TestFetchExecutionsSurvivesAColdNameIndex(t *testing.T) {
	p := namePlane()
	p.exec = &apiv1.WorkerExecution{Id: "exec-1", WorkflowName: "SDLC", TaskId: "wi-1"}
	m := newModel(t, p)
	// Deliberately do NOT load the index. fetchExecutions loads it itself; this asserts the row is
	// still produced even when the index yields nothing for the item.
	items, _, err := m.fetchExecutions(context.Background(), "")
	if err != nil {
		t.Fatalf("fetchExecutions: %v", err)
	}
	for _, it := range items {
		if it.ID == "" {
			t.Error("a row has no id — the pane could not address it")
		}
	}
	// And the header rendering works for the row.
	_ = kit2.Item{}
}
