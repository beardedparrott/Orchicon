package workitem

import (
	"encoding/json"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// ComputeNextRunAtFromScheduleJSON parses a raw recurring_schedule JSONB value (as stored on the
// work_items column) and computes the next occurrence >= now.
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
