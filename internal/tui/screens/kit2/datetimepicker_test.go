package kit2

// datetimepicker_test.go — the calendar AND clock modal.
//
// The operator: "What I would really like is a calendar and time picker as opposed to the canned times."
//
// Two classes of thing go quietly wrong here, and both are pinned:
//
//	THE ARITHMETIC — a day step that fails to cross a month, a month shift that invents a date, a clock
//	step that wraps 23:59 back to 00:00 and abandons the date. Calendar maths is where this kind of
//	widget lies, so the expectations are written against known-correct dates rather than against the
//	widget's own output.
//
//	THE ZONE — the value leaving the modal must denote the moment the operator SAW. The components are
//	local wall clock, so these assertions round-trip through the local zone rather than assuming one;
//	written the other way they would pass only on the author's machine, which is precisely the class of
//	bug the whole change is about.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dtAt builds a picker at a LOCAL wall-clock moment.
func dtAt(y int, m time.Month, d, hh, mm int) *DateTimePicker {
	return NewDateTimePicker("Scheduled start", time.Date(y, m, d, hh, mm, 0, 0, time.Local))
}

// dtKey sends one key to the picker and discards the outcome (the outcome is asserted from the state).
func dtKey(p *DateTimePicker, k tea.KeyMsg) { p.HandleKey(k) }

// tabToTime hands the keys to the clock — the only route to the time keys.
func tabToTime(p *DateTimePicker) { dtKey(p, tea.KeyMsg{Type: tea.KeyTab}) }

// runeKey is a plain letter.
func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestDateTimePickerSeedsFromTheInstantInLocalTime(t *testing.T) {
	seed := time.Date(2026, time.September, 18, 14, 30, 0, 0, time.Local)
	p := NewDateTimePicker("t", seed)

	if got := p.Time(); !got.Equal(seed) {
		t.Fatalf("Time() = %v, want the seed %v", got, seed)
	}
	if p.hour != 14 || p.minute != 30 || p.day != 18 {
		t.Errorf("components = %d:%02d on day %d, want 14:30 on the 18th (LOCAL wall clock)",
			p.hour, p.minute, p.day)
	}
}

// A ZERO SEED OPENS ON NOW, so the common case — scheduling something soon — starts where the operator
// is, rather than on the zero date.
func TestDateTimePickerZeroSeedIsNow(t *testing.T) {
	p := NewDateTimePicker("t", time.Time{})
	now := time.Now()
	if p.year != now.Year() || p.month != now.Month() || p.day != now.Day() {
		t.Errorf("a zero seed opened on %v, want today", p.Time())
	}
}

// THE VALUE ROUND-TRIPS TO THE MOMENT ON SCREEN. This is the property that matters: whatever the
// machine's zone is, what leaves the modal is the instant the operator picked.
func TestDateTimePickerValueRoundTripsThroughTheLocalOffset(t *testing.T) {
	p := dtAt(2026, time.September, 18, 14, 30)
	got := p.Value()

	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("Value() = %q is not RFC3339: %v", got, err)
	}
	if !parsed.Equal(p.Time()) {
		t.Fatalf("Value() = %q parses to %v, want the chosen instant %v", got, parsed, p.Time())
	}
	// AND IT IS NOT RE-SPELLED IN UTC. Both spellings denote the same instant, but this string is what
	// a person reads back later, and the local one carries the zone it was chosen in.
	want := time.Date(2026, time.September, 18, 14, 30, 0, 0, time.Local).Format(time.RFC3339)
	if got != want {
		t.Errorf("Value() = %q, want the local spelling %q", got, want)
	}
}

// ---------------------------------------------------------------- the calendar

func TestDateTimePickerMovesByDayWeekAndMonth(t *testing.T) {
	p := dtAt(2026, time.September, 18, 9, 0)

	dtKey(p, tea.KeyMsg{Type: tea.KeyRight})
	if p.day != 19 {
		t.Fatalf("right → day %d, want 19", p.day)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyLeft})
	if p.day != 18 {
		t.Fatalf("left → day %d, want back to 18", p.day)
	}
	// The vertical arrows are a WEEK, which is how a calendar is normally read.
	dtKey(p, tea.KeyMsg{Type: tea.KeyDown})
	if p.day != 25 {
		t.Fatalf("down → day %d, want a week on (25)", p.day)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyUp})
	if p.day != 18 {
		t.Fatalf("up → day %d, want back to 18", p.day)
	}
	// PgUp/PgDn are a MONTH.
	dtKey(p, tea.KeyMsg{Type: tea.KeyPgDown})
	if p.month != time.October || p.day != 18 {
		t.Fatalf("PgDn → %s %d, want October 18", p.month, p.day)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyPgUp})
	if p.month != time.September || p.day != 18 {
		t.Fatalf("PgUp → %s %d, want September 18", p.month, p.day)
	}
}

// A DAY STEP CROSSES THE MONTH rather than clamping at its end — the difference between "tomorrow" and
// "the last day of this month, forever".
func TestDateTimePickerDayStepCrossesTheMonth(t *testing.T) {
	p := dtAt(2026, time.September, 30, 9, 0)
	dtKey(p, tea.KeyMsg{Type: tea.KeyRight})
	if p.month != time.October || p.day != 1 {
		t.Fatalf("Sep 30 + 1 day = %s %d, want October 1", p.month, p.day)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyLeft})
	if p.month != time.September || p.day != 30 {
		t.Fatalf("back = %s %d, want September 30", p.month, p.day)
	}
}

// A MONTH SHIFT CLAMPS THE DAY: 31 January → 28 February, never "31 February" rolled silently into
// March — the operator never saw a March day highlighted, so committing one would be a jump.
func TestDateTimePickerMonthShiftClampsTheDay(t *testing.T) {
	p := dtAt(2026, time.January, 31, 9, 0)
	dtKey(p, tea.KeyMsg{Type: tea.KeyPgDown})
	if p.month != time.February || p.day != 28 {
		t.Fatalf("Jan 31 + 1 month = %s %d, want February 28", p.month, p.day)
	}
}

func TestDateTimePickerTodayKeepsTheClock(t *testing.T) {
	p := dtAt(2020, time.March, 3, 7, 45)
	dtKey(p, runeKey("t"))

	now := time.Now()
	if p.year != now.Year() || p.month != now.Month() || p.day != now.Day() {
		t.Errorf("t → %v, want today", p.Time())
	}
	if p.hour != 7 || p.minute != 45 {
		t.Errorf("t moved the clock to %02d:%02d — today is a DATE move only, and the time the operator "+
			"had chosen must survive it", p.hour, p.minute)
	}
}

func TestDateTimePickerNowSetsTheDateAndTheClock(t *testing.T) {
	p := dtAt(2020, time.March, 3, 7, 45)
	dtKey(p, runeKey("n"))

	now := time.Now()
	got := p.Time()
	if got.Year() != now.Year() || got.Month() != now.Month() || got.Day() != now.Day() ||
		got.Hour() != now.Hour() || got.Minute() != now.Minute() {
		t.Errorf("n → %v, want this minute (%v)", got, now)
	}
}

// ------------------------------------------------------------------- the clock

// TAB MOVES THE KEYS BETWEEN THE CALENDAR AND THE CLOCK; it never commits. Tab-is-a-cursor-move is what
// lets the day and the time be edited in one modal instead of two.
func TestDateTimePickerTabSwitchesRegionWithoutCommitting(t *testing.T) {
	p := dtAt(2026, time.September, 18, 9, 0)
	if p.region != dtDate {
		t.Fatal("the calendar must own the keys first")
	}
	tabToTime(p)
	if p.region != dtTime {
		t.Fatal("tab must hand the keys to the clock")
	}
	if p.Done() {
		t.Fatal("tab must not finish the modal")
	}
	tabToTime(p)
	if p.region != dtDate {
		t.Fatal("tab must switch back to the calendar")
	}
}

// THE STEP LADDER. The report was "very specific time jumps like 15 minutes is very limiting"; the
// answer is that every one of these is reachable, so no time is unreachable and none costs a typing test.
func TestDateTimePickerClockSteps(t *testing.T) {
	p := dtAt(2026, time.September, 18, 9, 0)
	tabToTime(p)

	// The MINUTE is the default unit: one minute, five with shift.
	dtKey(p, tea.KeyMsg{Type: tea.KeyUp})
	if p.minute != 1 {
		t.Fatalf("↑ = %02d:%02d, want +1 minute", p.hour, p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyShiftUp})
	if p.minute != 6 {
		t.Fatalf("shift+↑ = :%02d, want +5 minutes", p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyShiftDown})
	if p.minute != 1 {
		t.Fatalf("shift+↓ = :%02d, want back to 1", p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyDown})
	if p.minute != 0 {
		t.Fatalf("↓ = :%02d, want back to 0", p.minute)
	}
	// The quarter-hour nudge, available whichever unit is selected.
	dtKey(p, tea.KeyMsg{Type: tea.KeyPgUp})
	if p.minute != 15 {
		t.Fatalf("PgUp = %02d:%02d, want :15", p.hour, p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyPgDown})
	if p.minute != 0 {
		t.Fatalf("PgDn = :%02d, want back to :00", p.minute)
	}
	// The HOUR unit moves whole hours.
	dtKey(p, runeKey("h"))
	if p.unit != dtHour {
		t.Fatal("h must select the hour")
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyUp})
	if p.hour != 10 || p.minute != 0 {
		t.Fatalf("↑ on the hour = %02d:%02d, want 10:00", p.hour, p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyDown})
	if p.hour != 9 {
		t.Fatalf("↓ on the hour = %d, want back to 9", p.hour)
	}
}

// A CARRY OUT OF THE CLOCK LANDS ON THE RIGHT DAY. Wrapping the hour to 00:00 and leaving the date
// behind would commit a time a full day away from the one on screen — and the resolved line would show
// it, so the modal would be contradicting itself.
func TestDateTimePickerClockCarryMovesTheDate(t *testing.T) {
	p := dtAt(2026, time.September, 18, 23, 59)
	tabToTime(p)

	dtKey(p, tea.KeyMsg{Type: tea.KeyUp})
	if p.day != 19 || p.hour != 0 || p.minute != 0 {
		t.Fatalf("23:59 + 1 minute = day %d %02d:%02d, want the 19th at 00:00", p.day, p.hour, p.minute)
	}
	dtKey(p, tea.KeyMsg{Type: tea.KeyDown})
	if p.day != 18 || p.hour != 23 || p.minute != 59 {
		t.Fatalf("back = day %d %02d:%02d, want the 18th at 23:59", p.day, p.hour, p.minute)
	}
}

// -------------------------------------------------------------- commit / cancel

func TestDateTimePickerEnterCommitsAndEscapeCancels(t *testing.T) {
	committed := dtAt(2026, time.September, 18, 9, 0)
	dtKey(committed, tea.KeyMsg{Type: tea.KeyEnter})
	if !committed.Done() || !committed.Committed() {
		t.Error("enter must finish the modal with a committed value")
	}

	cancelled := dtAt(2026, time.September, 18, 9, 0)
	dtKey(cancelled, tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled.Done() {
		t.Error("esc must finish the modal")
	}
	if cancelled.Committed() {
		t.Error("esc must finish the modal as CANCELLED, or the host would write the cursor position back " +
			"as if the operator had chosen it")
	}
}

// ------------------------------------------------------------------ rendering

// THE MODAL PRINTS THE RESOLVED INSTANT WITH ITS ZONE. That line is why the box is wider than the
// calendar: it is what turns "some time" into a moment the operator can vouch for.
func TestDateTimePickerViewShowsTheResolvedInstantWithItsZone(t *testing.T) {
	p := dtAt(2026, time.September, 18, 14, 30)
	p.SetScreen(120, 40)
	plain := stripDTANSI(p.View())

	if !strings.Contains(plain, "2026-09-18 14:30") {
		t.Errorf("the modal does not print the chosen instant:\n%s", plain)
	}
	name, _ := time.Date(2026, time.September, 18, 14, 30, 0, 0, time.Local).Zone()
	if !strings.Contains(plain, name) {
		t.Errorf("the modal does not name the zone (%q), so the operator cannot tell which zone the time "+
			"is in:\n%s", name, plain)
	}
	if !strings.Contains(plain, "September 2026") {
		t.Errorf("the calendar is not drawn for the chosen month:\n%s", plain)
	}
	// The chosen day is marked, so the modal says which date is selected rather than only which month.
	if !strings.Contains(plain, "18") {
		t.Errorf("the chosen day is not on the grid:\n%s", plain)
	}
}

// EVERY PAINTED LINE FITS THE TERMINAL. A modal that overflows its own border reads as a broken frame.
func TestDateTimePickerViewFitsItsWidth(t *testing.T) {
	for _, w := range []int{40, 60, 120, 200} {
		p := dtAt(2026, time.September, 18, 14, 30)
		p.SetScreen(w, 40)
		for _, ln := range strings.Split(p.View(), "\n") {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("screen %d: a painted line is %d cells wide: %q", w, got, ln)
			}
		}
	}
}

// NOTHING IS TRUNCATED. Pad() cuts a line that is too wide, and the cut lands on the TAIL — which is
// where the chords are. The first version of this box painted "esc: ca" and "t t", so every chord the
// modal advertises is asserted PRESENT rather than merely "some hint text exists".
func TestDateTimePickerViewDoesNotTruncateItsChords(t *testing.T) {
	p := dtAt(2026, time.September, 18, 14, 30)
	p.SetScreen(120, 40)
	plain := stripDTANSI(p.View())

	for _, want := range []string{
		"esc: cancel", "enter: choose", "tab: switch", "t: today", "n: now",
		"15m", "day", "week", "month", "hour/min",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("the painted modal is missing %q — a chord painted short is a chord the operator "+
				"cannot find:\n%s", want, plain)
		}
	}
}

// stripDTANSI removes SGR sequences so the assertions read the modal's text, not the renderer's escapes.
func stripDTANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
