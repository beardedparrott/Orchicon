package work

import (
	"strings"
	"testing"
	"time"
)

// The operator: "Schedule picker should have more options and once selected, it
// should print out the full start date/time and allow that to be customized."
//
// The field's committed display IS the option's label, so each preset label must
// carry the exact timestamp — that is what makes the chosen time visible.
func TestSchedulePresetsShowTheFullTimestamp(t *testing.T) {
	opts := schedulePresets()
	if len(opts) < 8 {
		t.Fatalf("expected a useful set of presets, got %d", len(opts))
	}
	now := time.Now().UTC()
	for _, o := range opts {
		if o.Value == "" {
			continue // "not scheduled"
		}
		at, err := time.Parse(time.RFC3339, o.Value)
		if err != nil {
			t.Fatalf("preset value %q is not RFC3339: %v", o.Value, err)
		}
		if !at.After(now) {
			t.Errorf("preset %q is in the past", o.Label)
		}
		// The label must PRINT the exact timestamp, not only a friendly name.
		if !strings.Contains(o.Label, o.Value) {
			t.Errorf("preset label %q must contain its full timestamp %q", o.Label, o.Value)
		}
	}
	// The list is finer-grained at the near end, where scheduling happens.
	if !strings.Contains(opts[1].Label, "15 minutes") || !strings.Contains(opts[2].Label, "30 minutes") {
		t.Fatalf("expected near-term presets first, got %q / %q", opts[1].Label, opts[2].Label)
	}
	// A past-time preset must be filtered rather than offered.
	for _, o := range opts {
		if o.Value == "" {
			continue
		}
		at, _ := time.Parse(time.RFC3339, o.Value)
		if at.Before(now.Add(-time.Minute)) {
			t.Fatalf("a past time was offered: %q", o.Label)
		}
	}
}

// The scheduled-start field validates a hand-edited value, so a custom time that
// is malformed is reported rather than sent.
func TestScheduledStartAcceptsEmptyOrRFC3339(t *testing.T) {
	if err := validateOptionalRFC3339(""); err != nil {
		t.Fatalf("an unscheduled field must be valid: %v", err)
	}
	if err := validateOptionalRFC3339("2026-09-01T09:00:00Z"); err != nil {
		t.Fatalf("a valid timestamp must be accepted: %v", err)
	}
	if err := validateOptionalRFC3339("tomorrow morning"); err == nil {
		t.Fatal("a malformed timestamp must be rejected")
	}
}
