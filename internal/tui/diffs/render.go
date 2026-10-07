package diffs

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// MinReadableCodeWidth is the number of CODE cells (after the line-number gutter
// and its separating space) that make a side-by-side column worth drawing. It is
// the MEASURE the collapse is keyed off, not the pane width: a column narrower
// than this cannot show a line of Go or TypeScript, so the pane is better off as
// one wide unified column than as two unreadable slivers.
const MinReadableCodeWidth = 19

// MinSideBySideWidth is the BODY-CONTENT width (cells) at or above which the
// renderer uses the two-column side-by-side layout. Below it the pane collapses
// to a single unified column — mirroring the GUI's DiffView `unified` collapse
// below its mobile breakpoint.
//
// IT IS DERIVED FROM THE PER-COLUMN BUDGET, not the pane width:
//
//	2 * (lineNumWidth + 1 + MinReadableCodeWidth) + sideSepWidth
//	= 2 * (4 + 1 + 19) + 3
//	= 51
//
// It used to be 48 — the SAME number as the shell's DiffRailMinWidth — so the
// collapse condition (width < MinSideBySideWidth) was never true at the pane's
// floor and the unified fallback was DEAD CODE in production.
//
// `width` here is the pane's BODY content width, which is the pane's budget
// minus two reserved columns the shell does not count as text: the panel's
// 1-cell left border and the scrollbar's 1-cell column (see Model.bodyWidth).
// At the shell's 48-cell pane floor the body is 46 (< 51) → ONE readable
// unified column, which is the reachable fallback this threshold exists to
// make. At a 120-column terminal the pane is 54, the body 52 (>= 51) → TWO
// columns of (52-3)/2-4-1 = 19 readable code cells each.
//
// The GUI sibling must match the SEMANTICS (a readable per-column cell budget),
// not this number.
const MinSideBySideWidth = 2*(lineNumWidth+1+MinReadableCodeWidth) + 3

// sideSep separates the old and new columns.
const sideSep = " │ "

// sideSepWidth is the DISPLAY width of sideSep (3), measured rather than
// assumed: `len(sideSep)` would be 5 (the │ is 3 bytes) and would misbudget
// every cell by two.
var sideSepWidth = ansi.StringWidth(sideSep)

// lineNumWidth is the padded width of each line-number gutter.
const lineNumWidth = 4

// RenderLines renders rows to the pane's PHYSICAL lines: a row that wraps to
// several terminal lines contributes several entries. This is the SINGLE SCROLL
// TRUTH — the body viewport slices it and Model.visibleLines derives the scroll
// clamp from it, so the two can never disagree (they used to: Scroll clamped
// against len(rows) while the body sliced RENDERED lines, which diverges the
// moment a row wraps). `width` is the pane's CONTENT width.
func RenderLines(rows []Row, width int, profile termenv.Profile) []string {
	lipgloss.SetColorProfile(profile)
	if width < MinSideBySideWidth {
		return renderUnifiedRows(rows, width)
	}
	return renderSideBySideLines(rows, width)
}

// RenderPane renders rows into the pane's frame as a single string (the joined
// RenderLines). `width` is the pane's content width; `profile` is the termenv
// color profile (truecolor / 256 / 16 / ascii) at which lipgloss renders.
func RenderPane(rows []Row, width int, profile termenv.Profile) string {
	return strings.Join(RenderLines(rows, width, profile), "\n")
}

// renderSideBySide lays out two columns (old | new), each with a line-number
// gutter and line-level red/green. Paired line numbers (blank when a side has
// no line) align via a fixed-width gutter. Hunk rows are never rendered.
// Both columns and the header are derived from the actual pane width so the
// output never exceeds the pane (no horizontal overflow / tearing) and the
// two columns of every row line up (each cell is exactly colW cells).
func renderSideBySide(rows []Row, width int) string {
	return strings.Join(renderSideBySideLines(rows, width), "\n")
}

// renderSideBySideLines is renderSideBySide as PHYSICAL lines. A row whose old or
// new side wraps to several lines emits one physical line per wrapped segment,
// with the two sides PAIRED BY HEIGHT (padded with a blank cell on the short
// side) so the `│` separator stays in a fixed cell column on every physical
// line — the alignment the acceptance criteria pin.
func renderSideBySideLines(rows []Row, width int) []string {
	if len(rows) == 0 {
		return []string{theme.DiffCtx.Render("   (no changes)")}
	}
	// Two text columns plus one separator make up the content width.
	colW := (width - sideSepWidth) / 2
	if colW < 1 {
		colW = 1
	}
	out := make([]string, 0, len(rows))
	// Header: each side is exactly colW cells so the separator stays in a
	// fixed column aligned with the row separators below.
	out = append(out,
		theme.DiffHeader.Render("─ old "+strings.Repeat("─", max(0, colW-6)))+
			theme.DiffGutter.Render(sideSep)+
			theme.DiffHeader.Render(" new "+strings.Repeat("─", max(0, colW-6))))
	for _, r := range rows {
		ol := renderCellLines(r, true, colW)
		nl := renderCellLines(r, false, colW)
		n := max(len(ol), len(nl))
		for i := 0; i < n; i++ {
			out = append(out,
				cellAt(ol, i, colW)+theme.DiffGutter.Render(sideSep)+cellAt(nl, i, colW))
		}
	}
	return out
}

// renderUnified collapses to a single column (sign + old-lineno + new-lineno
// + text) — the GUI's narrow-screen fallback.
func renderUnified(rows []Row, width int) string {
	return strings.Join(renderUnifiedRows(rows, width), "\n")
}

// renderUnifiedRows is renderUnified as PHYSICAL lines (one entry per wrapped
// segment).
func renderUnifiedRows(rows []Row, width int) []string {
	if len(rows) == 0 {
		return []string{theme.DiffCtx.Render("   (no changes)")}
	}
	var out []string
	for _, r := range rows {
		out = append(out, renderUnifiedLines(r, width)...)
	}
	return out
}

// renderUnifiedLines renders one unified row to its physical lines: the leading
// line-number/sign gutter on the first line, indented continuation lines for a
// wrapped row. Each line is exactly `width` cells (never wider).
func renderUnifiedLines(r Row, width int) []string {
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
	spans := r.OldSpans
	if r.Kind == KindAdd {
		text = r.NewText
		spans = r.NewSpans
	}
	text, spans = expandTabsWithSpans(text, spans)
	// The gutter alone can exceed a very narrow width; reserve at least one code
	// cell so the rendered line can never overflow the pane.
	if ansi.StringWidth(left)+1 >= width {
		left = ansi.Truncate(left, max(0, width-1), "")
	}
	bodyW := max(1, width-ansi.StringWidth(left)-1)
	segs := wrapByColumns(text, bodyW)
	// Continuations indent past the gutter so a wrapped row reads as one row.
	cont := strings.Repeat(" ", ansi.StringWidth(left)+1)
	out := make([]string, 0, len(segs))
	start := 0
	for i, seg := range segs {
		pre := cont
		if i == 0 {
			pre = left + " "
		}
		sub := subSpans(spans, start, start+len(seg))
		start += len(seg)
		var styled string
		switch r.Kind {
		case KindAdd:
			styled = theme.DiffAdd.Render(pre + emphasis(seg, sub))
		case KindDel:
			styled = theme.DiffDel.Render(pre + emphasis(seg, sub))
		default:
			styled = theme.DiffCtx.Render(pre + seg)
		}
		// fitToWidth, not padToWidth: the width invariant must hold even when a
		// single wide rune cannot fit its reserved cell, so no line ever exceeds
		// the pane.
		out = append(out, fitToWidth(styled, width))
	}
	return out
}

// renderCellLines renders one half (old or new) of a side-by-side row to its
// physical lines. When the row has no line on that side, it emits a single blank
// gutter + cell so the two columns stay aligned. Each returned line is exactly
// `width` printable columns (the cell body is wrapped/padded to the column), so
// the two cells of a row always line up and the separator stays in a fixed
// column — no tearing or truncation artifacts from a wide diff line.
func renderCellLines(r Row, old bool, width int) []string {
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
	if (r.Kind == KindAdd && old) || (r.Kind == KindDel && !old) {
		// An add has no old text and a delete has no new text: show an empty
		// cell (still padded to the column) so the separator stays aligned.
		return []string{blankCell(width)}
	}
	// Tabs are expanded before wrapping: ansi.StringWidth("\t") is 0, so a
	// literal tab would let a line silently run past its column.
	text, spans = expandTabsWithSpans(text, spans)
	// The cell body starts after the gutter + a space; wrap it to what fits the
	// column (gutter + " " + text <= width).
	bodyW := max(1, width-lineNumWidth-1)
	segs := wrapByColumns(text, bodyW)
	var style lipgloss.Style
	switch r.Kind {
	case KindAdd:
		style = theme.DiffAdd
	case KindDel:
		style = theme.DiffDel
	default:
		style = theme.DiffCtx
	}
	out := make([]string, 0, len(segs))
	start := 0
	for i, seg := range segs {
		pre := gutter + " "
		if i > 0 {
			// Continuation lines blank the line-number gutter so they read as a
			// continuation of the row above, keeping the code column aligned.
			pre = strings.Repeat(" ", lineNumWidth) + " "
		}
		var line string
		if r.Kind == KindCtx {
			line = style.Render(pre + seg)
		} else {
			sub := subSpans(spans, start, start+len(seg))
			line = style.Render(pre + emphasis(seg, sub))
		}
		start += len(seg)
		out = append(out, padToWidth(line, width))
	}
	return out
}

// cellAt returns physical line `i` of a rendered cell, or a blank cell of the
// same width when the cell has fewer lines than its opposite side (the height
// pairing that keeps the separator in a fixed column).
func cellAt(cell []string, i, width int) string {
	if i < len(cell) {
		return padToWidth(cell[i], width)
	}
	return blankCell(width)
}

// blankCell is an empty side-by-side cell, exactly `width` cells.
func blankCell(width int) string {
	return padToWidth(theme.DiffGutter.Render(pad("", lineNumWidth-1))+" "+theme.DiffCtx.Render(""), width)
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

// truncate clips `s` to `w` printable columns, appending an ellipsis when it
// does not fit. It delegates to ansi.Truncate so wide/multi-byte runes (CJK,
// emoji) are never split mid-character — the previous byte-length slicing
// emitted invalid UTF-8 on narrow panes (a truncation artifact the acceptance
// criteria forbid). ansi.Truncate also measures column width (not bytes), so
// a wide CJK/emoji line truncates to the actual cell budget instead of
// overflowing the pane.
//
// It is intentionally NOT used for diff content any more: diff lines WRAP (see
// wrapByColumns) so no byte of the diff is destroyed. It remains for the
// Tree/Timeline file-list rows (an index entry, not a diff line) and the error
// banner, where a one-line ellipsized row is the right shape.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
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
