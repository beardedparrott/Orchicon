package scheduler

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// WHY THIS FILE EXISTS.
//
// Run 01M8CSC92YWEZSC2FTRWC2K5A7 stalled for over an hour in status `running` with nothing in
// flight. The loop DID fire — #1033 fixed that — but it took the wrong branch:
//
//	step-b2bmqv2p iter 0  succeeded   result {"loop":"recovered"}
//	step-b2bmqv2p iter 1  pending     result {}
//	step-sse              succeeded   never superseded, never re-dispatched
//
// `{"loop":"recovered"}` is the RECOVERY branch, whose entire purpose is to spawn an iteration and
// WAIT for a recovery cycle to finish before continuing. But recovery had already been ruled out at
// the dispatch site (the verdict was delivered, so the step went terminal), so no recovery cycle
// would ever run. The loop parked itself waiting for something that could not happen.
//
// The cause was a field mismatch. QA's step run carried:
//
//	_decision: "failure"      <- the worker's judgement, which the loop reads
//	_verdict:  (absent)       <- the field the loop's CLASSIFIER read
//
// An empty verdict means "infrastructure fault" to upstreamFaultedByInfra, so `anyFailed` was set
// and the recovery branch ran — for a failure that was a delivered verdict all along.
//
// `_verdict` is only stamped when a failure is judged to have been CAUSED by a verdict, which is a
// narrower condition than "the worker finished and reported failure". These tests pin the fix on
// both sides of the wire: the writer now backfills, and the loop's reader accepts the decision.

// seededSDLCSteps is the dependency graph of workflow version 12 (SDLC Non-human), transcribed from
// the live row rather than from memory — the shape is the whole point of the bug, so inventing it
// would test nothing.
func seededSDLCSteps() []workflow.StepWire {
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

// TestChainStepIDsRebuildsTheSseToQaChain is a guard on the OTHER hypothesis for that stall.
//
// The run's own history made "chainStepIDs returned nothing" an equally plausible explanation —
// step-sse was never superseded, which is exactly what an empty chain would produce. It is not the
// cause, and this test says so loudly so nobody re-derives that theory from the same evidence: the
// chain is four steps, and a chain that came back empty would be a different, worse bug.
func TestChainStepIDsRebuildsTheSseToQaChain(t *testing.T) {
	got := chainStepIDs(seededSDLCSteps(), "step-sse", "step-b2bmqv2p")

	want := map[string]bool{
		"step-sse": true, "step-pr-reviewer": true, "step-9gfybwnv": true, "step-qa": true,
	}
	if len(got) != len(want) {
		t.Fatalf("chainStepIDs(sse -> b2bmqv2p) = %v, want %d steps %v", got, len(want), want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("chain contains %q, which is not between the loop target and the loop decision", id)
		}
	}
	// The loop decision itself must never be in its own loop-back chain.
	for _, id := range got {
		if id == "step-b2bmqv2p" {
			t.Fatal("the loop decision was included in its own loop-back chain")
		}
	}
}

// TestFailedStepWithOnlyADecisionIsADeliveredVerdict pins the CLASSIFIER contract that broke.
//
// The exact values from the stalled run: status failed, decision "failure", verdict absent.
func TestFailedStepWithOnlyADecisionIsADeliveredVerdict(t *testing.T) {
	// What the loop used to do: consult the verdict alone, see "", and call it an infrastructure
	// fault — taking the recovery branch that parked the run.
	const emptyVerdict = ""
	if !upstreamFaultedByInfra(domain.StepRunFailed, emptyVerdict) {
		t.Fatal("precondition changed: an empty verdict is no longer classified as an infra fault, " +
			"so this test no longer describes the bug it exists to prevent")
	}

	// What it must do now: the decision IS the judgement, so the failure routes instead.
	verdict := loopVerdictForFailedStep("failure", "", nil)
	if upstreamFaultedByInfra(domain.StepRunFailed, verdict) {
		t.Fatal("a failed step that reported decision \"failure\" was still treated as an " +
			"infrastructure fault — the loop will take the recovery branch and park the run again")
	}
	if verdict != "failure" {
		t.Fatalf("loopVerdictForFailedStep = %q, want the reported decision", verdict)
	}
}

// TestLoopVerdictPrefersAnExplicitVerdict: when both are present the explicit verdict wins, because
// it is the narrower, more deliberate signal.
func TestLoopVerdictPrefersAnExplicitVerdict(t *testing.T) {
	if got := loopVerdictForFailedStep("failure", "rejected", nil); got != "rejected" {
		t.Fatalf("loopVerdictForFailedStep = %q, want the explicit _verdict to win", got)
	}
}

// TestLoopVerdictFallsBackToTheWorkItem is the third source: the recovery engine writes `_verdict`
// on the WORK ITEM, so a reader that only consults the step run misses it.
func TestLoopVerdictFallsBackToTheWorkItem(t *testing.T) {
	wi := map[string]any{"_verdict": "failure"}
	if got := loopVerdictForFailedStep("", "", wi); got != "failure" {
		t.Fatalf("loopVerdictForFailedStep = %q, want the work-item verdict", got)
	}
}

// TestLoopVerdictIsEmptyForAGenuineCrash is the edge that must NOT change: a step that died without
// reporting anything is still an infrastructure fault, and recovery still owns it.
func TestLoopVerdictIsEmptyForAGenuineCrash(t *testing.T) {
	if got := loopVerdictForFailedStep("", "", nil); got != "" {
		t.Fatalf("loopVerdictForFailedStep = %q, want empty for a crash with no judgement", got)
	}
	if !upstreamFaultedByInfra(domain.StepRunFailed, "") {
		t.Fatal("a crash with no verdict must still be recoverable")
	}
}

// TestRecoveredLoopCannotParkAVerdictFailure is the behavioural assertion: the recovery branch's
// marker must be unreachable for a verdict-failed upstream, because nothing will ever resume it.
func TestRecoveredLoopCannotParkAVerdictFailure(t *testing.T) {
	// The marker the recovery branch writes, from the live row.
	const recoveredMarker = `{"loop":"recovered"}`

	verdict := loopVerdictForFailedStep("failure", "", nil)
	if upstreamFaultedByInfra(domain.StepRunFailed, verdict) {
		t.Fatalf("the recovery branch would run and write %s for a verdict-failed upstream — that "+
			"iteration waits forever, which is exactly how run 01M8CSC92YWEZSC2FTRWC2K5A7 stalled",
			recoveredMarker)
	}
}

// TestLoopReaderConsultsTheDecisionBeforeTheWorkItem guards the wiring, because the rule is
// worthless if the reader does not call it.
func TestLoopReaderConsultsTheDecisionBeforeTheWorkItem(t *testing.T) {
	src := readWorkflowReconcilerSource(t)
	if !strings.Contains(src, "loopVerdictForFailedStep(upResult.Decision, upResult.Verdict, nil)") {
		t.Error("the loop's verdict reader no longer consults the reported decision, so a worker that " +
			"reports failure without a derived verdict is classified as an infrastructure fault again")
	}
	_ = db.WorkflowStepRunRow{}
}
