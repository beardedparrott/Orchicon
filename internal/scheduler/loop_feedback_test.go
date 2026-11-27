package scheduler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/domain"
	"github.com/beardedparrott/orchicon/internal/workflow"
)

// WHY THIS FILE EXISTS — the second half of "route it to SSE".
//
// Routing a rejected step back to the engineer only helps if the engineer learns WHY. The chain runs
// the loop re-created carried NO Result at all, so SSE re-entered knowing only "this is iteration 2":
// it re-derived the same work, produced the same defect, and the loop spent its iterations on a
// round trip that carried no information. Fixing the routing without this made the loop fire and
// changed nothing about the outcome — which is exactly the operator's complaint.
//
// These tests pin the two halves: the feedback is COLLECTED from the rejecting step, and it TRAVELS
// on every re-armed step in the chain.

func sdlcLoopStep() workflow.StepWire {
	return workflow.StepWire{
		ID:        "step-b2bmqv2p",
		Kind:      domain.StepKindLoopDecision,
		Name:      "Loop Decision",
		DependsOn: []string{"step-qa"},
	}
}

// qaRejection is the shape QA's step run actually had on run 01M8CSC92YWEZSC2FTRWC2K5A7: a decision,
// a summary carrying the diagnosis, and no separate issues array.
func qaRejection() db.WorkflowStepRunRow {
	return db.WorkflowStepRunRow{
		StepID: "step-qa",
		Status: domain.StepRunFailed,
		Result: []byte(`{
			"_decision": "failure",
			"_summary": "AC4 and AC5 fail because ask_user.options is an array with no items, so Gemini rejects the tool schema with 400 INVALID_ARGUMENT.",
			"_output": "long raw output that should NOT be used when a summary exists"
		}`),
	}
}

func TestLoopFeedbackCarriesTheRejectingStepsReason(t *testing.T) {
	runs := map[string]db.WorkflowStepRunRow{"step-qa": qaRejection()}

	fb := loopFeedbackFrom(runs, sdlcLoopStep(), 0)
	if fb == nil {
		t.Fatal("no feedback collected from a failed QA step — the engineer re-enters blind, which is " +
			"the defect this exists to prevent")
	}
	if !strings.Contains(fb.Summary, "ask_user.options") {
		t.Errorf("summary = %q, want the reviewer's diagnosis carried forward", fb.Summary)
	}
	if fb.FromStep != "step-qa" {
		t.Errorf("FromStep = %q, want the step that rejected the work", fb.FromStep)
	}
	if fb.Decision != "failure" {
		t.Errorf("Decision = %q", fb.Decision)
	}
	// The summary is the distilled judgement; the raw output must not displace it.
	if strings.Contains(fb.Summary, "long raw output") {
		t.Error("feedback used the raw output when a summary existed — the summary is the judgement")
	}
	if fb.Iteration != 1 {
		t.Errorf("Iteration = %d, want 1 (this is the loop generation being entered)", fb.Iteration)
	}
}

// TestLoopFeedbackFallsBackToTheOutput: a step that reported but wrote no separate summary still has
// a judgement worth carrying.
func TestLoopFeedbackFallsBackToTheOutput(t *testing.T) {
	runs := map[string]db.WorkflowStepRunRow{"step-qa": {
		StepID: "step-qa",
		Result: []byte(`{"_decision":"failure","_output":"the live probe could not run; no credentials"}`),
	}}
	fb := loopFeedbackFrom(runs, sdlcLoopStep(), 0)
	if fb == nil || !strings.Contains(fb.Summary, "no credentials") {
		t.Fatalf("fb = %+v, want the output used when no summary exists", fb)
	}
}

// TestLoopFeedbackIsNilWhenThereIsNothingToSay: a crash with no judgement must not fabricate one.
func TestLoopFeedbackIsNilWhenThereIsNothingToSay(t *testing.T) {
	runs := map[string]db.WorkflowStepRunRow{"step-qa": {StepID: "step-qa", Result: []byte(`{}`)}}
	if fb := loopFeedbackFrom(runs, sdlcLoopStep(), 0); fb != nil {
		t.Fatalf("fb = %+v, want nil — a step that said nothing has nothing to carry", fb)
	}
}

// TestLoopFeedbackReachesTheReArmedSteps is the wiring half: createChainRuns must STAMP it. A payload
// nothing writes onto the row is a payload no worker will ever see.
func TestLoopFeedbackReachesTheReArmedSteps(t *testing.T) {
	src := readWorkflowReconcilerSource(t)
	i := strings.Index(src, "func (r *WorkflowReconciler) createChainRuns")
	if i < 0 {
		t.Fatal("createChainRuns is gone")
	}
	body := src[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "feedback.resultJSON()") {
		t.Error("createChainRuns no longer stamps the loop feedback on the re-armed runs, so a " +
			"re-entered step starts with no Result and re-derives the rejected work")
	}
	// And the prompt must state it, or the stamp is inert.
	if !strings.Contains(src, "Why this is being re-run") {
		t.Error("the composite prompt never tells the worker why it is being re-run")
	}
}

// TestLoopFeedbackPayloadIsJSON guards the wire form.
func TestLoopFeedbackPayloadIsJSON(t *testing.T) {
	fb := &loopFeedback{FromStep: "step-qa", Summary: "s", Decision: "failure", Iteration: 2}
	b, err := fb.resultJSON()
	if err != nil {
		t.Fatalf("resultJSON: %v", err)
	}
	var back loopFeedback
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if back.FromStep != "step-qa" || back.Iteration != 2 {
		t.Errorf("round trip lost fields: %+v", back)
	}
}
