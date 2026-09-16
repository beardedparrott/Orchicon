package execution

// execution_context_test.go — the CONTEXT / token / cost panel, and the run's AUTHORED step order.
//
// These pin two operator reports:
//
//	"Also there is no context, token, or cost information being shown."
//	"I think it is showing the results in reverse order" — the run's steps.
//
// The ordering fixtures use the REAL SDLC workflow the report was filed against, read out of the
// live database: steps step-sse, step-parallel, step-branch-a, step-branch-b, step-loop with their
// real depends_on chain. That matters, because the defect only shows up on ids whose alphabetical
// order differs from their authored order — a fixture with steps named a/b/c would have passed
// against the broken sort.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// --- the run's step order ------------------------------------------------------------------------

// The live SDLC version's steps JSON, verbatim in shape (ids, names and depends_on as stored).
const sdlcStepsJSON = `[
  {"id":"step-sse","name":"Senior Software Engineer","kind":"task","depends_on":null},
  {"id":"step-parallel","name":"Parallel","kind":"parallel","depends_on":["step-sse"]},
  {"id":"step-branch-a","name":"PR Reviewer","kind":"task","depends_on":["step-parallel"]},
  {"id":"step-branch-b","name":"QA Engineer","kind":"task","depends_on":["step-parallel"]},
  {"id":"step-loop","name":"Loop Decision","kind":"loop_decision","depends_on":["step-branch-a","step-branch-b"]}
]`

// The steps come back in the AUTHORED order, not alphabetical by id.
//
// The report: "It is showing the PR Reviewer and QA Engineer as first in the list." Sorting ids
// gives branch-a, branch-b, loop, parallel, sse — PR Reviewer, QA Engineer, Loop Decision, Parallel,
// Senior Software Engineer — which IS the report, and is not an order in which the steps run.
func TestRunStepsFollowTheWorkflowDefinitionOrder(t *testing.T) {
	// The rows as the server returns them: created_at ASC, id ASC, which for one shared
	// created_at is id order — the same defect, so the test cannot pass by accident.
	runs := []*apiv1.WorkflowStepRun{
		stepRun("sr-a", "step-branch-a", "PR Reviewer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED, 0, 0, "e-a"),
		stepRun("sr-b", "step-branch-b", "QA Engineer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_FAILED, 0, 0, "e-b"),
		stepRun("sr-l", "step-loop", "Loop Decision", apiv1.StepKind_STEP_KIND_LOOP_DECISION, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
		stepRun("sr-p", "step-parallel", "Parallel", apiv1.StepKind_STEP_KIND_PARALLEL, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
		stepRun("sr-s", "step-sse", "Senior Software Engineer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
	}

	rows := runStepRows(runs, authoredStepOrder(sdlcStepsJSON))

	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.name)
	}
	want := []string{"Senior Software Engineer", "Parallel", "PR Reviewer", "QA Engineer", "Loop Decision"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("step order = %v\nwant (authored) %v", got, want)
	}
	// And the SPECIFIC claim in the report: the reviewers must not lead.
	if got[0] == "PR Reviewer" || got[0] == "QA Engineer" {
		t.Errorf("the list still leads with a reviewer: %v", got)
	}
}

// A step the definition does not mention is appended, never dropped — a renderer must not hide a
// row because the version changed underneath the run.
func TestRunStepsKeepAStepTheDefinitionDoesNotName(t *testing.T) {
	runs := []*apiv1.WorkflowStepRun{
		stepRun("sr-1", "step-sse", "SSE", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
		stepRun("sr-2", "step-retired", "A Retired Step", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
	}
	rows := runStepRows(runs, []string{"step-sse"})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the unnamed step kept", len(rows))
	}
	if rows[0].name != "SSE" {
		t.Errorf("the authored step must come first, got %q", rows[0].name)
	}
	if rows[1].name != "A Retired Step" {
		t.Errorf("the unnamed step must be appended, got %q", rows[1].name)
	}
}

// With NO authored order (the version could not be read), the run's own arrival order stands — the
// flow still renders, it just loses the definition's order. It must NOT fall back to sorting ids.
func TestRunStepsFallBackToArrivalOrderNotIDSort(t *testing.T) {
	runs := []*apiv1.WorkflowStepRun{
		stepRun("sr-1", "step-sse", "SSE", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
		stepRun("sr-2", "step-branch-a", "PR Reviewer", apiv1.StepKind_STEP_KIND_TASK, apiv1.StepRunStatus_STEP_RUN_STATUS_SUCCEEDED, 0, 0, ""),
	}
	rows := runStepRows(runs, nil)
	if rows[0].name != "SSE" {
		t.Errorf("the fallback must keep the server's arrival order, got %q first", rows[0].name)
	}
}

// authoredStepOrder reads the version's steps, and a malformed array yields nothing rather than an
// error (the caller falls back).
func TestAuthoredStepOrderParsesAndDegrades(t *testing.T) {
	got := authoredStepOrder(sdlcStepsJSON)
	if len(got) != 5 || got[0] != "step-sse" || got[4] != "step-loop" {
		t.Errorf("authoredStepOrder = %v, want the five steps sse→loop", got)
	}
	for _, bad := range []string{"", "not json", "{", "null"} {
		if ids := authoredStepOrder(bad); len(ids) != 0 {
			t.Errorf("authoredStepOrder(%q) = %v, want nothing", bad, ids)
		}
	}
}

// --- the context panel ---------------------------------------------------------------------------

// The context panel states the model, the share of its window, the buckets and the cost — the
// numbers the GUI's Context sidebar shows.
func TestUsagePanelShowsContextTokensAndCost(t *testing.T) {
	// The live shape from execution 01M28MA4G8X1H345RZ9N08VEPP: deepseek-flash, a peak working set
	// of ~196k, heavy cache reads, and a fraction of a cent.
	u := &execUsage{
		provider: "deepseek", model: "deepseek-flash",
		workingSet: 196332, window: 128000,
		prompt: 250000, completion: 12000, reasoning: 0, cacheRead: 800000,
		total: 1062000, costUSD: 0.0977, turns: 31,
	}
	out := renderUsage(u, 80)
	for _, want := range []string{
		"deepseek/deepseek-flash",
		"context", // the section header
		"153%",    // 196332/128000, rounded and NOT clamped to 100 — over-window is the truth
		"196.3k",  // the working set, humanised
		"in 250.0k", "out 12.0k", "cache read 800.0k", "total 1.06M",
		"31 turns",
		"$0.0977",
		"█", // the bar
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the context panel does not show %q:\n%s", want, out)
		}
	}
}

// The percentage is the PEAK working set against the window, cache EXCLUDED — not the summed total
// (which on a long run would read as thousands of percent and mean nothing).
func TestUsagePercentIsThePeakWorkingSetNotTheSum(t *testing.T) {
	u := &execUsage{workingSet: 64000, window: 128000, total: 10_000_000}
	if got := u.percent(); got != 50 {
		t.Errorf("percent = %d, want 50 (64000/128000) — the sum must NOT be the numerator", got)
	}
	// Cache reads are re-sends of already-counted tokens, so they are excluded from the numerator.
	// Built through summarizeUsage, because the working set is what THAT computes — asserting on a
	// hand-built struct would test a field assignment rather than the rule.
	withCache := summarizeUsage([]*apiv1.UsageRecord{
		{Provider: "p", Model: "m", PromptTokens: 64000, CacheReadTokens: 900000, TotalTokens: 964000},
	})
	withCache.window = 128000
	if got := withCache.percent(); got != 50 {
		t.Errorf("percent = %d, want 50 — cache reads must not inflate the working set", got)
	}
	// No window resolved: no percentage at all, rather than one guessed against nothing.
	if got := (&execUsage{workingSet: 1000}).percent(); got != -1 {
		t.Errorf("percent with no window = %d, want -1 (omit it)", got)
	}
}

// An execution the gateway recorded NOTHING for gets no panel — a block of zeroes reads as "it used
// nothing", which is a different claim from "there is no data".
func TestUsagePanelIsAbsentWithoutUsage(t *testing.T) {
	if got := renderUsage(nil, 80); got != "" {
		t.Errorf("renderUsage(nil) = %q, want nothing", got)
	}
	if got := renderUsage(&execUsage{turns: 3}, 80); got != "" {
		t.Errorf("renderUsage(no tokens, no cost) = %q, want nothing", got)
	}
	// And a summary of no records is nil, not an empty struct.
	if got := summarizeUsage(nil); got != nil {
		t.Errorf("summarizeUsage(nil) = %+v, want nil", got)
	}
}

// summarizeUsage aggregates the buckets, takes the LAST model, and takes the MAX working set.
func TestSummarizeUsageAggregatesAndTakesThePeak(t *testing.T) {
	recs := []*apiv1.UsageRecord{
		{Provider: "deepseek", Model: "old-model", PromptTokens: 100, TotalTokens: 100, CostUsd: 0.01},
		{Provider: "deepseek", Model: "deepseek-flash", PromptTokens: 196332, CompletionTokens: 898,
			CacheReadTokens: 6656, TotalTokens: 202988, CostUsd: 0.05},
	}
	u := summarizeUsage(recs)
	if u == nil {
		t.Fatal("summarizeUsage returned nil for records")
	}
	if u.workingSet != 197230 {
		t.Errorf("working set = %d, want the PEAK (196332+898)", u.workingSet)
	}
	if u.total != 203088 || u.cacheRead != 6656 {
		t.Errorf("totals = %d / cache %d, want the sums", u.total, u.cacheRead)
	}
	if u.costUSD < 0.059 || u.costUSD > 0.061 {
		t.Errorf("cost = %v, want the sum (~0.06)", u.costUSD)
	}
	if u.turns != 2 {
		t.Errorf("turns = %d, want 2", u.turns)
	}
}

// The bar is CLAMPED to its cell count, so a working set over the nominal window cannot overflow
// the pane and wrap the panel.
func TestContextBarIsClamped(t *testing.T) {
	for _, pct := range []int{0, 1, 50, 99, 100, 153, 1000} {
		bar := contextBar(pct, 80)
		// The bar is styled, so measure it stripped of escape sequences.
		if n := len([]rune(stripANSI(bar))); n != 24 {
			t.Errorf("contextBar(%d) has %d cells, want 24", pct, n)
		}
	}
	// A narrow pane truncates rather than wrapping.
	if n := len([]rune(stripANSI(contextBar(50, 10)))); n != 10 {
		t.Errorf("a narrow bar has %d cells, want 10", n)
	}
}

// The panel is part of the COMPOSED execution body, so it survives the session's repaints and the
// todo landing the same way the facts and the transcript do.
func TestUsagePanelIsComposedIntoTheDetail(t *testing.T) {
	p := execPlane()
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.DeliverFetchForTest(srcExecutions, []screenkit.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	// The RECORD lands first, the way it does in the running app (the detail fetch, which is also
	// what stores the parts the body is composed from).
	title, fields, body, err := m.detail(context.Background(), srcExecutions, "exec-1")
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	m.Base.DeliverDetailForTest(srcExecutions, "exec-1", title, body, fields)
	m.execUsage.put("exec-1", &execUsage{provider: "deepseek", model: "deepseek-flash", workingSet: 1000, window: 10000, total: 5000, costUSD: 0.02, turns: 2})

	// A repaint from cache is what the usage landing does.
	m.repaintExecutionDetail()
	_, _, withPanel := m.Base.DetailForTest()
	if !strings.Contains(withPanel, "deepseek/deepseek-flash") {
		t.Errorf("the context panel is not in the pane body:\n%s", withPanel)
	}
	// A session repaint must not remove it.
	m.RenderSession([]chat.ChatItem{{Kind: chat.KindText, Text: "hello"}})
	_, _, after := m.Base.DetailForTest()
	if !strings.Contains(after, "deepseek/deepseek-flash") {
		t.Errorf("a session repaint dropped the context panel:\n%s", after)
	}
	if !strings.Contains(after, "hello") {
		t.Errorf("the transcript did not land beside the panel:\n%s", after)
	}
}

// The usage fetch obeys the staleness gate, so a landing cannot re-trigger itself into a loop.
func TestUsageFetchIsGatedOnStaleness(t *testing.T) {
	m := newModel(t, execPlane())
	if m.usageRefreshCmd("exec-1") == nil {
		t.Error("a cold usage cache did not request a fetch — the context panel would never appear")
	}
	m.execUsage.put("exec-1", &execUsage{total: 1})
	if m.usageRefreshCmd("exec-1") != nil {
		t.Error("a warm usage cache still requested a fetch — that is the flicker loop again")
	}
}

// --- the follow-up box must be DISCOVERABLE ------------------------------------------------------

// The follow-up box is offered in the action bar, not merely bound to a key.
//
// The operator: "I don't see the follow-up chat box to kick off additional questions to the worker."
// `f` was handled by the screen's dispatch all along, but the bar is the ONE place the pane states
// what it can do — a key that is handled but never advertised is a key nobody knows about.
func TestFollowUpIsOfferedInTheActionBar(t *testing.T) {
	p := execPlane()
	m := newModel(t, p)
	m.Base.SelectSource(srcExecutions)
	m.Base.LoadItems(srcExecutions, []kit2.Item{{ID: "exec-1", Title: "exec-1", Meta: "succeeded"}}, "")

	labels := map[string]string{}
	for _, a := range m.actionsForSelection() {
		labels[a.Key] = a.Label
	}
	if _, ok := labels[keyFollowUp]; !ok {
		t.Errorf("the follow-up box is not offered in the action bar (keys: %v) — the operator cannot find it", labels)
	}
	if _, ok := labels[keyInterject]; !ok {
		t.Errorf("interject left the action bar (keys: %v)", labels)
	}
	// Both are present on a FINISHED execution: interject's own refusal explains that it needs a
	// LIVE one, which is exactly why the two do not share a row.
}

// A usage landing repaints from cache and requests nothing.
func TestUsageLandingDoesNotReissueWork(t *testing.T) {
	m := newModel(t, execPlane())
	m.Base.SelectSource(srcExecutions)
	_, next := m.Update(execUsageMsg{execID: "exec-1", usage: &execUsage{total: 10}})
	if next != nil {
		if msg := next(); msg != nil {
			t.Errorf("the usage landing produced follow-up work (%T) — a landing that requests work is a loop", msg)
		}
	}
}
