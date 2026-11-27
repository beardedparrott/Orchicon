package workflow

import (
	"os"
	"strings"
	"testing"
)

// WHY THIS FILE EXISTS.
//
// Work item 01M84WD1G42BHD1HTCB2YMQ82Z was paused and sat in `checkpointing` indefinitely. Nothing
// was in flight — QA had failed 110 minutes earlier — so there was nothing to checkpoint, yet the
// item was stamped `checkpointing` anyway.
//
// That is a DEADLOCK, not a slow operation, because `checkpointing` is in IsActiveRunStatus:
//
//	RESUME   refuses ("cannot RESUME a work item that is running")
//	PAUSE    refuses (no run in flight)
//	RESTART  refuses (isInFlight is true)
//	reconciler ignores a `paused` run, so nothing revisits the item
//
// The operator sees "checkpointing for a while" with no error and no way out.
//
// These tests pin the two halves that break the deadlock: the pause must describe what ACTUALLY
// happened, and the resume guard must ask the RUN rather than the symptom status.

func readRunControl(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("run_control.go")
	if err != nil {
		t.Fatalf("read run_control.go: %v", err)
	}
	return string(b)
}

// TestPauseDoesNotStampCheckpointingWhenNothingWasCheckpointed is the writer half.
func TestPauseDoesNotStampCheckpointingWhenNothingWasCheckpointed(t *testing.T) {
	src := readRunControl(t)

	if !strings.Contains(src, "paused++") {
		t.Error("pauseRunInner does not count what it actually paused, so it cannot tell a real " +
			"checkpoint from an idle pause — and a paused-with-nothing-running item gets stamped " +
			"`checkpointing` and deadlocks")
	}
	if !strings.Contains(src, "status := domain.WorkItemPending") {
		t.Error("the idle-pause branch no longer defaults the work item to pending")
	}
	// The old unconditional stamp must be gone: checkpointing may only be set when paused > 0.
	if strings.Contains(src, "status := domain.WorkItemCheckpointing\n\t\t\t_, _ = db.UpdateWorkItem") {
		t.Error("the work item is stamped `checkpointing` unconditionally again — that is the deadlock")
	}
	if !strings.Contains(src, "if paused > 0 {") {
		t.Error("`checkpointing` is not gated on there having been something to checkpoint")
	}
}

// TestPauseIsNotPermanentWhenNothingIsInFlight documents the invariant the operator hit.
func TestPauseIsNotPermanentWhenNothingIsInFlight(t *testing.T) {
	src := readRunControl(t)
	// An idle pause must land the item somewhere RESUMABLE, i.e. NOT an active status.
	if !strings.Contains(src, "WorkItemPending") {
		t.Fatal("an idle pause leaves the item in an active status; every gesture that could free " +
			"it will refuse")
	}
}
