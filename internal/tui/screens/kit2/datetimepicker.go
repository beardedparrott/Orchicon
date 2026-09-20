package kit2

// datetimepicker.go — a calendar AND a time picker: one modal that chooses an INSTANT.
//
// The operator: "What I would really like is a calendar and time picker as opposed to the canned times.
// The gui has this now."
//
// WHY THE DATE PICKER WAS NOT ENOUGH. kit2.DatePicker (datepicker.go) picks a DAY and returns
// "2006-01-02". A scheduled start is an INSTANT, so the work-item form had to pair that day with a
// TIME — and the only control it had for the time was a list of ten canned presets
// (work screen, schedulePresets(): "in 15 minutes" … "in 1 week", each printing a raw UTC timestamp).
// That list is exactly why the operator's report reads the way it does:
//
//	"Having to type the time in the EXACT format or pick very specific time jumps like 15 minutes is
//	 very limiting."
//
// Where a preset was close, the operator took it and accepted a time they did not want; where none was
// close, they had to type RFC3339 by hand. Neither is choosing. The list is gone, and this is what
// replaced it.
//
// THE SHAPE BEING MATCHED IS THE GUI'S. The GUI's control is `<input type="datetime-local">`
// (frontend/src/routes/work-items_.$id.tsx:613) — ONE control that edits a day and a clock together —
// and not "a date picker plus a separate time field". So this is one modal with both, and the day and
// the clock are edited in place with the keyboard rather than in sequence through two modals.
//
// IT EDITS LOCAL WALL CLOCK AND COMMITS AN INSTANT. The day and the clock are what a person thinks in;
// the wire wants an instant. The components are therefore held in LOCAL terms and the value is built
// through time.Local, which is what makes the zone label ON this modal the truth about what will be
// sent — the operator reads "14:30 EDT (UTC-04:00)" and that is precisely the moment the request
// carries.
//
// DST IS HANDLED BY CONSTRUCTION, not special-cased. Stepping a day goes through AddDate, which walks
// CALENDAR days (so a 23- or 25-hour day still advances the date by one, which is what "tomorrow"
// means); stepping the clock goes through Add on a duration. time.Date then NORMALISES a wall clock
// that does not exist (02:30 on a spring-forward morning), so the modal cannot display a time the
// machine cannot represent, or commit one silently.
//
// WHAT IT DELIBERATELY DOES NOT DO. No preset list, no "in 15 minutes" shortcut, no free-text entry:
// the whole complaint was that the old control made the operator choose between the wrong time and a
// typing test. Arrow keys reach any minute of any day directly, so there is nothing to type and no
// preset to settle for.

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// dtRegion is which half of the modal owns the keys. The two halves are edited in place, so TAB is a
// cursor move and never a commit.
type dtRegion int

const (
	dtDate dtRegion = iota
	dtTime
)

// dtUnit is which field of the clock the vertical keys step.
type dtUnit int

const (
	dtHour dtUnit = iota
	dtMinute
)

const (
	// dateTimePickerBoxW fits the widest thing drawn — the resolved instant, which can run to a long
	// zone name — AND the hint lines, because Pad() TRUNCATES and a truncated hint loses its TAIL, where
	// the chords live. Measured before the first fix: at 44 wide, "esc: cancel" was painted as "esc: ca"
	// and "t today" as "t t". datetimepicker_test asserts every painted line fits.
	dateTimePickerBoxW = 46
	// inner rows: month header, weekday header, up to 6 week rows, blank, the time row, the resolved
	// instant, blank, then five hint rows.
	dateTimePickerBoxH = 19
)

// DateTimePicker is the modal instant chooser. The zero value is not usable; call NewDateTimePicker.
type DateTimePicker struct {
	title string

	year   int
	month  time.Month
	day    int
	hour   int
	minute int

	region dtRegion
	unit   dtUnit

	screenW, screenH int

	// zone is the location the wall clock is read AND written in. It is set by the constructor rather
	// than read from the machine at commit time because a schedule stores its OWN zone: a legacy UTC
	// schedule's 09:00 means 09:00 UTC whatever the operator's clock says, so the modal has to edit and
	// commit in the SCHEDULE's zone — otherwise merely opening and closing it would shift the stored
	// time by the operator's offset, with nobody asking for it.
	zone *time.Location

	// done/committed record the OUTCOME; the HOST closes the modal, for the reason ModelPicker.Done
	// documents (a callback capturing the host model would mutate a copy bubbletea has already
	// replaced).
	done      bool
	committed bool
}

// NewDateTimePicker builds a picker seeded from an instant (zero = now) in the OPERATOR's zone.
func NewDateTimePicker(title string, initial time.Time) *DateTimePicker {
	return NewDateTimePickerIn(title, initial, time.Local)
}

// NewDateTimePickerIn builds a picker that edits and commits in a SPECIFIC zone.
//
// A picker with no zone of its own is right for a one-shot time (the operator chooses a moment and the
// wire takes an instant). A RECURRENCE is different: its wall clock belongs to the schedule's zone, so a
// host editing a recurring definition has to hand the modal that zone — otherwise a legacy UTC schedule
// would be displayed and re-committed in local time, and merely opening and closing the modal would move
// its fire time by the operator's offset without anyone asking for it.
func NewDateTimePickerIn(title string, initial time.Time, zone *time.Location) *DateTimePicker {
	if zone == nil {
		zone = time.Local
	}
	if initial.IsZero() {
		initial = time.Now()
	}
	p := &DateTimePicker{title: title, zone: zone}
	p.setFrom(initial)
	// MINUTES FIRST. dtUnit's zero value is dtHour, and for a schedule the FINE end is the one the
	// arrows are reached for: an operator choosing a start time is almost always nudging by a minute or
	// a few. The hour is then one keystroke away (←) rather than the other way round. Set explicitly,
	// so the default never silently depends on the enum's declaration order.
	p.unit = dtMinute
	return p
}

// setFrom adopts the LOCAL components of an instant — the one entry point for every mutation, so the
// six fields can never be left disagreeing about which instant they describe. "Local" here means the
// picker's own ZONE (see zone), not the machine's.
func (p *DateTimePicker) setFrom(t time.Time) {
	l := t.In(p.loc())
	p.year, p.month, p.day = l.Year(), l.Month(), l.Day()
	p.hour, p.minute = l.Hour(), l.Minute()
}

// loc is the zone the picker edits and commits in, defaulting to the machine's when it was built the
// simple way (NewDateTimePicker) rather than with an explicit one.
func (p *DateTimePicker) loc() *time.Location {
	if p.zone == nil {
		return time.Local
	}
	return p.zone
}

// Zone names the zone the picker is editing in, so a host can say which one it is.
func (p *DateTimePicker) Zone() *time.Location { return p.loc() }

// Done reports that the modal finished and the host must close it.
func (p *DateTimePicker) Done() bool { return p.done }

// Committed reports whether Done was reached by CHOOSING (true) or by cancelling.
func (p *DateTimePicker) Committed() bool { return p.committed }

// Time is the chosen instant. The picker's own zone is what makes the components on screen mean what
// they say.
func (p *DateTimePicker) Time() time.Time {
	return time.Date(p.year, p.month, p.day, p.hour, p.minute, 0, 0, p.loc())
}

// Value is the chosen instant as RFC3339 carrying the LOCAL OFFSET ("2026-09-18T14:30:00-04:00"), not
// a UTC "Z".
//
// Both spellings are valid RFC3339 and both denote the same moment once parsed, so this is not about
// correctness on the wire — it is about what a person reads later. The offset states the zone the
// operator chose in, which is the one fact a bare "Z" throws away, and a stored scheduled start is
// something an operator does read back.
func (p *DateTimePicker) Value() string { return p.Time().Format(time.RFC3339) }

// Label is the chosen instant rendered for a human, in the picker's own zone.
func (p *DateTimePicker) Label() string { return screenkit.FmtZoneFull(p.Time(), p.loc()) }

func (p *DateTimePicker) SetScreen(w, h int) { p.screenW, p.screenH = w, h }

func (p *DateTimePicker) boxSize() (int, int) {
	w, h := dateTimePickerBoxW, dateTimePickerBoxH
	if p.screenW > 0 && p.screenW-4 < w {
		w = p.screenW - 4
	}
	if w < 24 {
		w = 24
	}
	if p.screenH > 0 && p.screenH-2 < h {
		h = p.screenH - 2
	}
	if h < 12 {
		h = 12
	}
	return w, h
}

// --- movement -----------------------------------------------------------------

// shiftDays walks CALENDAR days. AddDate is the right tool rather than adding 24h: on a DST boundary a
// day is 23 or 25 hours, so a duration step would land on the same date (or skip one) twice a year.
func (p *DateTimePicker) shiftDays(n int) { p.setFrom(p.Time().AddDate(0, 0, n)) }

// shiftMonths moves a whole month, CLAMPING the day: the 31st of a 30-day month does not exist, and
// silently landing on "the 31st of September" (which time.Date would roll into October) would commit a
// date the operator never saw highlighted.
func (p *DateTimePicker) shiftMonths(n int) {
	t := time.Date(p.year, p.month, 1, p.hour, p.minute, 0, 0, p.loc()).AddDate(0, n, 0)
	last := daysIn(t.Year(), t.Month())
	d := p.day
	if d > last {
		d = last
	}
	p.year, p.month, p.day = t.Year(), t.Month(), d
}

// stepClock moves the instant by a duration and re-reads the local components, so a carry out of the
// clock lands on the right DAY rather than wrapping the hour and leaving the date behind.
func (p *DateTimePicker) stepClock(d time.Duration) { p.setFrom(p.Time().Add(d)) }

// now jumps to this minute; today keeps the clock and jumps to today's date.
func (p *DateTimePicker) now() { p.setFrom(time.Now()) }

func (p *DateTimePicker) today() {
	n := time.Now()
	p.year, p.month, p.day = n.Year(), n.Month(), n.Day()
}

// --- keys ---------------------------------------------------------------------

func (p *DateTimePicker) HandleKey(k keyMsg) (bool, tea.Cmd) {
	// COMMIT, CANCEL, REGION AND "NOW" ARE REGION-INDEPENDENT: they mean the same thing wherever the
	// cursor is, so they are answered before the region dispatches.
	switch k.String() {
	case "esc":
		p.done, p.committed = true, false
		return true, nil
	case "enter", " ", "space":
		p.done, p.committed = true, true
		return true, nil
	case "tab", "shift+tab":
		if p.region == dtDate {
			p.region = dtTime
		} else {
			p.region = dtDate
		}
		return true, nil
	case "n":
		p.now()
		return true, nil
	}
	if p.region == dtTime {
		return p.timeKey(k)
	}
	return p.dateKey(k)
}

// dateKey navigates the calendar: a day at a time, a week with the vertical arrows, a month with
// PgUp/PgDn — the same vocabulary kit2.DatePicker established, so the two calendars in the TUI are not
// two things to learn.
func (p *DateTimePicker) dateKey(k keyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "left", "h":
		p.shiftDays(-1)
		return true, nil
	case "right", "l":
		p.shiftDays(1)
		return true, nil
	case "up", "k":
		p.shiftDays(-7)
		return true, nil
	case "down", "j":
		p.shiftDays(7)
		return true, nil
	case "pgup":
		p.shiftMonths(-1)
		return true, nil
	case "pgdown":
		p.shiftMonths(1)
		return true, nil
	case "t":
		p.today()
		return true, nil
	}
	return false, nil
}

// timeKey edits the clock.
//
// THE STEPS ARE THE POINT, and they are a ladder rather than one rate. The old control offered only
// 15-minute-ish jumps, which is why "very specific time jumps like 15 minutes" is in the report: an
// operator who wanted 18:07 had no way to say so. Here ↑/↓ move ONE minute, shift+↑/↓ move five, and
// PgUp/PgDn move a quarter hour — so both ends of that complaint are addressable, and accuracy never
// costs a typing test.
func (p *DateTimePicker) timeKey(k keyMsg) (bool, tea.Cmd) {
	unit := time.Minute
	if p.unit == dtHour {
		unit = time.Hour
	}
	switch k.String() {
	case "left", "h":
		p.unit = dtHour
		return true, nil
	case "right", "l":
		p.unit = dtMinute
		return true, nil
	case "up":
		p.stepClock(unit)
		return true, nil
	case "down":
		p.stepClock(-unit)
		return true, nil
	case "shift+up":
		p.stepClock(5 * unit)
		return true, nil
	case "shift+down":
		p.stepClock(-5 * unit)
		return true, nil
	case "pgup":
		p.stepClock(15 * time.Minute)
		return true, nil
	case "pgdown":
		p.stepClock(-15 * time.Minute)
		return true, nil
	}
	return false, nil
}

// --- rendering ----------------------------------------------------------------

// Calendar renders the month grid with the chosen day highlighted and today marked.
func (p *DateTimePicker) Calendar() []string {
	var rows []string

	hdr := fmt.Sprintf("%s %d", p.month.String(), p.year)
	pad := (calendarGridW - lipgloss.Width(hdr)) / 2
	if pad < 0 {
		pad = 0
	}
	rows = append(rows, strings.Repeat(" ", pad)+hdr)

	cells := make([]string, 0, 7)
	for _, d := range datePickerWeekdays {
		cells = append(cells, theme.DetailKey.Render(" "+d+" "))
	}
	rows = append(rows, strings.Join(cells, ""))

	lead := mondayIndex(p.year, p.month)
	last := daysIn(p.year, p.month)
	now := time.Now()
	isThisMonth := now.Year() == p.year && now.Month() == p.month

	day := 1 - lead
	// Six rows is the most any month needs; the loop stops early for a month that needs fewer, and the
	// box pads the difference.
	for week := 0; week < 6 && day <= last; week++ {
		var b strings.Builder
		for col := 0; col < 7; col++ {
			switch {
			case day < 1 || day > last:
				b.WriteString("    ")
			case day == p.day:
				// The CHOSEN day is always marked, whichever region has the keys: it is the selection,
				// and hiding it while the clock is being edited would make the modal look like it had
				// forgotten the date.
				b.WriteString(theme.ListItemSelected.Render(fmt.Sprintf("%3d ", day)))
			case isThisMonth && day == now.Day():
				b.WriteString(theme.StatusOK.Render(fmt.Sprintf("%3d ", day)))
			default:
				b.WriteString(fmt.Sprintf("%3d ", day))
			}
			day++
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return rows
}

// clock renders the time of day, marking which unit the vertical keys will step. The marker is drawn
// ONLY while the TIME region holds the keys, so an unfocused modal never claims a unit is live.
func (p *DateTimePicker) clock() string {
	hh := fmt.Sprintf("%02d", p.hour)
	mm := fmt.Sprintf("%02d", p.minute)
	if p.region != dtTime {
		return hh + ":" + mm
	}
	hi := lipgloss.NewStyle().Foreground(theme.AccentIndigo).Bold(true)
	if p.unit == dtHour {
		return hi.Render(hh) + ":" + mm
	}
	return hh + ":" + hi.Render(mm)
}

// View renders the boxed modal.
func (p *DateTimePicker) View() string {
	w, h := p.boxSize()
	innerW, innerH := w-2, h-2
	if innerW < 10 || innerH < 4 {
		return ""
	}

	rows := p.Calendar()
	// Centre the grid: it is a fixed 7×4-cell block, and leaving it flush left inside a wider box
	// reads as a bug rather than as a layout.
	indent := (innerW - calendarGridW) / 2
	if indent < 0 {
		indent = 0
	}
	gpad := strings.Repeat(" ", indent)
	for i := range rows {
		rows[i] = gpad + rows[i]
	}

	// THE FOCUSED REGION IS MARKED IN TEXT, not only in colour: a cursor a monochrome terminal cannot
	// show is not a cursor, and the region is the one piece of state this modal has to communicate.
	active := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	dim := theme.DetailKey

	// THE HINTS ARE SHORT DELIBERATELY. Pad() truncates, so a hint wider than the box loses its TAIL —
	// and the tail is where the chords are. They are split across lines rather than trimmed, and the
	// fitted-width assertion in datetimepicker_test keeps them honest.
	dateHint := "  ←→ day · ↑↓ week · PgUp/Dn month"
	timeHint := "  ←→ hour/min · ↑↓ step · shift+↑↓ ×5"
	if p.region == dtTime {
		dateHint, timeHint = dim.Render(dateHint), active.Render(timeHint)
	} else {
		dateHint, timeHint = active.Render(dateHint), dim.Render(timeHint)
	}

	// The time row NAMES the region — TAB is discoverable from the modal itself rather than only from a
	// hint underneath — and carries the ▸ marker when the clock holds the keys, so where the arrows act
	// is legible without colour.
	timeRow := "  " + dim.Render("  time  ") + p.clock()
	if p.region == dtTime {
		timeRow = "  " + active.Render("▸ time  ") + p.clock()
	}

	body := make([]string, 0, innerH)
	body = append(body, rows...)
	body = append(body, "")
	body = append(body, timeRow)
	// THE RESOLVED INSTANT, in full — day, clock, zone NAME and UTC offset. This is the line the whole
	// redesign exists for: it is what turns "some time" into a moment the operator can vouch for.
	body = append(body, theme.HintText.Render("  "+p.Label()))
	body = append(body, "")
	body = append(body, dateHint, timeHint)
	body = append(body, theme.HintText.Render("  PgUp/Dn 15m · t: today · n: now"))
	body = append(body, theme.HintText.Render("  tab: switch · enter: choose"))
	body = append(body, theme.HintText.Render("  esc: cancel"))

	border := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	title := ansi.Truncate(p.title, max(0, w-5), "…")
	used := 4 + lipgloss.Width(title)
	dash := w - used - 1
	if dash < 0 {
		dash = 0
	}

	var b strings.Builder
	b.WriteString(border.Render("┌─ ") + theme.MenuTitle.Render(title) +
		border.Render(" "+strings.Repeat("─", dash)+"┐"))
	b.WriteString("\n")
	for i := 0; i < innerH; i++ {
		content := ""
		if i < len(body) {
			content = body[i]
		}
		b.WriteString(border.Render("│") + theme.PanelBgStyle.Render(Pad(content, innerW)) + border.Render("│"))
		b.WriteString("\n")
	}
	b.WriteString(border.Render("└" + strings.Repeat("─", w-2) + "┘"))
	return b.String()
}
