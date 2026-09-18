package screenkit

// format_test.go — TIMES ARE LOCAL AND ALWAYS LABELLED.
//
// The operator: "I also would like all times in orch to actually show local time and print the time zone
// by the time so people truly know what timezone it is reporting in. This should be able to be gathered
// from the system."
//
// THESE ASSERTIONS ARE DELIBERATELY WRITTEN TO HOLD IN WHATEVER ZONE THE MACHINE IS IN, because that is
// the entire point of the change: the formatter reports the SYSTEM's zone rather than assuming one. A
// test that hard-coded "EDT" would pass only on the author's laptop — and it would be asserting the
// wrong property anyway, since the defect being fixed is not "it printed the wrong zone", it is "it
// printed UTC and said nothing".

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestFmtTimeRendersTheLocalWallClockWithItsZone(t *testing.T) {
	instant := time.Date(2026, time.September, 18, 15, 4, 0, 0, time.UTC)
	got := FmtTime(timestamppb.New(instant))

	local := instant.In(time.Local)
	if !strings.Contains(got, local.Format("15:04")) {
		t.Errorf("FmtTime(%v) = %q, want the LOCAL wall clock (%q), not the plane's",
			instant, got, local.Format("2006-01-02 15:04"))
	}
	if !strings.Contains(got, local.Format("2006-01-02")) {
		t.Errorf("FmtTime(%v) = %q, want the local calendar day", instant, got)
	}
	// THE ZONE IS PRINTED. Whatever it is, it is on the row.
	name, _ := local.Zone()
	if !strings.Contains(got, name) {
		t.Errorf("FmtTime(%v) = %q, want the zone name %q beside the time — the operator's whole ask",
			instant, got, name)
	}
}

// THE DEFECT THIS REPLACED, pinning the exact output. FmtTime used to be
// `ts.AsTime().UTC().Format("2006-01-02 15:04")`, so this is the string it produced: a bare UTC wall
// clock with nothing saying so.
func TestFmtTimeIsNotTheUnlabelledUTCForm(t *testing.T) {
	got := FmtTime(timestamppb.New(time.Date(2026, time.January, 2, 3, 4, 0, 0, time.UTC)))
	if got == "2026-01-02 03:04" {
		t.Fatalf("FmtTime still emits the old unlabelled UTC form: %q", got)
	}
}

// nil and invalid keep the detail pane's "absent" marker — a formatter change must not disturb the
// convention every other field shares.
func TestFmtTimeAbsentMarkerUnchanged(t *testing.T) {
	if got := FmtTime(nil); got != "—" {
		t.Errorf("FmtTime(nil) = %q, want the absent marker", got)
	}
}

// THE ZONE LABEL NAMES THE ZONE AND GIVES ITS OFFSET. Driven with FIXED zones so the expectation is a
// constant rather than a property of the machine — including the half-hour offset, which a naive
// "hours only" implementation renders as the wrong time entirely.
func TestZoneLabelNamesTheZoneAndItsOffset(t *testing.T) {
	for _, c := range []struct {
		what string
		loc  *time.Location
		want string
	}{
		{"a whole-hour offset", time.FixedZone("XYZ", -4*3600), "XYZ (UTC-04:00)"},
		{"a half-hour offset", time.FixedZone("IST", 5*3600+1800), "IST (UTC+05:30)"},
		{"a positive offset", time.FixedZone("JST", 9*3600), "JST (UTC+09:00)"},
	} {
		at := time.Date(2026, time.September, 18, 14, 30, 0, 0, c.loc)
		if got := ZoneLabel(at); got != c.want {
			t.Errorf("%s: ZoneLabel = %q, want %q", c.what, got, c.want)
		}
	}
	// UTC says itself ONCE: "UTC (UTC+00:00)" would be saying it twice.
	if got := ZoneLabel(time.Date(2026, time.September, 18, 14, 30, 0, 0, time.UTC)); got != "UTC" {
		t.Errorf("ZoneLabel(UTC) = %q, want \"UTC\"", got)
	}
}

// THE OFFSET COMES FROM THE INSTANT, NOT FROM "NOW". Asking the machine for its offset today and
// applying it to a date on the other side of a DST boundary prints the wrong offset on one of them —
// off by an hour and confidently so, which is the worst kind of wrong for a time.
func TestZoneLabelIsReadFromTheInstantNotFromNow(t *testing.T) {
	// Two instants a year apart in a DST-observing zone. Their offsets must be whatever EACH instant's
	// zone says; the assertion is that the label follows the instant, so the two labels in a DST zone
	// differ, and the label of a past instant is not simply today's.
	cet, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no zone database on this machine — the instant-vs-now property is unobservable here")
	}
	winter := time.Date(2026, time.January, 15, 12, 0, 0, 0, cet) // CET, +01:00
	summer := time.Date(2026, time.July, 15, 12, 0, 0, 0, cet)    // CEST, +02:00

	if got := ZoneLabel(winter); !strings.Contains(got, "UTC+01:00") {
		t.Errorf("January in Berlin = %q, want the winter offset (UTC+01:00) — the label must come from "+
			"the INSTANT, so a January timestamp rendered in July still reads +01:00", got)
	}
	if got := ZoneLabel(summer); !strings.Contains(got, "UTC+02:00") {
		t.Errorf("July in Berlin = %q, want the summer offset (UTC+02:00)", got)
	}
}

// THE FULL FORM IS THE DAY, THE CLOCK, THE ZONE NAME AND THE OFFSET — the string an operator reads when
// they are deciding about a moment rather than scanning past one.
func TestFmtLocalFullCarriesTheClockAndTheZone(t *testing.T) {
	instant := time.Date(2026, time.September, 18, 14, 30, 0, 0, time.UTC)
	got := FmtLocalFull(instant)

	local := instant.In(time.Local)
	if !strings.HasPrefix(got, local.Format("2006-01-02 15:04")) {
		t.Errorf("FmtLocalFull(%v) = %q, want it to open with the local wall clock %q",
			instant, got, local.Format("2006-01-02 15:04"))
	}
	name, _ := local.Zone()
	if !strings.Contains(got, name) {
		t.Errorf("FmtLocalFull(%v) = %q, want the zone name %q", instant, got, name)
	}
	// And the offset is there whenever there IS one (UTC is a zero offset and says so in its name).
	if _, secs := local.Zone(); secs != 0 {
		if !strings.Contains(got, "UTC") {
			t.Errorf("FmtLocalFull(%v) = %q, want the UTC offset beside the zone name", instant, got)
		}
	}
}

// FmtClock keeps the zone, because a clock with no date and no zone is the least decodable thing the TUI
// can draw (the run step flow used to print exactly that, in UTC).
func TestFmtClockKeepsTheZone(t *testing.T) {
	instant := time.Date(2026, time.September, 18, 15, 4, 5, 0, time.UTC)
	got := FmtClock(instant)

	local := instant.In(time.Local)
	if !strings.Contains(got, local.Format("15:04:05")) {
		t.Errorf("FmtClock(%v) = %q, want the local clock %q", instant, got, local.Format("15:04:05"))
	}
	name, _ := local.Zone()
	if !strings.Contains(got, name) {
		t.Errorf("FmtClock(%v) = %q, want the zone name %q", instant, got, name)
	}
}

// LocalZoneLabel reports the SYSTEM's zone, which is the only definition of "local" a process has.
func TestLocalZoneLabelMatchesTheSystemZone(t *testing.T) {
	name, _ := time.Now().Zone()
	if got := LocalZoneLabel(); !strings.HasPrefix(got, name) {
		t.Errorf("LocalZoneLabel() = %q, want it to start with the system zone name %q", got, name)
	}
}
