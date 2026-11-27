package workitem

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS — the reader half of the pause deadlock.
//
// A work item in `checkpointing` whose RUN is `paused` has nothing running and is perfectly
// resumable. The old guard refused it because it tested the work item's status, and `checkpointing`
// is an active status. The item could then never be freed by any gesture.
//
// The fix is to ask the RUN — the thing that actually decides — rather than the symptom status.

func TestResumeAsksTheRunNotTheStatus(t *testing.T) {
	b, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	src := strings.ReplaceAll(string(b), "\t", "")

	if !strings.Contains(src, "pausedRun") {
		t.Fatal("the RESUME guard no longer consults the run's status, so an item left in " +
			"`checkpointing` by an idle pause can never be resumed")
	}
	if !strings.Contains(src, "pausedRun = run.Status == domain.WorkflowRunPaused") {
		t.Error("the guard does not check for a paused run specifically")
	}
	// And the exemption must actually gate the refusal, not merely be computed.
	if !strings.Contains(src, "if !pausedRun && (IsActiveRunStatus(current.Status)") {
		t.Error("the paused-run exemption is computed but does not gate the refusal — a dead guard")
	}
}
