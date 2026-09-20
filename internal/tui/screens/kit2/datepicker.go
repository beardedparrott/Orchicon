// datepicker.go — a calendar DATE picker modal.
//
// The GUI gets this free from the browser (`<input type="date">`), so its
// RecurringScheduleForm is a `type="date"` control. A terminal has no such thing,
// and typing `2026-09-01` into a text box is the worst of both worlds: the format
// must be remembered, a typo is only caught at validation, and there is no way to
// see that the 14th is a Saturday.
//
// So this is a real calendar, navigated the way a calendar is: arrows move by a
// day or a week, PgUp/PgDn by a month, and the grid shows the month you are
// choosing from. That is the TUI-native form of the GUI's control, and it is the
// same widget the model picker established (a modal with Done/Committed outcome
// that the HOST closes — see ModelPicker for why the host owns that).
package kit2

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

const (
	datePickerBoxW = 42
	// title row + weekday header + up to 6 week rows + blank + 2 hint rows.
	datePickerBoxH = 12
	// calendarGridW is the grid's exact width: 7 columns × 4 cells.
	calendarGridW = 28
)

// DatePicker is the modal date chooser. The zero value is not usable; call NewDatePicker.
type DatePicker struct {
	title string

	year   int
	month  time.Month
	cursor int // highlighted day of month (1..31)

	screenW, screenH int

	// done/committed record the OUTCOME; the HOST closes the modal (a callback
	// capturing the host model would mutate a copy bubbletea has already replaced
	// — see ModelPicker.Done for the full reasoning).
	done      bool
	committed bool
}

// NewDatePicker builds a picker showing the given date (zero time = today).
func NewDatePicker(title string, initial time.Time) *DatePicker {
	if initial.IsZero() {
		initial = time.Now()
	}
	return &DatePicker{
		title:  title,
		year:   initial.Year(),
		month:  initial.Month(),
		cursor: initial.Day(),
	}
}

// Done reports that the modal finished and the host must close it.
func (dp *DatePicker) Done() bool { return dp.done }

// Committed reports whether Done was reached by CHOOSING (true) or cancelling.
func (dp *DatePicker) Committed() bool { return dp.committed }

// Value is the chosen date as YYYY-MM-DD — the wire format the schedule uses.
func (dp *DatePicker) Value() string {
	return fmt.Sprintf("%04d-%02d-%02d", dp.year, int(dp.month), dp.cursor)
}

func (dp *DatePicker) SetScreen(w, h int) { dp.screenW, dp.screenH = w, h }

func (dp *DatePicker) boxSize() (int, int) {
	w, h := datePickerBoxW, datePickerBoxH
	if dp.screenW > 0 && dp.screenW-4 < w {
		w = dp.screenW - 4
	}
	if w < 24 {
		w = 24
	}
	if dp.screenH > 0 && dp.screenH-2 < h {
		h = dp.screenH - 2
	}
	if h < 8 {
		h = 8
	}
	return w, h
}

// shift moves the cursor by whole days, normalising across month and year
// boundaries through time.Date (which handles the overflow rather than needing it
// spelled out).
func (dp *DatePicker) shift(days int) {
	t := time.Date(dp.year, dp.month, dp.cursor, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)
	dp.year, dp.month, dp.cursor = t.Year(), t.Month(), t.Day()
}

// shiftMonth moves a whole month, CLAMPING the cursor: the 31st of a 30-day month
// does not exist, and silently landing on "the 31st of September" (which time.Date
// would roll into October) would commit a date the operator never saw highlighted.
func (dp *DatePicker) shiftMonth(n int) {
	t := time.Date(dp.year, dp.month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, n, 0)
	last := daysIn(t.Year(), t.Month())
	c := dp.cursor
	if c > last {
		c = last
	}
	dp.year, dp.month, dp.cursor = t.Year(), t.Month(), c
}

// daysIn is the length of a month.
func daysIn(y int, m time.Month) int {
	// Day 0 of the NEXT month is the last day of this one — the standard trick,
	// and it is year-aware (February 2028 has 29).
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// mondayIndex is the column (0=Mon .. 6=Sun) of the 1st of the displayed month.
// Monday-first matches the schedule's own weekday vocabulary (Mon,Tue,…).
func mondayIndex(y int, m time.Month) int {
	wd := int(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Weekday()) // Sunday=0
	return (wd + 6) % 7
}

var datePickerWeekdays = []string{"Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"}

func (dp *DatePicker) HandleKey(k keyMsg) (bool, tea.Cmd) {
	switch k.String() {
	case "esc":
		dp.done, dp.committed = true, false
		return true, nil
	case "enter", " ", "space":
		dp.done, dp.committed = true, true
		return true, nil
	case "left", "h":
		dp.shift(-1)
		return true, nil
	case "right", "l":
		dp.shift(1)
		return true, nil
	case "up", "k":
		dp.shift(-7)
		return true, nil
	case "down", "j":
		dp.shift(7)
		return true, nil
	case "pgup":
		dp.shiftMonth(-1)
		return true, nil
	case "pgdown":
		dp.shiftMonth(1)
		return true, nil
	case "t":
		now := time.Now()
		dp.year, dp.month, dp.cursor = now.Year(), now.Month(), now.Day()
		return true, nil
	}
	return false, nil
}

// Calendar renders the month grid as text — the widget's whole point, and
// separately testable from the box it is drawn into.
func (dp *DatePicker) Calendar() []string {
	var rows []string

	hdr := fmt.Sprintf("%s %d", dp.month.String(), dp.year)
	pad := (calendarGridW - lipgloss.Width(hdr)) / 2
	if pad < 0 {
		pad = 0
	}
	rows = append(rows, strings.Repeat(" ", pad)+hdr)

	// Weekday header, aligned to the same 4-cell columns as the day cells.
	cells := make([]string, 0, 7)
	for _, d := range datePickerWeekdays {
		cells = append(cells, theme.DetailKey.Render(" "+d+" "))
	}
	rows = append(rows, strings.Join(cells, ""))

	lead := mondayIndex(dp.year, dp.month)
	last := daysIn(dp.year, dp.month)
	// TODAY gets a marker of its own, so "today" is findable without pressing t.
	now := time.Now()
	isThisMonth := now.Year() == dp.year && now.Month() == dp.month

	day := 1 - lead // day-of-month of the first cell (<=0 means leading blanks)
	for week := 0; week < 6 && day <= last; week++ {
		var b strings.Builder
		for col := 0; col < 7; col++ {
			switch {
			case day < 1 || day > last:
				b.WriteString("    ")
			case day == dp.cursor:
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

// View renders the boxed modal.
func (dp *DatePicker) View() string {
	w, h := dp.boxSize()
	innerW, innerH := w-2, h-2
	if innerW < 10 || innerH < 4 {
		return ""
	}

	rows := dp.Calendar()
	// Centre the grid inside the box: the calendar is a fixed 7×4 cell block, and
	// leaving it flush left inside a wider box reads as a bug rather than a layout.
	indent := (innerW - calendarGridW) / 2
	if indent < 0 {
		indent = 0
	}
	pad := strings.Repeat(" ", indent)
	for i := range rows {
		rows[i] = pad + rows[i]
	}
	// The hints take a SMALL indent of their own rather than the grid's: they are
	// full-width text, so centring them like the grid would push the tail past the
	// border and silently truncate the last chord ("esc: ca").
	hpad := "  "
	hint := theme.HintText.Render(hpad + "←→ ↑↓ day/week · PgUp/PgDn month")
	hint2 := theme.HintText.Render(hpad + "t today · enter choose · esc cancel")

	border := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	title := ansi.Truncate(dp.title, max(0, w-5), "…")
	used := 4 + lipgloss.Width(title)
	dash := w - used - 1
	if dash < 0 {
		dash = 0
	}

	body := make([]string, 0, innerH)
	body = append(body, rows...)
	body = append(body, "")
	body = append(body, hint, hint2)

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
