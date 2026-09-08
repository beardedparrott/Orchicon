package diffs

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// MinSideBySideWidth is the pane width (cells) at or above which the renderer
// uses the two-column side-by-side layout. Below it (a narrow window or the
// pane is crowded out) it collapses to a single unified column — mirroring
// the GUI's DiffView `unified` collapse below MOBILE_BREAKPOINT.
//
// This must be <= the shell's DiffPaneWidth (48) so the pane actually shows
// side-by-side at its default width — otherwise the primary layout the
// acceptance criteria pin (side-by-side with paired line numbers) is
// unreachable and the pane always collapses to unified.
const MinSideBySideWidth = 48

// sideSep separates the old and new columns.
const sideSep = " │ "

// lineNumWidth is the padded width of each line-number gutter.
const lineNumWidth = 4

// RenderPane renders a set of parsed rows into a side-by-side (two-column)
// frame, or a unified (single-column) frame when width < MinSideBySideWidth.
// `width` is the pane's content width; `profile` is the termenv color
// profile (truecolor / 256 / 16 / ascii) at which lipgloss renders.
func RenderPane(rows []Row, width int, profile termenv.Profile) string {
	lipgloss.SetColorProfile(profile)
	if width < MinSideBySideWidth {
		return renderUnified(rows, width)
	}
	return renderSideBySide(rows, width)
}

// renderSideBySide lays out two columns (old | new), each with a line-number
// gutter and line-level red/green. Paired line numbers (blank when a side has
// no line) align via a fixed-width gutter. Hunk rows are never rendered.
// Both columns and the header are derived from the actual pane width so the
// output never exceeds the pane (no horizontal overflow / tearing) and the
// two columns of every row line up (each cell is exactly colW cells).
func renderSideBySide(rows []Row, width int) string {
	if len(rows) == 0 {
		return theme.DiffCtx.Render("   (no changes)")
	}
	// Two text columns plus one separator make up the content width.
	colW := (width - ansi.StringWidth(sideSep)) / 2
	if colW < 1 {
		colW = 1
	}
	var b strings.Builder
	// Header: each side is exactly colW cells so the separator stays in a
	// fixed column aligned with the row separators below.
	b.WriteString(theme.DiffHeader.Render("─ old " + strings.Repeat("─", max(0, colW-6))))
	b.WriteString(theme.DiffGutter.Render(sideSep))
	b.WriteString(theme.DiffHeader.Render(" new " + strings.Repeat("─", max(0, colW-6))))
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString(renderCell(r, true, colW))
		b.WriteString(theme.DiffGutter.Render(sideSep))
		b.WriteString(renderCell(r, false, colW))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderUnified collapses to a single column (sign + old-lineno + new-lineno
// + text) — the GUI's narrow-screen fallback.
func renderUnified(rows []Row, width int) string {
	if len(rows) == 0 {
		return theme.DiffCtx.Render("   (no changes)")
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(renderUnifiedLine(r, width))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderUnifiedLine(r Row, width int) string {
	oldNo := ""
	if r.HasOld {
		oldNo = itoa(r.LineNoOld)
	}
	newNo := ""
	if r.HasNew {
		newNo = itoa(r.LineNoNew)
	}
	left := pad(oldNo, lineNumWidth-1) + pad(newNo, lineNumWidth-1) + " " + r.Sign
	text := r.OldText
	if r.Kind == KindAdd {
		text = r.NewText
	}
	text = truncate(text, max(0, width-len(left)-1))
	var styled string
	switch r.Kind {
	case KindAdd:
		styled = theme.DiffAdd.Render(left + " " + emphasis(text, r.NewSpans))
	case KindDel:
		styled = theme.DiffDel.Render(left + " " + emphasis(text, r.OldSpans))
	default:
		styled = theme.DiffCtx.Render(left + " " + text)
	}
	return styled
}

// renderCell renders one half (old or new) of a side-by-side row. When the
// row has no line on that side, it emits a blank gutter + cell so the two
// columns stay aligned. The returned string is exactly `width` printable
// columns (the cell body is truncated/padded to the column), so the two cells
// of a row always line up and the separator stays in a fixed column — no
// tearing or truncation artifacts from a wide diff line.
func renderCell(r Row, old bool, width int) string {
	gutter := ""
	var text string
	var spans []EmphasisSpan
	if old {
		if r.HasOld {
			gutter = pad(itoa(r.LineNoOld), lineNumWidth)
			text = r.OldText
			spans = r.OldSpans
		} else {
			gutter = strings.Repeat(" ", lineNumWidth)
		}
	} else {
		if r.HasNew {
			gutter = pad(itoa(r.LineNoNew), lineNumWidth)
			text = r.NewText
			spans = r.NewSpans
		} else {
			gutter = strings.Repeat(" ", lineNumWidth)
		}
	}
	// The cell body starts after the gutter + a space. Truncate the text to
	// what fits the column (gutter + " " + text <= width).
	text = truncate(text, max(0, width-lineNumWidth-1))

	if r.Kind == KindAdd && old {
		// An add has no old text: show an empty old column, still with the
		// add line-number gutter on the new side.
		return padToWidth(theme.DiffGutter.Render(pad("", lineNumWidth-1))+" "+theme.DiffCtx.Render(""), width)
	}
	if r.Kind == KindDel && !old {
		// A delete has no new text: show an empty new column.
		return padToWidth(theme.DiffGutter.Render(pad("", lineNumWidth-1))+" "+theme.DiffCtx.Render(""), width)
	}

	var line string
	switch r.Kind {
	case KindAdd:
		line = theme.DiffAdd.Render(gutter + " " + emphasis(text, spans))
	case KindDel:
		line = theme.DiffDel.Render(gutter + " " + emphasis(text, spans))
	default:
		line = theme.DiffCtx.Render(gutter + " " + text)
	}
	return padToWidth(line, width)
}

// padToWidth right-pads a rendered cell to exactly `w` printable columns so
// the two side-by-side columns align (the separator stays in a fixed column).
// ansi.StringWidth ignores SGR escape codes, so emphasis/reverse styling does
// not skew the alignment.
func padToWidth(cell string, w int) string {
	if cur := ansi.StringWidth(cell); cur < w {
		return cell + strings.Repeat(" ", w-cur)
	}
	return cell
}

// emphasis wraps the span ranges of `text` in the emphasis attribute.
func emphasis(text string, spans []EmphasisSpan) string {
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		if sp.Start < prev {
			continue
		}
		if sp.Start > len(text) {
			break
		}
		if sp.End > len(text) {
			sp.End = len(text)
		}
		if sp.Start > prev {
			b.WriteString(text[prev:sp.Start])
		}
		b.WriteString(theme.DiffEmphasis.Render(text[sp.Start:sp.End]))
		prev = sp.End
	}
	if prev < len(text) {
		b.WriteString(text[prev:])
	}
	return b.String()
}

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) <= w {
		return s
	}
	if w <= 1 {
		return s[:1]
	}
	return s[:w-1] + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// currentProfile returns the active lipgloss/termenv color profile (what the
// terminal actually supports). RenderPane uses it to degrade colors.
func currentProfile() termenv.Profile {
	return lipgloss.ColorProfile()
}
