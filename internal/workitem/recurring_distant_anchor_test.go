package workitem

// recurring_distant_anchor_test.go — A SCHEDULE WHOSE ANCHOR IS FAR IN THE PAST STILL COMPUTES A FUTURE RUN.
//
// This surfaced only once the DB suite could run: TestRecurringAdvanceNextRunAt failed with
// `next_run_at 2026-08-12 should be in the future after advance`. The walk in ComputeNextRunAt is bounded at
// 1000 steps as a runaway guard and used to start AT THE ANCHOR — hourly/2 anchored 89 days back is ~1068
// steps, so the guard exhausted and the ANCHOR was returned. next_run_at then sat months in the past, so the
// item would fire, re-arm in the past, and hot-loop.
//
// The fix fast-forwards whole cadence steps before the walk, so the anchor's distance stops mattering.

import (
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// A schedule whose anchor is far in the past must still give a FUTURE occurrence. This is the reported
// case: hourly/2 anchored 89 days back is ~1068 steps, past the walk's 1000-step guard, so the old code
// returned the ANCHOR and the item would have fired forever.
func TestNextRunAtIsInTheFutureFromADistantAnchor(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		s    *apiv1.RecurringSchedule
	}{
		{"hourly x2 anchored 89 days back", &apiv1.RecurringSchedule{Frequency: "hourly", Interval: 2, StartDate: "2026-08-12", StartTime: "08:00"}},
		{"hourly x1 anchored 3 years back", &apiv1.RecurringSchedule{Frequency: "hourly", Interval: 1, StartDate: "2023-01-01", StartTime: "00:00"}},
		{"minute x5 anchored 2 years back", &apiv1.RecurringSchedule{Frequency: "minute", Interval: 5, StartDate: "2024-01-01", StartTime: "00:00"}},
		{"daily x3 anchored 5 years back", &apiv1.RecurringSchedule{Frequency: "daily", Interval: 3, StartDate: "2021-06-01", StartTime: "09:00"}},
		{"monthly x1 anchored 4 years back", &apiv1.RecurringSchedule{Frequency: "monthly", Interval: 1, StartDate: "2022-01-15", StartTime: "07:30"}},
		{"weekly x2 anchored 3 years back", &apiv1.RecurringSchedule{Frequency: "weekly", Interval: 2, StartDate: "2023-02-01", StartTime: "06:00"}},
	} {
		got := ComputeNextRunAt(tc.s, now)
		if got == nil {
			t.Fatalf("%s: nil next_run_at", tc.name)
		}
		// The contract is ">= now" (a schedule starting now fires now), so the defect to catch is the
		// ANCHOR being returned: a time months or years in the past.
		if got.Before(now) {
			t.Errorf("%s: next_run_at %v is in the PAST (now %v) — the bounded walk exhausted and returned "+
				"the anchor, so the item would fire, re-arm in the past and hot-loop", tc.name, *got, now)
		}
		// And it must be a real fast-forward, not a stub: within one cadence of now.
		if got.Sub(now) > 40*24*time.Hour {
			t.Errorf("%s: next_run_at %v is absurdly far ahead of now %v", tc.name, *got, now)
		}
	}
}

// THE CADENCE IS STILL HONOURED. Fast-forwarding must not just return "now".
func TestFastForwardPreservesTheCadence(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	s := &apiv1.RecurringSchedule{Frequency: "hourly", Interval: 2, StartDate: "2026-08-12", StartTime: "08:00"}
	got := ComputeNextRunAt(s, now)
	if got == nil {
		t.Fatal("nil")
	}
	anchor := time.Date(2026, 8, 12, 8, 0, 0, 0, time.UTC)
	delta := got.Sub(anchor)
	if delta%(2*time.Hour) != 0 {
		t.Errorf("next_run_at %v is not on the 2-hour grid anchored at %v (offset %v)", *got, anchor, delta%(2*time.Hour))
	}
	if got.Sub(now) >= 2*time.Hour {
		t.Errorf("next_run_at %v is more than one interval after now %v — the fast-forward overshot", *got, now)
	}
}

// A FUTURE anchor is untouched: the first occurrence is the anchor itself.
func TestAFutureAnchorIsUnchanged(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	s := &apiv1.RecurringSchedule{Frequency: "hourly", Interval: 2, StartDate: "2026-12-01", StartTime: "08:00"}
	got := ComputeNextRunAt(s, now)
	want := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	if got == nil || !got.Equal(want) {
		t.Fatalf("next_run_at = %v, want %v", got, want)
	}
}
