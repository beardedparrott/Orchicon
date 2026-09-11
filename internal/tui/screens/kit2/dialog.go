package kit2

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Dialog is a modal overlay: a centered box with a title, body, and one or
// more buttons. It reuses the shell's overlay discipline (row-by-row,
// ANSI-aware splice) so an overlay never breaks the full-viewport
// View() == exactly h×w contract.
type Dialog struct {
	Title   string
	Body    string
	Buttons []string
	Sel     int
	Danger  bool
}

// Confirm builds the standard destructive-confirm dialog.
func Confirm(title, body, ok string) *Dialog {
	return &Dialog{Title: title, Body: body, Buttons: []string{ok, "cancel"}}
}

// NewDialog builds a dialog with the given buttons.
func NewDialog(title, body string, buttons ...string) *Dialog {
	if len(buttons) == 0 {
		buttons = []string{"ok", "cancel"}
	}
	return &Dialog{Title: title, Body: body, Buttons: buttons}
}

// HandleKey drives the dialog. It returns the chosen button label and
// closed=true once the dialog resolves (enter = the choice, esc = "").
// Navigation keys return closed=false: the caller consumes them and keeps
// the modal up — a dialog owns every key while it is open.
func (d *Dialog) HandleKey(k teaKeyMsg) (string, bool) {
	switch k.String() {
	case "esc":
		return "", true // dismissed without choosing
	case "left", "shift+tab":
		d.Sel = (d.Sel - 1 + len(d.Buttons)) % len(d.Buttons)
		return "", false
	case "right", "tab":
		d.Sel = (d.Sel + 1) % len(d.Buttons)
		return "", false
	case "enter":
		if d.Sel >= 0 && d.Sel < len(d.Buttons) {
			return d.Buttons[d.Sel], true
		}
		return "", true
	}
	return "", false
}

// Box renders the dialog as an exactly w×h block (min size enforced) with a
// rounded border.
func (d *Dialog) Box(w, h int) string {
	if w < 8 {
		w = 8
	}
	if h < 4 {
		h = 4
	}
	border := lipgloss.NewStyle().Foreground(theme.AccentIndigo)
	if d.Danger {
		border = lipgloss.NewStyle().Foreground(theme.Err)
	}
	innerW, innerH := w-2, h-2

	title := ansi.Truncate(d.Title, max(0, w-5), "…")
	used := 4 + lipgloss.Width(title)
	dash := w - used - 1
	if dash < 0 {
		dash = 0
	}
	top := border.Render("┌─ ") + theme.MenuTitle.Render(title) + border.Render(" "+strings.Repeat("─", dash)+"┐")

	bodyLines := []string{}
	if d.Body != "" {
		bodyLines = strings.Split(d.Body, "\n")
	}
	buttonLine := d.buttonLine(innerW)

	rows := []string{top}
	// interior: body lines, then a gap, then the buttons pinned to the
	// bottom row when there is room.
	for i := 0; i < innerH; i++ {
		var content string
		switch {
		case i < len(bodyLines):
			content = bodyLines[i]
		case i == innerH-1 && buttonLine != "":
			content = buttonLine
		default:
			content = ""
		}
		rows = append(rows, border.Render("│")+theme.ScreenBg.Render(Pad(" "+content, innerW))+border.Render("│"))
	}
	rows = append(rows, border.Render("└"+strings.Repeat("─", w-2)+"┘"))
	out := strings.Join(rows, "\n")
	if len(rows) != h {
		out = FitLines(out, w, h)
	}
	return out
}

func (d *Dialog) buttonLine(w int) string {
	if len(d.Buttons) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d.Buttons))
	for i, b := range d.Buttons {
		if i == d.Sel {
			parts = append(parts, theme.ListItemSelected.Render(" "+b+" "))
		} else {
			parts = append(parts, theme.HintText.Render("["+b+"]"))
		}
	}
	return strings.Join(parts, "  ")
}

// teaKeyMsg is aliased so dialog.go needs no bubbletea import elsewhere.
type teaKeyMsg = keyMsg

// OverlayRow splices overlay into row starting at column left (both
// ANSI-aware). Mirrors the shell's overlayRow so an overlay composes over
// the opaque base without disturbing cells it does not cover.
func OverlayRow(row, overlay string, left, width int) string {
	cols := lipgloss.Width(row)
	var prefix, suffix string
	if left > 0 {
		if left >= cols {
			prefix = row + strings.Repeat(" ", left-cols)
		} else {
			prefix = ansi.Truncate(row, left, "")
		}
	}
	total := lipgloss.Width(prefix) + width
	if rest := cols - total; rest > 0 {
		keep := lipgloss.Width(prefix) + width
		suffix = ansi.Truncate(ansi.Truncate(row, keep+rest, ""), cols, "")
		if keep > 0 {
			suffix = ansi.TruncateLeft(suffix, keep, "")
		}
	}
	return prefix + overlay + suffix
}

// Splice composes box over base starting at absolute (top, left).
func Splice(base, box string, top, left int) string {
	rows := strings.Split(base, "\n")
	bw := lipgloss.Width(box)
	for i, br := range strings.Split(box, "\n") {
		r := top + i
		if r < 0 {
			continue
		}
		if r >= len(rows) {
			break
		}
		rows[r] = OverlayRow(rows[r], br, left, bw)
	}
	return strings.Join(rows, "\n")
}

// Center splices box centered over a width×height base. The result keeps
// exactly `height` lines of `width` cells when the base does.
func Center(base, box string, width, height int) string {
	rows := strings.Split(base, "\n")
	bh := len(strings.Split(box, "\n"))
	top := (len(rows) - bh) / 2
	if top < 0 {
		top = 0
	}
	left := (width - lipgloss.Width(box)) / 2
	if left < 0 {
		left = 0
	}
	return Splice(base, box, top, left)
}
