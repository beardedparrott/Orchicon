package kit2

// datepicker_test.go — the calendar maths.
//
// Calendar arithmetic is where this kind of widget goes quietly wrong: an off-by-one
// in the weekday offset puts every date in the wrong column, and a month shift that
// does not clamp invents a date ("31 September") the operator never saw highlighted.
// Both are pinned here against known-correct dates.

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func dpAt(y int, m time.Month, d int) *DatePicker {
	return NewDatePicker("Start date", time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

func TestDatePickerValueIsTheWireFormat(t *testing.T) {
	dp := dpAt(2026, time.September, 1)
	if got := dp.Value(); got != "2026-09-01" {
		t.Fatalf("Value = %q, want the schedule's YYYY-MM-DD", got)
	}
	// Single-digit months and days are zero-padded, or the server rejects it.
	dp2 := dpAt(2026, time.January, 5)
	if got := dp2.Value(); got != "2026-01-05" {
		t.Fatalf("Value = %q, want zero padding", got)
	}
}

// The month length, including the century-leap rules.
func TestDaysInMonth(t *testing.T) {
	for _, c := range []struct {
		y int
		m time.Month
		w int
	}{
		{2026, time.January, 31},
		{2026, time.February, 28},
		{2028, time.February, 29}, // leap
		{2027, time.February, 28},
		{2000, time.February, 29}, // century leap
		{1900, time.February, 28}, // century non-leap
		{2026, time.April, 30},
		{2026, time.September, 30},
		{2026, time.December, 31},
	} {
		if got := daysIn(c.y, c.m); got != c.w {
			t.Errorf("daysIn(%d, %s) = %d, want %d", c.y, c.m, got, c.w)
		}
	}
}

// mondayIndex is the column of the 1st, Monday-first. Verified against real dates.
func TestMondayIndexForKnownMonthStarts(t *testing.T) {
	for _, c := range []struct {
		y    int
		m    time.Month
		want int
	}{
		// 2026-09-01 is a Tuesday → column 1 (Mon=0).
		{2026, time.September, 1},
		// 2026-11-01 is a Sunday → column 6.
		{2026, time.November, 6},
		// 2027-02-01 is a Monday → column 0.
		{2027, time.February, 0},
	} {
		if got := mondayIndex(c.y, c.m); got != c.want {
			t.Errorf("mondayIndex(%d, %s) = %d, want %d", c.y, c.m, got, c.want)
		}
	}
}

// Day stepping crosses month and year boundaries correctly — the arrows are the
// primary way to move, so this is the path that gets exercised most.
func TestDatePickerShiftCrossesBoundaries(t *testing.T) {
	dp := dpAt(2026, time.September, 30)
	dp.shift(1)
	if dp.Value() != "2026-10-01" {
		t.Fatalf("30 Sep + 1 day = %q, want 2026-10-01", dp.Value())
	}
	dp = dpAt(2026, time.December, 31)
	dp.shift(1)
	if dp.Value() != "2027-01-01" {
		t.Fatalf("31 Dec + 1 day = %q, want 2027-01-01 (the year must roll)", dp.Value())
	}
	dp = dpAt(2026, time.January, 1)
	dp.shift(-1)
	if dp.Value() != "2025-12-31" {
		t.Fatalf("1 Jan − 1 day = %q, want 2025-12-31", dp.Value())
	}
	// A whole week is the up/down step.
	dp = dpAt(2026, time.September, 1)
	dp.shift(7)
	if dp.Value() != "2026-09-08" {
		t.Fatalf("+7 days = %q, want 2026-09-08", dp.Value())
	}
}

// A month shift CLAMPS the cursor rather than rolling into the next month: the 31st
// of September does not exist, and time.Date would silently give 1 October — a date
// the operator never saw highlighted.
func TestDatePickerMonthShiftClampsTheCursor(t *testing.T) {
	dp := dpAt(2026, time.August, 31)
	dp.shiftMonth(1) // August 31 → September (30 days)
	if dp.Value() != "2026-09-30" {
		t.Fatalf("31 Aug, next month = %q, want 2026-09-30 (clamped, not 2026-10-01)", dp.Value())
	}
	// And the other way: a 30-day month into a 31-day one keeps the day.
	dp = dpAt(2026, time.September, 30)
	dp.shiftMonth(1)
	if dp.Value() != "2026-10-30" {
		t.Fatalf("30 Sep, next month = %q, want 2026-10-30", dp.Value())
	}
	// Rolling back over a year boundary.
	dp = dpAt(2026, time.January, 15)
	dp.shiftMonth(-1)
	if dp.Value() != "2025-12-15" {
		t.Fatalf("15 Jan, previous month = %q, want 2025-12-15", dp.Value())
	}
	// February is the sharpest case: 31 Jan → 28 Feb.
	dp = dpAt(2026, time.January, 31)
	dp.shiftMonth(1)
	if dp.Value() != "2026-02-28" {
		t.Fatalf("31 Jan, next month = %q, want 2026-02-28", dp.Value())
	}
}

// The outcome contract: enter chooses, esc cancels, and each flips Done so the
// HOST can close the modal. This is the pattern that was once broken on the model
// picker (a callback capturing a discarded model copy), so it is pinned.
func TestDatePickerReportsItsOutcome(t *testing.T) {
	dp := dpAt(2026, time.September, 1)
	if dp.Done() {
		t.Fatal("a freshly opened picker must not report Done")
	}
	dp.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !dp.Done() || !dp.Committed() {
		t.Fatal("enter must report Done+Committed so the host closes it and keeps the date")
	}

	dp2 := dpAt(2026, time.September, 1)
	dp2.HandleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !dp2.Done() || dp2.Committed() {
		t.Fatal("esc must report Done without Committed")
	}
}

// 't' jumps to today, so the operator never has to page through months to reach a
// date they are scheduling from now.
func TestDatePickerTodayKey(t *testing.T) {
	dp := dpAt(2000, time.January, 1)
	dp.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	now := time.Now()
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	if got := dp.Value(); got != want {
		t.Fatalf("t landed on %q, want today (%q)", got, want)
	}
}

// The grid is what the operator actually reads, so assert it shows the month and
// every day — a mis-sized grid is how "I can't see the 30th" happens.
func TestCalendarShowsTheWholeMonth(t *testing.T) {
	dp := dpAt(2026, time.September, 1)
	dp.SetScreen(120, 40)
	rows := dp.Calendar()
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"September 2026", "Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"} {
		if !strings.Contains(joined, want) {
			t.Errorf("calendar is missing %q:\n%s", want, joined)
		}
	}
	for day := 1; day <= 30; day++ {
		if !strings.Contains(joined, " ") || !strings.Contains(joined, itoaDay(day)) {
			t.Errorf("calendar does not show day %d:\n%s", day, joined)
		}
	}
	// September has 30 days: the 31st must NOT appear.
	if strings.Contains(joined, " 31") {
		t.Errorf("September must not render a 31st:\n%s", joined)
	}
}

func itoaDay(d int) string {
	if d < 10 {
		return " " + string(rune('0'+d))
	}
	return string(rune('0'+d/10)) + string(rune('0'+d%10))
}

func TestDatePickerViewFitsItsBox(t *testing.T) {
	dp := dpAt(2026, time.September, 17)
	dp.SetScreen(120, 40)
	v := dp.View()
	if v == "" {
		t.Fatal("View returned nothing at a normal screen size")
	}
	w, h := dp.boxSize()
	lines := strings.Split(v, "\n")
	if len(lines) != h {
		t.Fatalf("View is %d rows, want the box height %d", len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Fatalf("row %d is %d cells, want %d", i, got, w)
		}
	}
}
