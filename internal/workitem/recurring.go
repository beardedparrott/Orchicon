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
