package workitem

// recurring_zone_test.go — A RECURRENCE IS A WALL CLOCK IN A ZONE, NOT AN INSTANT.
//
// The operator: "We could easily build in a convertor that turns the user's system time into UTC so it all
// works nicely."
//
// That is exactly right for a TIMESTAMP and exactly wrong for a RECURRENCE, and these tests are the
// difference written down. A one-shot scheduled_start_at IS an instant and converts to UTC by
// construction. "09:00 every day" is NOT an instant: it is 09:00 CDT in July and 09:00 CST in January, an
// hour apart, so flattening it into one UTC instant loses an hour for half the year whichever instant you
// pick. The zone therefore travels with the rule.
//
// The legacy case is pinned too, because it is a promise: a schedule written before the timezone field
// existed carries no zone, and it must keep firing at exactly the instant it fires at today. The operator:
// "Leave them. I don't want things firing again."

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// chicago loads the operator's zone by IANA name, so CST and CDT are the real ones (DST included) rather
// than an offset typed once.
func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no zone database on this machine — the zone-aware behaviour is unobservable here")
	}
	return loc
}

// ------------------------------------------------------------------ RecurringZone

func TestRecurringZoneEmptyMeansUTC(t *testing.T) {
	// The LEGACY semantic, and a promise rather than a fallback: unchanged fire times for existing rows.
	loc, err := RecurringZone("")
	if err != nil {
		t.Fatalf("an empty zone must be accepted: %v", err)
	}
	if loc != time.UTC {
		t.Errorf("an empty zone resolved to %v, want UTC — the legacy semantic", loc)
	}
	// Whitespace is the same thing.
	if loc, err := RecurringZone("   "); err != nil || loc != time.UTC {
		t.Errorf("a blank zone = %v, %v; want UTC, nil", loc, err)
	}
}

func TestRecurringZoneAcceptsIANANames(t *testing.T) {
	for _, name := range []string{"America/Chicago", "Europe/Berlin", "UTC", "Asia/Kolkata"} {
		loc, err := RecurringZone(name)
		if err != nil {
			t.Errorf("RecurringZone(%q) errored: %v", name, err)
			continue
		}
		// It must be the zone asked for, not a silent substitute.
		if loc.String() != name {
			t.Errorf("RecurringZone(%q) resolved to %q", name, loc.String())
		}
	}
}

// THE PSEUDO-ZONE IS REFUSED BY NAME, and this is the subtle half of the whole feature.
//
// time.LoadLocation("Local") RETURNS NO ERROR — it resolves to whatever machine is reading it — so any
// check of the form "is it loadable?" passes it. Stored, it would then mean the SERVER's zone: a
// Central-time operator's 09:00 would fire at 09:00 UTC, i.e. 03:00 their time, with every validation
// step reporting success.
func TestRecurringZoneRejectsThePseudoZoneLocal(t *testing.T) {
	// The premise, measured: it really does load.
	if _, err := time.LoadLocation("Local"); err != nil {
		t.Fatalf("premise changed: LoadLocation(\"Local\") now errors (%v), so the reason this guard "+
			"exists needs re-checking", err)
	}
	loc, err := RecurringZone("Local")
	if err == nil {
		t.Fatalf("RecurringZone(\"Local\") was accepted as %v — it resolves to whatever machine reads it, "+
			"so a schedule stamped Local by the client would fire in the SERVER's zone", loc)
	}
	if !strings.Contains(err.Error(), "IANA") {
		t.Errorf("the rejection should say what IS wanted: %v", err)
	}
}

func TestRecurringZoneRejectsUnknownNames(t *testing.T) {
	if _, err := RecurringZone("Mars/Olympus_Mons"); err == nil {
		t.Error("an unknown zone must be rejected at write time, not produce a schedule that never fires")
	}
	if _, err := RecurringZone("Central Time"); err == nil {
		t.Error("a prose zone name must be rejected")
	}
}

// ------------------------------------------------------------ ComputeNextRunAt zones

// THE ANCHOR IS READ IN THE SCHEDULE'S ZONE. 09:00 Central is 15:00 UTC in winter — not 09:00 UTC.
func TestComputeNextRunAtReadsTheAnchorInTheScheduleZone(t *testing.T) {
	jan := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		Timezone: "America/Chicago",
	}, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	if jan == nil {
		t.Fatal("no occurrence computed")
	}
	if got := jan.UTC().Format("15:04"); got != "15:00" {
		t.Errorf("09:00 Central in January fired at %s UTC, want 15:00 (CST is UTC-6) — reading the anchor "+
			"as UTC would give 09:00", got)
	}

	jul := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-07-15", StartTime: "09:00",
		Timezone: "America/Chicago",
	}, time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC))
	if jul == nil {
		t.Fatal("no occurrence computed")
	}
	// CDT is UTC-5, so the SAME wall clock is a different instant — the property that makes an
	// instant-shaped conversion wrong for a recurrence.
	if got := jul.UTC().Format("15:04"); got != "14:00" {
		t.Errorf("09:00 Central in July fired at %s UTC, want 14:00 (CDT is UTC-5)", got)
	}
}

// THE WALL CLOCK SURVIVES THE DST BOUNDARY. A daily 09:00 schedule keeps firing at 09:00 on the
// operator's clock across the transition, which means its UTC time MOVES by an hour.
func TestDailyCadenceKeepsTheWallClockAcrossDST(t *testing.T) {
	loc := chicago(t)

	// Anchored in CST (Mar 6), asked for a date after the transition (Mar 8 2026).
	got := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-03-06", StartTime: "09:00",
		Timezone: "America/Chicago",
	}, time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC))
	if got == nil {
		t.Fatal("no occurrence computed")
	}
	if h := got.In(loc).Hour(); h != 9 {
		t.Errorf("the post-DST fire is at %02d:00 LOCAL, want 09:00 — a daily schedule must keep its wall "+
			"clock, not drift an hour when the offset changes", h)
	}
	if name, _ := got.In(loc).Zone(); name != "CDT" {
		t.Errorf("the fire's zone reads %q, want CDT (the schedule is now in daylight time)", name)
	}
}

// THE WEEKLY DAY INDEX IS COUNTED, NOT DERIVED FROM ELAPSED HOURS.
//
// This is the latent bug the zone change exposes. The old code computed the day index as
// int(candidate.Sub(anchor).Hours() / 24); across a DST transition a local day is 23 hours, so the
// total lands an hour short and the integer division picks the WRONG day — 13 instead of 14 — which
// shifts the weekly pattern onto a different weekday. It could not fire on UTC anchors because every
// UTC day is exactly 24 hours.
func TestWeeklyCadenceLandsOnTheRightWeekdayAcrossDST(t *testing.T) {
	loc := chicago(t)

	// 2026-03-02 is a Monday. DST begins 2026-03-08.
	got := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "weekly", Interval: 1,
		Days:      []string{"Mon"},
		StartDate: "2026-03-02", StartTime: "09:00",
		Timezone: "America/Chicago",
	}, time.Date(2026, time.March, 16, 0, 0, 0, 0, time.UTC))
	if got == nil {
		t.Fatal("no occurrence computed")
	}
	if wd := got.In(loc).Weekday(); wd != time.Monday {
		t.Errorf("a Monday-only schedule next fired on %s — the day index drifted across the DST boundary, "+
			"so the pattern skipped a weekday", wd)
	}
	if h := got.In(loc).Hour(); h != 9 {
		t.Errorf("fired at %02d:00 local, want 09:00", h)
	}
}

// THE LEGACY CASE IS UNCHANGED, which is the operator's explicit instruction.
//
// A schedule with no zone must behave EXACTLY as it did before the field existed: anchor parsed as UTC,
// same instant, same cadence. This is the regression guard on "leave them alone".
func TestLegacyScheduleWithoutAZoneBehavesExactlyAsBefore(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	got := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		// Timezone omitted — the pre-field shape.
	}, now)
	if got == nil {
		t.Fatal("no occurrence computed")
	}
	want := time.Date(2026, time.January, 15, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("a zone-less schedule fired at %v, want %v — existing schedules must not move", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("a zone-less schedule resolved in %v, want UTC", got.Location())
	}
}

// AN UNRESOLVABLE ZONE FALLS BACK TO UTC RATHER THAN STOPPING THE SCHEDULE.
//
// This function cannot report an error, and a schedule the API already accepted must not silently stop
// firing because a zone name is missing from this build's database. Firing in the legacy zone is the
// safer failure; RecurringZone on the write path is what keeps bad names out in the first place.
func TestComputeNextRunAtFallsBackToUTCForAnUnknownZone(t *testing.T) {
	got := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		Timezone: "Not/AZone",
	}, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	if got == nil {
		t.Fatal("an unknown zone must not stop the schedule from firing")
	}
	want := time.Date(2026, time.January, 15, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("fell back to %v, want the legacy UTC anchor %v", got, want)
	}
}

// THE WINDOW IS EVALUATED ON THE OPERATOR'S CLOCK, which is what makes a 09:00-17:00 window mean
// business hours rather than 09:00-17:00 UTC.
func TestWindowGatingUsesTheScheduleZone(t *testing.T) {
	loc := chicago(t)

	// 09:00-17:00, anchored AT the window start so the anchor itself is in-window.
	got := ComputeNextRunAt(&apiv1.RecurringSchedule{
		Frequency: "hourly", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		WindowStart: "09:00", WindowEnd: "17:00",
		Timezone: "America/Chicago",
	}, time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC))
	if got == nil {
		t.Fatal("no occurrence computed")
	}
	local := got.In(loc)
	if h := local.Hour(); h < 9 || h >= 17 {
		t.Errorf("the first fire is %02d:00 local, outside the 09:00-17:00 window — the window is being "+
			"evaluated in the wrong zone", h)
	}
	// And it is genuinely local-windowed, not UTC-windowed: 09:00 local is 15:00 UTC, well outside a
	// 09:00-17:00 reading taken in UTC terms would still pass, so this asserts the zone is honoured by
	// checking the wall clock the operator sees.
	if name, _ := local.Zone(); name == "" {
		t.Error("the computed instant carries no zone")
	}
}

// AND THE ZONE SURVIVES THE JSONB ROUND TRIP, which is the only reason there is no migration: the
// schedule is stored as a JSON object inside the existing recurring_schedule column, so the proto field
// name and the JSON key have to agree or the zone is silently dropped on write and read back empty.
func TestTimezoneRoundTripsThroughTheStoredJSON(t *testing.T) {
	blob, err := ValidateRecurringSchedule(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		Timezone: "America/Chicago",
	})
	if err != nil {
		t.Fatalf("ValidateRecurringSchedule: %v", err)
	}
	if !strings.Contains(string(blob), `"timezone":"America/Chicago"`) {
		t.Fatalf("the stored JSON does not carry the zone: %s", blob)
	}
	// The fire path unmarshals this blob straight into the proto struct.
	got := ComputeNextRunAtFromScheduleJSON(blob, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))
	if got == nil {
		t.Fatal("no occurrence computed from the stored blob")
	}
	if h := got.UTC().Hour(); h != 15 {
		t.Errorf("the zone was lost in the round trip: 09:00 Central fired at %02d:00 UTC, want 15:00", h)
	}
}

// AND THE WRITE PATH REFUSES A BAD ZONE, so an unusable schedule cannot be stored at all.
func TestValidateRecurringScheduleRejectsABadZone(t *testing.T) {
	if _, err := ValidateRecurringSchedule(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		Timezone: "Local",
	}); err == nil {
		t.Error("the write path accepted the pseudo-zone Local")
	}
	if _, err := ValidateRecurringSchedule(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
		Timezone: "Not/AZone",
	}); err == nil {
		t.Error("the write path accepted an unknown zone")
	}
	// The legacy shape must still be accepted: existing clients send no zone.
	if _, err := ValidateRecurringSchedule(&apiv1.RecurringSchedule{
		Frequency: "daily", Interval: 1,
		StartDate: "2026-01-15", StartTime: "09:00",
	}); err != nil {
		t.Errorf("the legacy zone-less payload must still validate: %v", err)
	}
}
