package diffs

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// codeTabStop is the column a tab advances to (the same multiple-of-4 stop the
// markdown code renderer uses). Tabs are EXPANDED before wrapping so the wrapper
// only ever measures printable runes: ansi.StringWidth("\t") is 0, so a tab left
// literal would let a line silently run past its column budget.
const codeTabStop = 4

// wrapByColumns hard-wraps `s` into lines of at most `width` display columns.
//
// IT PRESERVES THE EXACT BYTES of `s`: concatenating the returned lines
// reproduces `s` (no glyph added or removed), which is what the acceptance
// criterion "concatenating a row's wrapped cells reproduces that row's full
// text" pins. Runes are measured with ansi.StringWidth(string(r)) so a wide
// CJK/emoji rune is never split mid-character and never overflows its line.
//
// Leading indentation is preserved for free: the wrap is a pure character flow,
// so a continuation line starts with the next character of the source (spaces
// and tabs that are already in the text appear literally).
//
// A single rune wider than `width` (only possible at width 1 with a wide rune)
// is emitted alone on its own line rather than dropped — a line is never
// destroyed by truncation here.
func wrapByColumns(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	if s == "" {
		return []string{""}
	}
	var lines []string
	var b strings.Builder
	cur := 0
	for _, r := range s {
		rw := ansi.StringWidth(string(r))
		if cur > 0 && cur+rw > width {
			lines = append(lines, b.String())
			b.Reset()
			cur = 0
		}
		b.WriteRune(r)
		cur += rw
	}
	lines = append(lines, b.String())
	return lines
}

// subSpans returns the spans of `spans` clipped to the byte range [start,end)
// and re-based so their offsets are relative to that range. It is what maps an
// emphasis span back onto the wrapped SEGMENT it falls in, so a span that
// straddles a wrap boundary is marked on both the lines it crosses.
func subSpans(spans []EmphasisSpan, start, end int) []EmphasisSpan {
	if len(spans) == 0 || end <= start {
		return nil
	}
	var out []EmphasisSpan
	for _, sp := range spans {
		s, e := sp.Start, sp.End
		if e <= start || s >= end {
			continue
		}
		if s < start {
			s = start
		}
		if e > end {
			e = end
		}
		if e <= s {
			continue
		}
		out = append(out, EmphasisSpan{Start: s - start, End: e - start, Type: sp.Type})
	}
	return out
}

// expandTabsWithSpans expands every tab in `s` to the next tab stop (spaces)
// and remaps `spans` (byte offsets into the RAW text) onto the expanded text, so
// emphasis stays aligned after tab expansion. When `s` has no tab the input is
// returned unchanged (the common case — most diff text is tab-free).
func expandTabsWithSpans(s string, spans []EmphasisSpan) (string, []EmphasisSpan) {
	if !strings.ContainsRune(s, '\t') {
		return s, spans
	}
	var b strings.Builder
	b.Grow(len(s))
	off := make([]int, len(s)+1)
	col := 0
	for i := 0; i < len(s); {
		off[i] = b.Len()
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\t' {
			n := codeTabStop - col%codeTabStop
			b.WriteString(strings.Repeat(" ", n))
			col += n
		} else {
			b.WriteRune(r)
			col += ansi.StringWidth(string(r))
		}
		i += size
		off[i] = b.Len()
	}
	if len(spans) == 0 {
		return b.String(), nil
	}
	out := make([]EmphasisSpan, 0, len(spans))
	for _, sp := range spans {
		st, en := sp.Start, sp.End
		if st < 0 {
			st = 0
		}
		if en > len(s) {
			en = len(s)
		}
		if st >= en {
			continue
		}
		os, oe := off[st], off[en]
		if os >= oe {
			continue
		}
		out = append(out, EmphasisSpan{Start: os, End: oe, Type: sp.Type})
	}
	return b.String(), out
}
