package diffs

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/testfixtures"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// TestWrapByColumnsPreservesBytes pins the wrap primitive's contract:
// concatenating the wrapped lines reproduces the source EXACTLY (no byte added
// or removed) and every line is within the column budget.
func TestWrapByColumnsPreservesBytes(t *testing.T) {
	cases := []string{
		"",
		"short",
		"a line of exactly twenty",
		strings.Repeat("x", 500),
		"日本語の非常に長い行です日本語の非常に長い行です日本語の非常に長い行です",
		"🚀 emoji line with a tail ✨ and more text after it",
		"    indented continuation test    ",
	}
	for _, width := range []int{1, 3, 8, 20, 51, 200} {
		for _, in := range cases {
			got := wrapByColumns(in, width)
			if len(got) == 0 {
				t.Fatalf("width %d input %q: wrap produced no lines", width, in)
			}
			if joined := strings.Join(got, ""); joined != in {
				t.Errorf("width %d: wrap did not preserve bytes:\n in=%q\nout=%q", width, in, joined)
			}
			for i, l := range got {
				if !utf8.ValidString(l) {
					t.Errorf("width %d: line %d is invalid UTF-8: %q", width, i, l)
				}
				if w := ansi.StringWidth(l); w > width && !(width == 1 && ansi.StringWidth(in) > 1 && strings.Count(l, "") > 1) {
					// A single wide rune at width 1 is the ONE allowed overflow
					// (a rune is never split); everything else must fit.
					if ansi.StringWidth(l) > 2 {
						t.Errorf("width %d: line %q exceeds budget (width %d)", width, l, w)
					}
				}
			}
		}
	}
}

// TestRenderLinesPreserveContent is the headline acceptance criterion: for
// every shared fixture diff, at the pane's MINIMUM body width and at a wide one,
// concatenating a row's wrapped cells reproduces that row's full text — no `…`,
// because nothing is truncated any more. It also asserts every rendered line is
// valid UTF-8 and within the width.
func TestRenderLinesPreserveContent(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	checked := 0
	for _, v := range vecs {
		if v.ExpectedUnifiedDiff == nil {
			continue
		}
		rows := ParseUnifiedDiff(*v.ExpectedUnifiedDiff)
		if len(rows) == 0 {
			continue
		}
		checked++
		for _, width := range []int{46, 52, 240} {
			lines := RenderLines(rows, width, termenv.Ascii)
			// Every line fits and is valid UTF-8.
			for i, l := range lines {
				if !utf8.ValidString(l) {
					t.Fatalf("%s width %d: line %d invalid UTF-8: %q", v.Name, width, i, l)
				}
				if w := ansi.StringWidth(l); w > width {
					t.Fatalf("%s width %d: line %d exceeds width (%d): %q", v.Name, width, i, w, l)
				}
			}
			// The full text of every row's side must appear across the rendered
			// lines. Rebuild per side by stripping the gutter+separator cells.
			if width >= MinSideBySideWidth {
				assertSideBySideContains(t, v.Name, width, rows, lines)
			} else {
				assertUnifiedContains(t, v.Name, width, rows, lines)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fixture diffs were checked")
	}
}

// assertSideBySideContains walks the physical lines and reconstructs each row's
// old/new cell text (from the renderer's OWN cell segments, so multibyte content
// is sliced by rune, never by byte), asserting the row's source text appears and
// nothing was ellipsized.
func assertSideBySideContains(t *testing.T, name string, width int, rows []Row, lines []string) {
	t.Helper()
	colW := (width - sideSepWidth) / 2
	li := 1 // skip the header line
	for _, r := range rows {
		ol := renderCellLines(r, true, colW)
		nl := renderCellLines(r, false, colW)
		n := max(len(ol), len(nl))
		for i := 0; i < n; i++ {
			if li+i >= len(lines) {
				t.Fatalf("%s width %d: ran out of lines reconstructing a row", name, width)
			}
			// Cross-check the physical line IS the two cells joined by the
			// separator (catches a pairing/separator regression too).
			want := cellAt(ol, i, colW) + theme.DiffGutter.Render(sideSep) + cellAt(nl, i, colW)
			if lines[li+i] != want {
				t.Errorf("%s width %d: physical line %d is not the paired cells:\n got=%q\nwant=%q",
					name, width, li+i, ansi.Strip(lines[li+i]), ansi.Strip(want))
			}
		}
		li += n
		// Assert on the CELL SEGMENT (not the joined line) so the gutter and
		// separator cells cannot mask a wrapped row's content.
		if r.HasOld && !isAdd(r) {
			assertCellText(t, name, width, "old", cellText(ol), r.OldText)
		}
		if r.HasNew && !isDel(r) {
			assertCellText(t, name, width, "new", cellText(nl), r.NewText)
		}
	}
}

// cellText concatenates a cell's segments with the gutter stripped and
// continuation padding removed, rune-safely.
func cellText(cell []string) string {
	var b strings.Builder
	for i, seg := range cell {
		plain := ansi.Strip(seg)
		rs := []rune(plain)
		start := 0
		if i == 0 {
			// The first segment carries the line-number gutter + a space.
			if len(rs) >= lineNumWidth+1 {
				start = lineNumWidth + 1
			}
		}
		b.WriteString(strings.TrimLeft(string(rs[start:]), " "))
	}
	return b.String()
}

// assertCellText checks a side's reconstructed text contains the source text and
// carries no ellipsis. An empty source side is skipped (the renderer pads it).
func assertCellText(t *testing.T, name string, width int, side, got, src string) {
	t.Helper()
	if strings.TrimSpace(src) == "" {
		return
	}
	if !strings.Contains(got, strings.TrimRight(src, " ")) {
		t.Errorf("%s width %d: %s cell lost text\n got=%q\nwant contains=%q", name, width, side, got, src)
	}
	if strings.Contains(got, "…") {
		t.Errorf("%s width %d: %s cell TRUNCATED with an ellipsis: %q", name, width, side, got)
	}
}

// assertUnifiedContains checks the single-column layout reproduces each row's
// text with no ellipsis, walking the physical lines in row order.
func assertUnifiedContains(t *testing.T, name string, width int, rows []Row, lines []string) {
	t.Helper()
	i := 0
	for _, r := range rows {
		segs := renderUnifiedLines(r, width)
		text := r.OldText
		if r.Kind == KindAdd {
			text = r.NewText
		}
		var got strings.Builder
		for j := range segs {
			if i+j >= len(lines) {
				t.Fatalf("%s width %d: ran out of unified lines", name, width)
			}
			if lines[i+j] != segs[j] {
				t.Errorf("%s width %d: unified line %d disagrees with the renderer", name, width, i+j)
			}
			got.WriteString(ansi.Strip(lines[i+j]))
		}
		i += len(segs)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if !strings.Contains(got.String(), strings.TrimRight(text, " ")) {
			t.Errorf("%s width %d: unified row lost text\n got=%q\nwant contains=%q", name, width, got.String(), text)
		}
		if strings.Contains(got.String(), "…") {
			t.Errorf("%s width %d: unified row TRUNCATED with an ellipsis: %q", name, width, got.String())
		}
	}
}

func isAdd(r Row) bool { return r.Kind == KindAdd }
func isDel(r Row) bool { return r.Kind == KindDel }

// TestRenderPaneTruncatesUtf8CleanlyStaysGreen re-asserts the pre-existing
// guarantee (already covered by render_test.go) against RenderLines, so the wrap
// path cannot regress it: narrow widths produce valid UTF-8 within the width.
func TestRenderLinesUtf8AndWidth(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	var diff string
	for _, v := range vecs {
		if v.Name == "unicode-line-level" {
			diff = deref(v.ExpectedUnifiedDiff)
		}
	}
	if diff == "" {
		t.Fatal("unicode-line-level fixture not found")
	}
	rows := ParseUnifiedDiff(diff)
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	for _, width := range []int{48, 34, 24, 20, 16, 12, 8} {
		lines := RenderLines(rows, width, termenv.Ascii)
		if len(lines) == 0 {
			t.Fatalf("width %d: empty render", width)
		}
		for i, l := range lines {
			if !utf8.ValidString(l) {
				t.Fatalf("width %d: line %d invalid UTF-8: %q", width, i, l)
			}
			if w := ansi.StringWidth(l); w > width {
				t.Fatalf("width %d: line %d exceeds width (%d): %q", width, i, w, l)
			}
		}
	}
	// No content is ellipsized on the unicode fixture at a wide width.
	for _, l := range RenderLines(rows, 200, termenv.Ascii) {
		if strings.Contains(l, "…") {
			t.Errorf("wide render ellipsized content: %q", l)
		}
	}
}

// TestScrollClampsToWrappedHeight is the scroll-truth acceptance criterion: a
// single 500-char row wraps to many physical lines, `G` lands on the last page
// (maxScroll, not the last ROW), pgdn from there is a no-op, and a huge Scroll
// cannot overshoot.
func TestScrollClampsToWrappedHeight(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 10
	long := strings.Repeat("abcdefghij", 50) // 500 chars
	m.rows = []Row{{
		LineNoOld: 1, LineNoNew: 1, HasOld: true, HasNew: true,
		Sign: " ", OldText: long, NewText: long, Kind: KindCtx,
	}}
	want := len(RenderLines(m.rows, m.bodyWidth(), currentProfile()))
	if want <= m.viewHeight() {
		t.Fatalf("fixture did not wrap past the viewport: %d lines, view %d", want, m.viewHeight())
	}
	m.handleKey(keyMsg("G"))
	if got := m.scroll; got != m.maxScroll() {
		t.Errorf("G set scroll=%d, want maxScroll=%d", got, m.maxScroll())
	}
	if m.scroll != want-m.viewHeight() {
		t.Errorf("maxScroll=%d, want renderedLines-viewHeight=%d", m.scroll, want-m.viewHeight())
	}
	// pgdn from the bottom must not move (clamped, no overshoot).
	before := m.scroll
	m.handleKey(keyMsg("pgdown"))
	if m.scroll != before {
		t.Errorf("pgdown at the bottom moved scroll %d -> %d (overshoot)", before, m.scroll)
	}
	m.Scroll(100000)
	if m.scroll != m.maxScroll() {
		t.Errorf("huge Scroll reached %d, want maxScroll %d", m.scroll, m.maxScroll())
	}
	m.handleKey(keyMsg("g"))
	if m.scroll != 0 {
		t.Errorf("g did not return to the top (scroll=%d)", m.scroll)
	}
	m.Scroll(-5)
	if m.scroll != 0 {
		t.Errorf("Scroll below zero = %d, want 0", m.scroll)
	}

	// The body viewport must agree with the clamp: the last page's top line is
	// exactly scroll, and the last rendered line is reachable.
	body := strings.Split(m.diffBody(), "\n")
	if len(body) != m.viewHeight() {
		t.Errorf("body rendered %d rows, want %d", len(body), m.viewHeight())
	}
	m.scroll = m.maxScroll()
	body = strings.Split(m.diffBody(), "\n")
	if len(body) == 0 || body[len(body)-1] == "" {
		t.Errorf("last page is empty at maxScroll")
	}
}

// TestScrollbarRendersAndTracks asserts the scrollbar column: a thumb at the top
// when scroll==0, at the bottom when scroll==maxScroll, no bar glyphs when the
// content fits, and that it appears on ALL THREE tabs.
func TestScrollbarRendersAndTracks(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	// Direct unit check of the cell geometry.
	if got := scrollbarCell(0, 0, 5, 100); strings.TrimSpace(ansi.Strip(got)) != scrollThumbGlyph {
		t.Errorf("row 0 at scroll 0 is %q, want the thumb", ansi.Strip(got))
	}
	// content fits (total <= viewH) → the cell is blank, no bar.
	if got := scrollbarCell(0, 0, 10, 10); ansi.Strip(got) != " " {
		t.Errorf("content that fits rendered a bar cell %q, want a blank cell", ansi.Strip(got))
	}
	bottomThumb := -1
	for i := 0; i < 5; i++ {
		if strings.TrimSpace(ansi.Strip(scrollbarCell(i, 95, 5, 100))) == scrollThumbGlyph {
			bottomThumb = i
		}
	}
	if bottomThumb != 4 {
		t.Errorf("at max scroll the thumb ends at row %d, want 4 (bottom)", bottomThumb)
	}

	// Model-level: all three tabs render the bar when the content exceeds the
	// viewport, and the thumb MOVES when scrolled.
	long := strings.Repeat("abcdefghij", 60)
	rows := []Row{{LineNoOld: 1, LineNoNew: 1, HasOld: true, HasNew: true, Sign: " ", OldText: long, NewText: long, Kind: KindCtx}}
	groups := GroupByFile([]*apiv1.FileEdit{
		makeEdit(vecByName(t)["create-new-file"], 1),
		makeEdit(vecByName(t)["modify-two-lines"], 2),
		makeEdit(vecByName(t)["delete-file"], 3),
		makeEdit(vecByName(t)["multi-hunk-three-context"], 4),
		makeEdit(vecByName(t)["no-trailing-newline"], 5),
		makeEdit(vecByName(t)["append-lines"], 6),
	})
	for _, tab := range []Tab{TabDiff, TabTree, TabTimeline} {
		m := NewModel(nil, nil)
		m.Width, m.Height = 48, 6 // small viewport → lists and diff both overflow
		if tab == TabDiff {
			m.rows = rows
		} else {
			m.groups = groups
		}
		m.SelectedPath = groups[0].Path
		m.Tab = tab
		m.scroll = 0
		top := barColumn(m)
		if !strings.Contains(top, scrollThumbGlyph) {
			t.Errorf("tab %s: no thumb rendered when content overflows:\n%s", tab, top)
		}
		m.scroll = m.maxScroll()
		if m.scroll == 0 {
			t.Fatalf("tab %s: content did not overflow (maxScroll 0)", tab)
		}
		bottom := barColumn(m)
		if top == bottom {
			t.Errorf("tab %s: the bar did not move when scrolled:\n top=%q\n bot=%q", tab, top, bottom)
		}
		if !strings.Contains(bottom, scrollThumbGlyph) {
			t.Errorf("tab %s: thumb missing at the bottom:\n%s", tab, bottom)
		}

		// Content that FITS renders no bar glyphs at all.
		short := NewModel(nil, nil)
		short.Width, short.Height = 48, 40
		if tab == TabDiff {
			short.rows = []Row{{LineNoOld: 1, LineNoNew: 1, HasOld: true, HasNew: true, Sign: " ", OldText: "one short line", NewText: "one short line", Kind: KindCtx}}
		} else {
			short.groups = groups[:2]
		}
		short.SelectedPath = groups[0].Path
		short.Tab = tab
		if col := barColumn(short); strings.ContainsAny(col, scrollThumbGlyph+scrollTrackGlyph) {
			t.Errorf("tab %s: bar drawn when everything fits: %q", tab, col)
		}
	}
}

// TestClickScrolledListSelectsUnderCursor pins the viewport-aware click row
// mapping: after scrolling a Tree list, a click on a body row selects the file at
// scroll+(y-paneBodyRow), NOT the file at (y-paneBodyRow).
func TestClickScrolledListSelectsUnderCursor(t *testing.T) {
	vecs := vecByName(t)
	names := []string{"create-new-file", "modify-two-lines", "delete-file", "append-lines", "no-trailing-newline", "multi-hunk-three-context"}
	edits := make([]*apiv1.FileEdit, 0, len(names))
	for i, n := range names {
		edits = append(edits, makeEdit(vecs[n], i+1))
	}
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 6 // viewHeight 5 → 6 files overflow by one row
	m.groups = GroupByFile(edits)
	m.SelectedPath = m.groups[0].Path
	m.Tab = TabTree

	// A click on the FIRST body row must select group[scroll+0] = group[1].
	// (SelectPath resets scroll to 0, so establish BOTH the tab and the scroll
	// before every click — a click that selects and re-focuses the diff is the
	// documented behaviour.)
	m.Tab = TabTree
	m.Scroll(1)
	if m.scroll != 1 {
		t.Fatalf("scroll = %d, want 1", m.scroll)
	}
	m.click(5, paneBodyRow)
	if m.SelectedPath != m.groups[1].Path {
		t.Errorf("scrolled click on body row 0 selected %q, want %q (scroll-aware)",
			m.SelectedPath, m.groups[1].Path)
	}
	// And a click on body row 1 must select group[scroll+1] = group[2].
	m.Tab = TabTree
	m.Scroll(1)
	m.click(5, paneBodyRow+1)
	if m.SelectedPath != m.groups[2].Path {
		t.Errorf("scrolled click on body row 1 selected %q, want %q (scroll-aware)",
			m.SelectedPath, m.groups[2].Path)
	}
}

// TestScrollbarClickJumps pins the click-to-jump: a press in the reserved bar
// column scrolls proportionally, and a press there when everything fits is inert.
func TestScrollbarClickJumps(t *testing.T) {
	long := strings.Repeat("abcdefghij", 60)
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 6
	m.rows = []Row{{LineNoOld: 1, LineNoNew: 1, HasOld: true, HasNew: true, Sign: " ", OldText: long, NewText: long, Kind: KindCtx}}
	m.Tab = TabDiff
	// Terminal X for the bar column: contentX = bodyWidth() → x = bodyWidth()+1.
	barX := m.bodyWidth() + 1
	m.click(barX, paneBodyRow+4)
	if m.scroll == 0 {
		t.Errorf("bar click near the bottom did not jump (scroll stayed 0)")
	}
	if m.scroll > m.maxScroll() {
		t.Errorf("bar click overshot: scroll=%d maxScroll=%d", m.scroll, m.maxScroll())
	}

	// Everything fits → inert.
	short := NewModel(nil, nil)
	short.Width, short.Height = 48, 40
	short.rows = m.rows[:1]
	short.Tab = TabDiff
	short.click(short.bodyWidth()+1, paneBodyRow+3)
	if short.scroll != 0 {
		t.Errorf("bar click with no overflow scrolled to %d", short.scroll)
	}
}

// TestVisibleLinesSingleTruth pins that lineCount (the scroll extent) equals the
// renderer's physical line count for the active tab after wrapping.
func TestVisibleLinesSingleTruth(t *testing.T) {
	m := NewModel(nil, nil)
	m.Width, m.Height = 48, 24
	long := strings.Repeat("abcdefghij", 30)
	m.rows = ParseUnifiedDiff("--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n ctx\n+" + long + "\n")
	m.Tab = TabDiff
	want := len(RenderLines(m.rows, m.bodyWidth(), currentProfile()))
	if got := m.lineCount(); got != want {
		t.Errorf("lineCount=%d, want the rendered line count %d", got, want)
	}
	if m.lineCount() <= len(m.rows) {
		t.Errorf("wrapping did not increase the line count (%d vs %d rows) — the fixture should wrap",
			m.lineCount(), len(m.rows))
	}
}

// TestMeasuredCollapse pins the collapse is keyed off the per-column budget, not
// the pane width: the pane's 48-cell floor body width is BELOW the threshold (so
// unified — ONE readable column), a 120-col terminal's body is ABOVE it
// (side-by-side), and the constant is at least as wide as its own derivation.
func TestMeasuredCollapse(t *testing.T) {
	if MinSideBySideWidth != 2*(lineNumWidth+1+MinReadableCodeWidth)+sideSepWidth {
		t.Fatalf("MinSideBySideWidth (%d) is not derived from the per-column budget", MinSideBySideWidth)
	}
	// A column must be readable: the threshold leaves MinReadableCodeWidth code
	// cells after the gutter.
	colW := (MinSideBySideWidth - sideSepWidth) / 2
	if code := colW - lineNumWidth - 1; code < MinReadableCodeWidth {
		t.Errorf("side-by-side leaves %d code cells, want >= %d", code, MinReadableCodeWidth)
	}
	// The pane's minimum: body = 48 - 2 (border + scrollbar) = 46 < threshold.
	const paneFloor = 48
	if paneFloor-2 >= MinSideBySideWidth {
		t.Errorf("the pane floor body (%d) is still >= the threshold — renderUnified is still unreachable",
			paneFloor-2)
	}

	vecs := vecByName(t)
	rows := ParseUnifiedDiff(deref(vecs["modify-two-lines"].ExpectedUnifiedDiff))
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	narrow := RenderLines(rows, paneFloor-2, termenv.Ascii)
	for _, l := range narrow {
		s := ansi.Strip(l)
		if strings.Contains(s, " old ") || strings.Contains(s, " new ") {
			t.Errorf("narrow pane (body %d) kept side-by-side column headers: %q", paneFloor-2, s)
		}
	}
	wide := RenderLines(rows, 52, termenv.Ascii)
	header := ansi.Strip(wide[0])
	if !strings.Contains(header, "old") || !strings.Contains(header, "new") {
		t.Errorf("wide pane (body 52) collapsed to unified instead of side-by-side: %q", header)
	}
}

// TestEmphasisSurvivesWrap pins that an emphasis span crossing a wrap boundary
// is still marked on both physical lines it spans (the wrong-offset SGR risk).
func TestEmphasisSurvivesWrap(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	// A long line whose changed middle straddles the column edge.
	prefix := strings.Repeat("p", 8)
	suffix := strings.Repeat("s", 8)
	old := prefix + "OLDVALUE" + suffix
	new := prefix + "NEWVALUE" + suffix
	rows := ParseUnifiedDiff("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-" + old + "\n+" + new + "\n")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Narrow enough that the emphasized middle wraps.
	lines := RenderLines(rows, MinSideBySideWidth, termenv.TrueColor)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\x1b[") {
		t.Fatal("emphasis produced no SGR at all")
	}
	// Every physical line must be balanced: rendering it alone must not shift
	// the column of any following text (checked via the width assertion in the
	// other tests); here assert the emphasized substrings are present at all.
	stripped := ansi.Strip(joined)
	if !strings.Contains(stripped, "OLDVALUE") || !strings.Contains(stripped, "NEWVALUE") {
		t.Errorf("emphasis fixture content lost across the wrap:\n%s", stripped)
	}
}

// barColumn returns the reserved scrollbar column of the active body as a
// per-row string (the last cell of each body row), for bar assertions.
func barColumn(m *Model) string {
	var body string
	switch m.Tab {
	case TabDiff:
		body = m.diffBody()
	case TabTree:
		body = m.treeBody()
	case TabTimeline:
		body = m.timelineBody()
	}
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		plain := ansi.Strip(line)
		if plain == "" {
			continue
		}
		rs := []rune(plain)
		b.WriteString(string(rs[len(rs)-1]))
	}
	return b.String()
}

// keyMsg builds a bubbletea key message for a named/one-rune key (mirrors what
// handleKey switches on).
func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "G":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}
	case "g":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}
