package screenkit

import (
	"fmt"
	"time"
)

// Shared field formatters for detail panes (all screens import these so
// timestamps and numbers render identically).

// --- TIME IS LOCAL, AND ALWAYS LABELLED -------------------------------------
//
// The operator: "I also would like all times in orch to actually show local time and print the time
// zone by the time so people truly know what timezone it is reporting in. This should be able to be
// gathered from the system."
//
// FmtTime USED TO BE:
//
//	return ts.AsTime().UTC().Format("2006-01-02 15:04")
//
// and there were two defects in it, not one.
//
//	1. IT PRINTED UTC. AsTime() yields the instant, so formatting it "in UTC" prints the PLANE's wall
//	   clock. An operator in New York reading "2026-09-18 22:30" for a run they started at 18:30 had
//	   to do the arithmetic themselves — and, worse, had no way to know they needed to. Nothing on the
//	   row said which zone it was in, so the only cue was the number looking wrong.
//
//	2. IT LABELLED NOTHING. Even a CORRECT local render is ambiguous on its own: "14:30" is not a time
//	   until you say 14:30 WHERE. And for part of the year an abbreviation is not the whole story
//	   either — a wall clock read across a DST boundary denotes two different instants, which is why
//	   the full form carries the offset as well as the name.
//
// So every time rendered here is LOCAL and carries its zone. There is ONE layout per shape, named in
// the constants below, so the panes cannot drift apart: a second Format("…") call anywhere in the TUI
// is how two screens end up disagreeing about what a timestamp looks like.
//
// WHERE "LOCAL" COMES FROM. time.Local — the operator's machine — which Go resolves from the TZ
// environment variable, else /etc/localtime. That is the only definition of "local" a process has, and
// it is the same rule the GUI follows in practice (the browser's toLocaleString / toLocaleTimeString),
// so the two clients agree about what local means without either being told. No tenant setting is
// consulted because none exists (grep: there is no timezone field in the schema).
//
// ONE CONSEQUENCE WORTH STATING PLAINLY. If the TUI runs where there is no zone database and no
// /etc/localtime, Go falls back to UTC and these formatters print "UTC". That is now VISIBLE on every
// row rather than silently wrong — which is the entire point of printing the zone at all.

const (
	// timeLayout is a full local timestamp with its zone abbreviation ("2026-09-18 14:30 EDT").
	timeLayout = "2006-01-02 15:04 MST"
	// clockLayout is a time of day with its zone ("14:30:05 EDT") — for rows that sit under a header
	// which already carries the date, so repeating the date would be noise.
	clockLayout = "15:04:05 MST"
	// dateLayout is a calendar DAY. A day is not an instant, so it carries no zone.
	dateLayout = "2006-01-02"
)

// Timestamp is anything that yields an instant. The generated protobuf timestamps satisfy it (their
// AsTime is nil-safe and IsValid reports a malformed one), and it is kept as an interface so the
// callers did not have to change SHAPE when the rendering rule did.
type Timestamp interface {
	AsTime() time.Time
	IsValid() bool
}

// FmtTime renders a protobuf timestamp as LOCAL wall clock with its zone ("2026-09-18 14:30 EDT").
// nil or invalid → "—", which is the same "absent" marker the rest of the detail pane uses.
func FmtTime(ts Timestamp) string {
	if ts == nil || !ts.IsValid() {
		return "—"
	}
	return FmtLocal(ts.AsTime())
}

// FmtLocal renders an instant as local wall clock with its zone abbreviation.
func FmtLocal(t time.Time) string { return t.In(time.Local).Format(timeLayout) }

// FmtLocalFull renders an instant as local wall clock with the zone NAME and its UTC offset:
//
//	2026-09-18 14:30 EDT (UTC-04:00)
//
// THE LONG FORM IS FOR MOMENTS THE OPERATOR IS DECIDING ABOUT, not scanning past: choosing a scheduled
// start, confirming a time. The extra characters buy away the last ambiguity, and an abbreviation
// alone cannot do that — "EST" and "EDT" name two different offsets, and a bare offset cannot be read
// back into a zone.
func FmtLocalFull(t time.Time) string { return FmtZoneFull(t, time.Local) }

// FmtZoneFull renders an instant in a GIVEN zone, naming the zone and its offset.
//
// It exists because "local" is not always the right zone to show. A recurring schedule stores its own
// zone, and its wall clock belongs to THAT zone: rendering a legacy UTC schedule's 09:00 through
// FmtLocalFull would print the operator's local conversion of it — a different wall clock from the one
// stored, which the form would then read back and save. A host showing a schedule's times passes the
// schedule's zone here.
func FmtZoneFull(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	l := t.In(loc)
	return l.Format(timeLayout) + offsetSuffix(l)
}

// FmtClock renders an instant as a local time of day with its zone.
func FmtClock(t time.Time) string { return t.In(time.Local).Format(clockLayout) }

// FmtDate renders an instant as a local calendar day.
func FmtDate(t time.Time) string { return t.In(time.Local).Format(dateLayout) }

// LocalZone is the system's zone name and its current offset from UTC.
func LocalZone() (string, time.Duration) {
	name, secs := time.Now().Zone()
	return name, time.Duration(secs) * time.Second
}

// LocalZoneLabel names the operator's current zone and gives its offset: "EDT (UTC-04:00)", or "UTC"
// where that is all there is to say.
func LocalZoneLabel() string { return ZoneLabel(time.Now()) }

// ZoneLabel names the zone AT A GIVEN INSTANT and gives its offset.
//
// IT TAKES THE INSTANT RATHER THAN READING "NOW", and that is deliberate: an offset is a property of
// the MOMENT, not of the machine. Asking "now" and applying the answer to a January timestamp prints
// "EDT" on a winter date — an hour out, and confidently so, which is the worst kind of wrong here.
// Every caller has the instant in hand, so every caller passes it.
func ZoneLabel(t time.Time) string {
	name, _ := t.Zone()
	if name == "" {
		name = "UTC"
	}
	return name + offsetSuffix(t)
}

// offsetSuffix renders the zone offset as a suffix (" (UTC-04:00)"), or nothing at all for UTC —
// where "UTC (UTC+00:00)" would be saying it twice.
func offsetSuffix(t time.Time) string {
	_, secs := t.Zone()
	if secs == 0 {
		return ""
	}
	return " (" + utcOffset(secs) + ")"
}

// utcOffset renders a zone offset the way a person writes it: "UTC-04:00", "UTC+05:30".
func utcOffset(secs int) string {
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, secs/3600, (secs%3600)/60)
}

// FmtInt renders an integer.
func FmtInt(n int) string { return fmt.Sprintf("%d", n) }

// FmtInt64 renders an int64.
func FmtInt64(n int64) string { return fmt.Sprintf("%d", n) }

// FmtBool renders a bool as yes/no.
func FmtBool(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// FmtDuration renders seconds as a compact duration ("90s", "2m30s").
func FmtDuration(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}
	d := time.Duration(seconds) * time.Second
	return d.String()
}
