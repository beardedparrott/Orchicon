package automation

// schedule_start.go — the recurring form's combined DATE + TIME control.
//
// The operator, looking at the edit form: "On recurring schedules it specifically says UTC. Did we change
// this? I see it has the calendar picker which is awesome by the way. I wonder if we should combine start
// date and time in the same type of calendar picker you made for work item schedules. That would be
// nice."
//
// THE UTC LABEL WAS WORKING AS INTENDED, and commit ef097df0 explains why it is there. A recurring
// schedule stores a WALL CLOCK plus the zone it is expressed in, and this item is a LEGACY one: it was
// written before the timezone field existed, carries none, and the server reads an absent zone as UTC so
// that its fire time is UNCHANGED. The operator asked for exactly that ("Leave them. I don't want things
// firing again"). So the label says so rather than implying the times are local — a silent "UTC or
// local?" is the bug the whole field exists to remove. A NEW schedule stamps the system zone and the
// label reads e.g. "America/Chicago".
//
// THE COMBINED CONTROL IS THE CHANGE HERE. The schedule's start is one MOMENT but two STORED STRINGS
// (start_date "YYYY-MM-DD" and start_time "HH:MM"), because that is the shape the recurring_schedule
// JSONB and the server's validator take. Previously the form made the operator visit a calendar for the
// date and a bare text box for the time, which is two controls describing one thing — and the text box
// was the one that demanded HH:MM exactly. The modal now edits the moment and the host writes it back
// as the pair.

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// openScheduleStart opens the combined calendar + clock for the recurring form's start, seeded from the
// date and time fields currently in it.
//
// THE SEED IS BUILT IN THE SCHEDULE'S ZONE, not the machine's. A legacy schedule's 09:00 means 09:00
// UTC, so seeding the modal from the operator's local "now" would show them a wall clock that does not
// match the one on the form. Building the seed through kit2.ThemedZone keeps the modal's resolved-instant
// line honest for the schedule being edited, whatever zone that is.
//
// An unparseable or empty pair falls back to now (the picker's own zero-value behaviour), so the common
// case — a new schedule, or a field the operator has not touched — opens somewhere sensible instead of on
// the zero date.
func (m *Model) openScheduleStart(_ string, _ string) tea.Cmd {
	f := m.ActiveForm()
	if f == nil {
		return nil
	}
	loc := scheduleZoneLocation(m.formZone)
	initial := seedTime(f.Values["start_date"], f.Values["start_time"], loc)

	m.dtStartField = "start_date"
	m.dtEndField = "start_time"
	p := kit2.NewDateTimePickerIn("Schedule start", initial, loc)
	p.SetScreen(m.w, m.h)
	m.dtPicker = p
	return nil
}

// finishDateTimePicker closes the modal when it reports Done and SPLITS the chosen instant back into the
// two fields the schedule stores.
//
// IT SPLITS IN THE SCHEDULE'S ZONE, which is the whole reason the zone is carried. The modal edits a wall
// clock ("09:00") and commits an instant; the schedule wants that wall clock back as a string, so the
// components are read out again in the SAME zone they were seeded in. Reading them out in the machine's
// zone would shift a legacy UTC schedule by the operator's offset every time they opened and closed the
// modal without meaning to change anything.
//
// A CANCELLED modal writes nothing: esc must leave both fields exactly as they were.
func (m *Model) finishDateTimePicker() tea.Cmd {
	if m.dtPicker == nil || !m.dtPicker.Done() {
		return nil
	}
	chosen, committed := m.dtPicker.Time(), m.dtPicker.Committed()
	dateField, timeField := m.dtStartField, m.dtEndField
	m.dtPicker, m.dtStartField, m.dtEndField = nil, "", ""
	if !committed {
		return nil
	}
	f := m.ActiveForm()
	if f == nil {
		return nil
	}
	local := chosen.In(scheduleZoneLocation(m.formZone))
	f.Set(dateField, local.Format("2006-01-02"))
	f.Set(timeField, local.Format("15:04"))
	return nil
}

// scheduleZoneLocation resolves the form's zone name to a location for the seed and the split. Empty (the
// legacy schedule) is UTC — the same reading the server applies, so the modal and the wire cannot
// disagree about what the wall clock means.
func scheduleZoneLocation(zone string) *time.Location {
	if strings.TrimSpace(zone) == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// seedTime builds the instant a schedule's stored date and time denote in its zone, or the zero time when
// either is missing or malformed (which the picker reads as "now").
func seedTime(date, clock string, loc *time.Location) time.Time {
	d := strings.TrimSpace(date)
	t := strings.TrimSpace(clock)
	if d == "" || t == "" {
		return time.Time{}
	}
	at, err := time.ParseInLocation("2006-01-02 15:04", d+" "+t, loc)
	if err != nil {
		return time.Time{}
	}
	return at
}
