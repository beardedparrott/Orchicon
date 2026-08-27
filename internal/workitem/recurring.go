package workitem

import (
	"encoding/json"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ComputeNextRunAtFromScheduleJSON parses a raw recurring_schedule JSONB
// value (as stored on the work_items column) and computes the next occurrence
// >= now. Returns nil when the schedule is empty or unparseable (the caller
// keeps whatever next_run_at it has). This is the canonical helper for
// re-arming a paused/expired cursor at the service layer (it mirrors the
// scheduler's computeRecurringNextRunAt).
func ComputeNextRunAtFromScheduleJSON(scheduleJSON []byte, now time.Time) *time.Time {
	if len(scheduleJSON) == 0 {
		return nil
	}
	var rs apiv1.RecurringSchedule
	if err := json.Unmarshal(scheduleJSON, &rs); err != nil {
		return nil
	}
	return ComputeNextRunAt(&rs, now)
}

// RecurringOutputsIdeas reports whether a recurring item's schedule declares
// outputs_mode = ideas. In this worktree 4.1's outputs_mode field does not
// exist on RecurringSchedule, so it always returns false — the
// idea/provenance stamping seam is a no-op until 4.1 lands (gated in
// CreateWorkItem). When 4.1 adds the field, this resolves the value from the
// schedule JSONB so worker-created work items inside a fire's run can be
// stamped idea-state + spawned_by provenance; with no such field the creation
// is unmarked (no idea routing). Never hard-depends on 4.1 symbols.
func RecurringOutputsIdeas(scheduleJSON []byte) bool {
	return false
}
