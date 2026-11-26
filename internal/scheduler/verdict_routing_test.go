package scheduler

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// WHY THIS FILE EXISTS.
//
// A worker that ran to completion and delivered `ORCHICON WORKER SUMMARY: failure` has JUDGED the work.
// The only useful thing a workflow can do with a judgement is ROUTE it: the engineer takes the notes
// and fixes, then the loop re-reviews. That is the whole point of a review/QA loop.
//
// Two code paths did not know that, and together they turned one QA verdict into six identical
// executions and ~1.13M tokens on run 01M8B2A33SB9WDKDB98GQQKV0G (and fourteen on
// 01M8AYA1V9RMJ35MK7XZ04HAB8), then failed the run without ever letting the loop fire:
//
//	1. pollTaskStep charged the verdict against `max_attempts` and resumed the same step on the same
//	   work item, with no checkpoint and no worktree, until the budget was gone.
//	2. the run settle marked every pending step `skipped` — the loop_decision among them — and flipped
//	   the run to failed, so the loop was never dispatched even after the budget was exhausted.
//
// The seeded SDLC graph is the exact shape that exposes it: step-qa fails, and the loop that routes it
// (step-b2bmqv2p) is one hop from QA while a SECOND loop (step-9gfybwnv) sits beyond it still waiting.
// A direct-dependency-only rule would miss QA's failure entirely, so the walk is transitive.

// sdlcSteps mirrors the seeded SDLC (Non-human) dependency graph, trimmed to the part under test.
func sdlcSteps() []workflow.StepWire {
	return []workflow.StepWire{
		{ID: "step-q4xlbg6v", Kind: domain.StepKindTask, Name: "Principal Software Architect"},
		{ID: "step-sse", Kind: domain.StepKindTask, Name: "Senior Software Engineer", DependsOn: []string{"step-q4xlbg6v"}},
		{ID: "step-pr-reviewer", Kind: domain.StepKindTask, Name: "PR Reviewer", DependsOn: []string{"step-sse"}},
		{ID: "step-9gfybwnv", Kind: domain.StepKindLoopDecision, Name: "Loop Decision", DependsOn: []string{"step-pr-reviewer"}},
		{ID: "step-qa", Kind: domain.StepKindTask, Name: "QA Engineer", DependsOn: []string{"step-9gfybwnv"}},
		{ID: "step-b2bmqv2p", Kind: domain.StepKindLoopDecision, Name: "Loop Decision", DependsOn: []string{"step-qa"}},
		{ID: "step-devops-pr", Kind: domain.StepKindTask, Name: "DevOps Engineer", DependsOn: []string{"step-b2bmqv2p"}},
		{ID: "step-3rplua0d", Kind: domain.StepKindLoopDecision, Name: "Loop Decision", DependsOn: []string{"step-devops-pr"}},
		{ID: "step-l32ezp4b", Kind: domain.StepKindEnd, Name: "End", DependsOn: []string{"step-3rplua0d"}},
	}
}

// runsFrom builds a lister over a fixed step-run set, so these rules need no database.
func runsFrom(byStep map[string][]db.WorkflowStepRunRow) stepRunsByStepIDFunc {
	return func(stepID string) ([]db.WorkflowStepRunRow, error) {
		return byStep[stepID], nil
	}
}

// TestVerdictRoutedLoopFindsTheLoopThroughAnIntermediateStep is the gap that made this a no-op before:
// QA's failure has NO loop_decision as a direct dependent — step-b2bmqv2p is one hop away and
// step-9gfybwnv one hop past that. A rule that only looked at direct dependents found nothing, which is
// why the QA verdict was recovered instead of routed.
func TestVerdictRoutedLoopFindsTheLoopThroughAnIntermediateStep(t *testing.T) {
	runs := runsFrom(map[string][]db.WorkflowStepRunRow{
		// step-b2bmqv2p has not decided — it is the loop that must route QA's verdict.
		"step-b2bmqv2p": {{ID: "sr-b2b", StepID: "step-b2bmqv2p", Status: domain.StepRunPending}},
		// step-9gfybwnv decided long ago (QA ran because of it); it must not be the reason.
		"step-9gfybwnv": {{ID: "sr-9gf", StepID: "step-9gfybwnv", Status: domain.StepRunSucceeded}},
	})

	id, ok := verdictRoutedLoop("step-qa", sdlcSteps(), runs)
	if !ok {
		t.Fatal("a failed QA step with a live loop_decision downstream must be routed, not recovered — " +
			"this is the exact case that burned six executions and then failed the run")
	}
	if id != "step-b2bmqv2p" {
		t.Fatalf("routed to %q, want step-b2bmqv2p — the first live loop downstream of step-qa", id)
	}
}

// TestVerdictRoutedLoopIgnoresDecidedLoops is the other edge: a loop that has ALREADY decided must not
// keep holding the run open, or a routed-then-failed run could never fail.
func TestVerdictRoutedLoopIgnoresDecidedLoops(t *testing.T) {
	runs := runsFrom(map[string][]db.WorkflowStepRunRow{
		"step-b2bmqv2p": {{ID: "sr-b2b", StepID: "step-b2bmqv2p", Status: domain.StepRunSucceeded}},
		"step-9gfybwnv": {{ID: "sr-9gf", StepID: "step-9gfybwnv", Status: domain.StepRunSucceeded}},
	})
	if id, ok := verdictRoutedLoop("step-qa", sdlcSteps(), runs); ok {
		t.Fatalf("loop %q already decided but was still treated as owed — the run could never fail", id)
	}
}

// TestVerdictRoutedLoopLeavesNonLoopWorkflowsAlone pins the blast radius: a failed step in a graph with
// no loop_decision keeps today's behaviour, because this fix must not change recovery for every
// workflow in the platform.
func TestVerdictRoutedLoopLeavesNonLoopWorkflowsAlone(t *testing.T) {
	steps := []workflow.StepWire{
		{ID: "a", Kind: domain.StepKindTask},
		{ID: "b", Kind: domain.StepKindTask, DependsOn: []string{"a"}},
	}
	runs := runsFrom(map[string][]db.WorkflowStepRunRow{})
	if id, ok := verdictRoutedLoop("a", steps, runs); ok {
		t.Fatalf("a workflow with no loop_decision reported an owed loop %q", id)
	}
}

// TestLoopDesiredDownstreamsCoversTheLoopsRemit asserts the run-settle exception set: the loop itself
// AND everything it is owed, so the sweep cannot bury the loop or the steps it routes to.
func TestLoopDesiredDownstreamsCoversTheLoopsRemit(t *testing.T) {
	runs := runsFrom(map[string][]db.WorkflowStepRunRow{
		"step-b2bmqv2p": {{ID: "sr-b2b", StepID: "step-b2bmqv2p", Status: domain.StepRunPending}},
		"step-9gfybwnv": {{ID: "sr-9gf", StepID: "step-9gfybwnv", Status: domain.StepRunSucceeded}},
		"step-q4xlbg6v": {{ID: "sr-q4x", StepID: "step-q4xlbg6v", Status: domain.StepRunSucceeded}},
	})
	got := loopDesiredDownstreams(sdlcSteps(), runs)

	for _, want := range []string{"step-b2bmqv2p", "step-devops-pr", "step-3rplua0d", "step-l32ezp4b"} {
		if !got[want] {
			t.Errorf("%s is downstream of a live loop and would be skipped by the failure sweep", want)
		}
	}
	for _, notWant := range []string{"step-qa", "step-sse", "step-q4xlbg6v", "step-pr-reviewer"} {
		if got[notWant] {
			t.Errorf("%s already ran (it is upstream of the loop) and must not be held dispatchable", notWant)
		}
	}
}

// TestVerdictRoutingWiredAtBothSites guards the wiring itself. These rules are worthless if nothing
// calls them — the failure mode this whole change exists to remove is a gate that is satisfied by its
// own neighbour. Asserted against source with comments stripped, because this file's prose names the
// same identifiers.
func TestVerdictRoutingWiredAtBothSites(t *testing.T) {
	src := readWorkflowReconcilerSource(t)

	if !strings.Contains(src, "verdictRoutedLoop(sr.StepID, allSteps, r.stepRunsOf(ctx, tx, tenantID, run.ID))") {
		t.Error("pollTaskStep no longer asks verdictRoutedLoop before choosing recovery, so a delivered " +
			"verdict is charged against max_attempts again and the step is resumed until the budget is gone")
	}
	if !strings.Contains(src, "loopDesiredDownstreams(steps, r.stepRunsOf(ctx, ttx.Tx, tenantID, runID))") {
		t.Error("the failure sweep no longer spares a live loop's steps, so the loop_decision is marked " +
			"skipped and the run fails without ever routing the verdict")
	}
	if !strings.Contains(src, "!r.loopOwesRouting(ctx, ttx.Tx, tenantID, runID, steps)") {
		t.Error("the run can settle to failed while a loop_decision is still owed its routing turn")
	}
}

// readWorkflowReconcilerSource reads workflow_reconciler.go (with comments stripped), which is where
// the routing lives — readReconcilerSource reads reconciler.go and would pass vacuously here.
func readWorkflowReconcilerSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("workflow_reconciler.go")
	if err != nil {
		t.Fatalf("read workflow_reconciler.go: %v", err)
	}
	return stripCommentsForGate(string(b))
}
