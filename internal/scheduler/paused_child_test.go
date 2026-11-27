package scheduler

import (
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/domain"
)

// WHY THIS FILE EXISTS — the third defect in pause, and the one that hid the first two.
//
// Pause parked a sequence child as `pending`. The sequence reconciler reads `pending` as "on deck —
// arm it", so it re-fired the child's workflow immediately. Measured on work item
// 01M84WD1G42BHD1HTCB2YMQ82Z:
//
//	03:12:27  work_item.paused
//	03:12:27  workflow.run_started     <- same second
//	03:12:52  work_item.paused
//	03:12:52  workflow.run_started     <- same second
//
// Two pauses, two paused runs, and a THIRD run each time. The item read "running" because the
// sequence engine had already undone the pause — so both earlier fixes looked like they did nothing.
//
// `paused` is now a real status, and every arming path must skip it.

func TestPausedChildIsNotArmed(t *testing.T) {
	b, err := os.ReadFile("sequence_reconciler.go")
	if err != nil {
		t.Fatalf("read sequence_reconciler.go: %v", err)
	}
	src := string(b)

	// It must appear in the "wait, don't touch" case of BOTH switches (main arm loop and the
	// dependency-gate pass). Counting occurrences proves it reached more than one.
	n := strings.Count(src, "domain.WorkItemPaused")
	if n < 2 {
		t.Fatalf("WorkItemPaused appears %d time(s) in the sequence reconciler; it must be in every "+
			"list of statuses that mean \"do not arm this child\"", n)
	}
	// And it must NOT be in the arming case alongside pending.
	if strings.Contains(src, "case domain.WorkItemPending, domain.WorkItemBlocked:\n\t\t\t// Backoff gate") {
		// that case is fine as long as paused is not added to it
		if strings.Contains(src, "domain.WorkItemPaused, domain.WorkItemBlocked") ||
			strings.Contains(src, "domain.WorkItemPending, domain.WorkItemPaused") {
			t.Fatal("paused was added to the ARMING case — the reconciler would start a paused child's " +
				"workflow, which is precisely the bug")
		}
	}
}

// TestPausedIsNotAnActiveRunStatus: a paused item must stay resumable.
func TestPausedIsNotAnActiveRunStatus(t *testing.T) {
	if domain.WorkItemPaused == "" {
		t.Fatal("WorkItemPaused is not defined")
	}
	// The value must not collide with an existing status string.
	for _, other := range []string{
		domain.WorkItemPending, domain.WorkItemRunning, domain.WorkItemCheckpointing,
		domain.WorkItemRecovering, domain.WorkItemFailed, domain.WorkItemCancelled,
	} {
		if domain.WorkItemPaused == other {
			t.Fatalf("WorkItemPaused collides with %q", other)
		}
	}
}
