package kit2

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// card.go is the shared VERTICAL card: a bordered box the transcript draws for a consent card or a
// clarifying question.

// CardLine is one selectable row.
type CardLine struct {
	Text   string
	Detail string // appended to a disabled row: why it cannot be chosen
	// Selected is the highlighted row. The caller owns the cursor, so the card
	// can be rebuilt from state on every frame rather than holding its own.
	Selected bool
	// Disabled rows are drawn dim, are skipped by the arrows, and cannot be
	// confirmed.
	Disabled bool
}

// CardSpec is everything a card draws.
type CardSpec struct {
	Title  string
	Body   string
	Notice string
	Lines  []CardLine
	Footer string
	// Input, when non-empty (or when the caller wants an empty input row),
	// adds the free-text "> " row beneath the options.
	Input     string
	ShowInput bool
}

// CardWidth clamps the card to the pane. avail is the pane's width, want the
// card's preferred width (0 = use the pane).
func CardWidth(avail, want int) int {
	if avail < 8 {
		avail = 8
	}
	if want <= 0 || want > avail {
		want = avail
	}
	if want < 8 {
		want = 8
	}
	return want
}

// CardRowSpan is where one CardSpec.Lines entry landed in the rendered card: the 0-based index of
// its FIRST line and how many lines it occupies (a long option wraps.
type CardRowSpan struct {
	Line  int
	Lines int
}

// CardLines renders the card as lines of EXACTLY width cells (ANSI-aware), with
// a rounded border, the shared panel tint and the shared selection highlight.
func CardLines(spec CardSpec, width int) []string {
	lines, _ := CardLinesSpans(spec, width)
	return lines
}

// CardLinesSpans is CardLines plus where each spec.Lines entry landed, so a caller that makes rows
// INTERACTIVE can resolve a click (or a keypress) to the row it actually drew.
func CardLinesSpans(spec CardSpec, width int) ([]string, []CardRowSpan) {
	w := CardWidth(width, 0)
	border := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	spans := make([]CardRowSpan, 0, len(spec.Lines))
	innerW := w - 2
	if innerW < 1 {
		innerW = 1
	}

	title := ansi.Truncate(spec.Title, max(0, w-5), "…")
	used := 4 + lipgloss.Width(title)
	dash := w - used - 1
	if dash < 0 {
		dash = 0
	}
	out := []string{
		border.Render("┌─ ") + theme.MenuTitle.Render(title) + border.Render(" "+strings.Repeat("─", dash)+"┐"),
	}
	emit := func(content string) {
		// OpaquePanel truncates to innerW, pads to innerW and REPAIRS the panel background after any style
		// reset the content carried.
		out = append(out, border.Render("│")+theme.OpaquePanel(content, innerW)+border.Render("│"))
	}

	if spec.Body != "" {
		for _, l := range strings.Split(spec.Body, "\n") {
			for _, wl := range wrapCells(l, innerW, " ", " ") {
				emit(Pad(wl, innerW))
			}
		}
	}
	if spec.Notice != "" {
		// The notice is the DENY-BY-FILE line: it must read as a warning, and it lives inside the card so
		// it cannot be scrolled away from the decision.
		for _, wl := range wrapCells("⚠ "+spec.Notice, innerW, " ", " ") {
			emit(theme.ErrorText.Render(wl))
		}
	}
	for _, l := range spec.Lines {
		start := len(out)
		switch {
		case l.Disabled:
			text := l.Text
			if l.Detail != "" {
				text += " — " + l.Detail
			}
			for _, wl := range wrapCells(text, innerW, "  ", "  ") {
				emit(theme.HintText.Render(wl))
			}
		case l.Selected:
			// The highlight spans the WHOLE row — and EVERY row a wrapped label runs onto — which is what makes
			// the cursor unmistakable in a box that also holds prose.
			for _, wl := range wrapCells(l.Text, innerW, " ▸ ", "   ") {
				emit(theme.ListItemSelected.Render(Pad(wl, innerW)))
			}
		default:
			for _, wl := range wrapCells(l.Text, innerW, "   ", "   ") {
				emit(Pad(wl, innerW))
			}
		}
		spans = append(spans, CardRowSpan{Line: start, Lines: len(out) - start})
	}
	if spec.ShowInput {
		for _, wl := range wrapCells(spec.Input, innerW, "> ", "  ") {
			emit(theme.ListItemSelected.Render(Pad(wl, innerW)))
		}
	}
	if spec.Footer != "" {
		for _, wl := range wrapCells(spec.Footer, innerW, " ", " ") {
			emit(theme.HintText.Render(wl))
		}
	}
	out = append(out, border.Render("└"+strings.Repeat("─", w-2)+"┘"))
	return out, spans
}

// IT EXISTS SO A CARD ROW IS NEVER SILENTLY TRUNCATED.
func wrapCells(text string, width int, first, cont string) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{first}
	}
	var out []string
	prefix, cur := first, ""
	flush := func() {
		out = append(out, prefix+cur)
		prefix, cur = cont, ""
	}
	for _, w := range words {
		room := func() int {
			if r := width - lipgloss.Width(prefix); r > 1 {
				return r
			}
			return 1
		}
		for lipgloss.Width(w) > room() {
			if cur != "" {
				flush()
			}
			head := ansi.Truncate(w, room(), "")
			out = append(out, prefix+head)
			w = dropCells(w, lipgloss.Width(head))
			prefix = cont
		}
		switch {
		case cur == "":
			cur = w
		case lipgloss.Width(prefix)+lipgloss.Width(cur)+1+lipgloss.Width(w) <= width:
			cur += " " + w
		default:
			flush()
			cur = w
		}
	}
	if cur != "" || len(out) == 0 {
		out = append(out, prefix+cur)
	}
	return out
}

// dropCells removes the first n DISPLAY CELLS from s.
func dropCells(s string, n int) string {
	if n <= 0 {
		return s
	}
	used := 0
	for i, r := range s {
		if used >= n {
			return s[i:]
		}
		used += lipgloss.Width(string(r))
	}
	return ""
}

// CardFooter is the card's own key hint.
func CardFooter(question bool) string {
	if question {
		return "↑/↓ select · enter confirm · esc dismisses"
	}
	return "↑/↓ select · enter confirm · esc denies"
}

// Card is the key handler for a vertical option list.
type Card struct {
	Lines []CardLine
	Sel   int
}

// HandleKey drives the card.
func (c *Card) HandleKey(k keyMsg) (choice int, closed bool) {
	switch k.String() {
	case "esc":
		return -1, true
	case "up", "k", "shift+tab":
		c.move(-1)
		return -1, false
	case "down", "j", "tab":
		c.move(1)
		return -1, false
	case "enter":
		if c.Sel >= 0 && c.Sel < len(c.Lines) && !c.Lines[c.Sel].Disabled {
			return c.Sel, true
		}
		return -1, false
	}
	return -1, false
}

// move highlights the next selectable row in direction d, wrapping.
func (c *Card) move(d int) {
	n := len(c.Lines)
	if n == 0 {
		return
	}
	i := c.Sel
	for step := 0; step < n; step++ {
		i = (i + d + n) % n
		if !c.Lines[i].Disabled {
			c.Sel = i
			return
		}
	}
}
