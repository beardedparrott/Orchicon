// Package kit2 is the screen & interaction re-architecture: bordered
// focus-aware panels, typed forms, entity-bound actions, modal dialogs,
// first-class streams, sortable/tree tables, the focus model, and a screen
// model built on all of them.
//
// It deliberately keeps screenkit's leaf widgets (List/Detail/Frame) — kit2
// composes them inside Panels rather than rewriting them.
package kit2

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Panel is a bordered, titled, focus-aware region. It replaces the ad-hoc
// paneW/PaneGap arithmetic screens used to hand-roll: a Panel renders
// EXACTLY Width×Height cells (border included) so panels tile the content
// region without drift.
type Panel struct {
	Title   string
	Focused bool
	Width   int
	Height  int
	Scroll  int

	body []string
}

// NewPanel builds a sized panel.
func NewPanel(title string, w, h int) *Panel {
	return &Panel{Title: title, Width: w, Height: h}
}

// SetContent replaces the panel body (lines are laid out top-down).
func (p *Panel) SetContent(s string) {
	if s == "" {
		p.body = nil
	} else {
		p.body = strings.Split(s, "\n")
	}
	p.clampScroll()
}

// Content returns the current body as a single string.
func (p *Panel) Content() string { return strings.Join(p.body, "\n") }

// SetSize resizes the panel.
func (p *Panel) SetSize(w, h int) { p.Width, p.Height = w, h; p.clampScroll() }

// Body returns the raw body lines (read-only use).
func (p *Panel) Body() []string { return p.body }

// Focus/Blur toggle the focus ring.
func (p *Panel) Focus() { p.Focused = true }
func (p *Panel) Blur()  { p.Focused = false }

// Wheel scrolls the body by delta rows.
func (p *Panel) Wheel(delta int) { p.Scroll += delta; p.clampScroll() }

// ScrollToTop / ScrollToBottom.
func (p *Panel) ScrollToTop()    { p.Scroll = 0 }
func (p *Panel) ScrollToBottom() { p.Scroll = len(p.body) }

// AtBottom reports whether the panel is scrolled to the last row.
func (p *Panel) AtBottom() bool {
	maxOff := len(p.body) - p.innerH()
	if maxOff < 0 {
		maxOff = 0
	}
	return p.Scroll >= maxOff
}

func (p *Panel) clampScroll() {
	maxOff := len(p.body) - p.innerH()
	if maxOff < 0 {
		maxOff = 0
	}
	if p.Scroll > maxOff {
		p.Scroll = maxOff
	}
	if p.Scroll < 0 {
		p.Scroll = 0
	}
}

func (p *Panel) innerW() int {
	if p.Width-2 < 0 {
		return 0
	}
	return p.Width - 2
}

func (p *Panel) innerH() int {
	if p.Height-2 < 0 {
		return 0
	}
	return p.Height - 2
}

// View renders exactly Width×Height cells.
func (p *Panel) View() string {
	w, h := p.Width, p.Height
	if w < 3 || h < 2 {
		return FitLines(strings.Join(p.body, "\n"), w, h)
	}
	border := lipgloss.NewStyle().Foreground(theme.Border)
	titleStyle := theme.ListTitle
	if p.Focused {
		border = lipgloss.NewStyle().Foreground(theme.AccentCyan)
		titleStyle = lipgloss.NewStyle().Foreground(theme.AccentCyan).Bold(true)
	}

	innerW, innerH := w-2, h-2

	// Top rule with the title embedded (focus ring lives on the border).
	var top strings.Builder
	top.WriteString(border.Render("┌"))
	if p.Title != "" {
		t := ansi.Truncate(p.Title, max(0, w-5), "…")
		used := 4 + lipgloss.Width(t) // "─ " + title + " "
		dash := w - used - 1
		if dash < 0 {
			dash = 0
		}
		top.WriteString(border.Render("─ "))
		top.WriteString(titleStyle.Render(t))
		top.WriteString(border.Render(" " + strings.Repeat("─", dash) + "┐"))
	} else {
		d := w - 2
		if d < 0 {
			d = 0
		}
		top.WriteString(border.Render(strings.Repeat("─", d) + "┐"))
	}

	rows := make([]string, 0, h)
	rows = append(rows, top.String())
	for i := 0; i < innerH; i++ {
		idx := p.Scroll + i
		line := ""
		if idx >= 0 && idx < len(p.body) {
			line = p.body[idx]
		}
		rows = append(rows, border.Render("│")+theme.ScreenBg.Render(Pad(line, innerW))+border.Render("│"))
	}
	d := w - 2
	if d < 0 {
		d = 0
	}
	rows = append(rows, border.Render("└"+strings.Repeat("─", d)+"┘"))
	return strings.Join(rows, "\n")
}

// SplitWidths divides w into n columns separated by n-1 gaps of `gap`
// cells. Remainder cells go to the leftmost columns.
func SplitWidths(w, n, gap int) []int {
	if n <= 0 {
		return nil
	}
	total := w - gap*(n-1)
	if total < n {
		total = n
	}
	base := total / n
	rem := total % n
	out := make([]int, n)
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// SplitHeights divides h into n rows (no gaps).
func SplitHeights(h, n int) []int {
	if n <= 0 {
		return nil
	}
	if h < n {
		h = n
	}
	base := h / n
	rem := h % n
	out := make([]int, n)
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// JoinRow lays out panels side by side (heights matched by the caller).
func JoinRow(panels ...string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, panels...)
}

// JoinCol stacks panels vertically.
func JoinCol(panels ...string) string {
	return lipgloss.JoinVertical(lipgloss.Left, panels...)
}

// Pad truncates s to exactly w visible cells (ANSI-aware) and pads with
// spaces when short.
func Pad(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "")
	if c := lipgloss.Width(s); c < w {
		s += strings.Repeat(" ", w-c)
	}
	return s
}

// FitLines normalizes content into exactly h lines of w cells (ANSI-aware),
// painting the opaque screen background on every padding cell. Same contract
// as screenkit.Frame, exported here so kit2 widgets can size themselves.
func FitLines(content string, w, h int) string {
	if w < 1 || h < 1 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = theme.ScreenBg.Render(Pad(l, w))
	}
	for len(lines) < h {
		lines = append(lines, theme.ScreenBg.Render(strings.Repeat(" ", w)))
	}
	return strings.Join(lines, "\n")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
