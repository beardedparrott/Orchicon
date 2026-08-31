package scheduler

import (
	"reflect"
	"testing"

	"github.com/beardedparrott/orchicon/internal/workflow"
)

// TestChainStepIDsParallelGateInLoop reproduces the bug that caused a
// catastrophic concurrent fan-out on loop re-entry:
//
// The "Coding Template with Approvers (Non-human) - Architect" workflow has
// a PARALLEL gate (step-rrcz490q) inside the SSE → loop_decision loop body.
// The stored step array is in canvas order, where the parallel gate sits
// AFTER the loop decision:
//
//	0 step-repo        5 step-approval
//	1 step-sse         6 step-devops-pr
//	2 step-pr-reviewer 7 step-q4xlbg6v
//	3 step-qa          8 step-i64wso0x
//	4 step-loop-2      9 step-rrcz490q   <-- parallel gate, index 9
//
// DAG: repo → q4xlbg6v → i64wso0x → sse → rrcz490q(parallel) →
//      {pr-reviewer, qa} → loop-2 → approval → devops-pr
//
// The old chainStepIDs sliced by array index from "step-sse" (1) to
// "step-loop-2" (4), producing [sse, pr-reviewer, qa] — excluding the
// parallel gate rrcz490q. On loop re-entry createChainRuns never superseded
// rrcz490q's succeeded iteration, so the freshly-created pr-reviewer and qa
// runs saw their dependency as already satisfied and dispatched in parallel
// with the re-entered SSE. This test asserts the chain is the full DAG slice
// (SSE + parallel gate + both leaves), so the gate is re-created and re-gates
// the fan-out.
func TestChainStepIDsParallelGateInLoop(t *testing.T) {
	steps := []workflow.StepWire{
		{ID: "step-repo", Kind: "task", DependsOn: nil},
		{ID: "step-sse", Kind: "task", DependsOn: []string{"step-i64wso0x"}},
		{ID: "step-pr-reviewer", Kind: "task", DependsOn: []string{"step-rrcz490q"}},
		{ID: "step-qa", Kind: "task", DependsOn: []string{"step-rrcz490q"}},
		{ID: "step-loop-2", Kind: "loop_decision", DependsOn: []string{"step-pr-reviewer", "step-qa"}},
		{ID: "step-approval", Kind: "approval", DependsOn: []string{"step-loop-2"}},
		{ID: "step-devops-pr", Kind: "task", DependsOn: []string{"step-approval"}},
		{ID: "step-q4xlbg6v", Kind: "task", DependsOn: []string{"step-repo"}},
		{ID: "step-i64wso0x", Kind: "approval", DependsOn: []string{"step-q4xlbg6v"}},
		{ID: "step-rrcz490q", Kind: "parallel", DependsOn: []string{"step-sse"}},
	}

	got := chainStepIDs(steps, "step-sse", "step-loop-2")
	want := []string{"step-sse", "step-pr-reviewer", "step-qa", "step-rrcz490q"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chainStepIDs = %v, want %v", got, want)
	}
}

// TestChainStepIDsLinearChain verifies the linear-chain behaviour still
// holds (the previously-supported shape): a plain upstream→downstream chain
// yields every step between the loop branch and the decision, in canvas
// order.
func TestChainStepIDsLinearChain(t *testing.T) {
	steps := []workflow.StepWire{
		{ID: "a", Kind: "task"},
		{ID: "b", Kind: "task", DependsOn: []string{"a"}},
		{ID: "c", Kind: "task", DependsOn: []string{"b"}},
		{ID: "loop", Kind: "loop_decision", DependsOn: []string{"c"},
			Config: `{"loop_branch":"b","success_branch":"approve","max_iterations":3}`},
		{ID: "approve", Kind: "approval", DependsOn: []string{"loop"}},
	}

	got := chainStepIDs(steps, "b", "loop")
	want := []string{"b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chainStepIDs = %v, want %v", got, want)
	}
}

// TestChainStepIDsEdgeCases covers the degenerate inputs: unknown steps and
// a fromID with no path to the decision.
func TestChainStepIDsEdgeCases(t *testing.T) {
	steps := []workflow.StepWire{
		{ID: "loop", Kind: "loop_decision", DependsOn: []string{"sse"}},
		{ID: "sse", Kind: "task", DependsOn: []string{"repo"}},
		{ID: "repo", Kind: "task"},
		{ID: "other", Kind: "task", DependsOn: []string{}},
	}

	if got := chainStepIDs(steps, "sse", "loop"); len(got) == 0 {
		t.Fatalf("expected a chain from sse to loop even when loop precedes sse in canvas order, got %v", got)
	}
	if got := chainStepIDs(steps, "missing", "loop"); got != nil {
		t.Fatalf("unknown fromID: got %v, want nil", got)
	}
	if got := chainStepIDs(steps, "sse", "missing"); got != nil {
		t.Fatalf("unknown toID: got %v, want nil", got)
	}
	if got := chainStepIDs(steps, "other", "loop"); got != nil {
		t.Fatalf("disconnected fromID: got %v, want nil", got)
	}
}
