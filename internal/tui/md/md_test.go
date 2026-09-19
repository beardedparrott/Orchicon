package md

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// strip removes the attribute codes this package emits, so a test can assert on what the operator
// READS rather than on the escapes — while the other tests assert the escapes are there.
func strip(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case inEsc:
			// inside the sequence
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func plain(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strip(l)
	}
	return out
}

// --- the width promise --------------------------------------------------------------------------

// TestNoLineExceedsWidth is THE contract the operator set: "A table or codeblock can stretch
// horizontally but only to the end of a pane's current width, otherwise we need to use a word wrap
// and render it properly." There is no horizontal scroll to fall back on, and the detail viewport
// TRUNCATES silently — so an over-wide line is invisible data loss, not a cosmetic wobble.
//
// The corpus is deliberately hostile: unbreakable tokens, wide runes, tables with many columns, deep
// nesting, empty structures, and the real constructs found in the tenant.
func TestNoLineExceedsWidth(t *testing.T) {
	corpus := []string{
		"# Heading that is long enough to need wrapping across several lines of a narrow pane indeed",
		"## Second\n\nA paragraph with **bold** and *italic* and `code` and a [link](https://example.com/a/very/long/path/that/keeps/going) in it.",
		"> a quoted line that is quite long and will need to wrap more than once inside a narrow pane",
		"> - a quoted bullet with a long body that wraps and needs a hanging indent to line up under it",
		"***",
		"- one\n- two\n- three\n  - nested three a\n  - nested three b",
		"1. first\n2. second\n10. tenth",
		"- [ ] todo\n- [x] done",
		"```go\nfunc main() { fmt.Println(\"a very long line of code that must wrap inside the pane and not overflow it at all\") }\n```",
		"    indented code block line that is long enough to need wrapping in a narrow pane for sure yes",
		"| a | b | c | d | e | f | g | h |\n|---|---|---|---|---|---|---|---|\n| 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 |",
		"| header one long | header two long |\n|---|---|\n| a value that is long | another long value here |",
		"**aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa**",
		"日本語のテキストがここにあります。これは幅の計算が正しく行われるかを確認するためのものです。",
		"",
		"   ",
		"### \n\n\n",
		"| |\n|-|",
		"> > nested quote deeply\n> > more",
		"text with an unboleatable token: " + strings.Repeat("x", 300),
		"![alt](https://example.com/very/long/image/path.png)",
		"[a]: https://example.com\n[a]",
		"1. item\n\n   with a second paragraph inside it that is long enough to wrap in a narrow pane",
	}
	for _, width := range []int{1, 2, 7, 12, 20, 40, 80} {
		for _, src := range corpus {
			for _, ln := range Render(src, width) {
				if w := lipgloss.Width(ln); w > width {
					t.Errorf("width %d: line %d cells wide (%q) from source %q", width, w, strip(ln), src)
				}
			}
		}
	}
}

// TestEmptySourceRendersNothing: a message with no body must not contribute a stray blank row.
func TestEmptySourceRendersNothing(t *testing.T) {
	for _, src := range []string{"", "   ", "\n", "\n\n\n", "\t"} {
		if got := Render(src, 40); got != nil {
			t.Errorf("Render(%q) = %v, want nil", src, got)
		}
	}
}

// --- the reported defect ------------------------------------------------------------------------

// TestBlockquoteRendersAsABlockNotAMarker is the operator's report, pinned exactly:
//
//	"The thing that concerned me was those greater than signs. What are those representing? It seemed
//	 kind of ugly."
//
// The markers were ATTRIBUTION — they mark quoted words, including the operator's own verbatim
// wording in a work item. So the ">" must be GONE and the quoted text must be visibly a block:
// indented behind a bar, faint and italic.
func TestBlockquoteRendersAsABlockNotAMarker(t *testing.T) {
	src := "Lead-in prose.\n\n> \"the box at the bottom that you type for chats\"\n> needs to be larger.\n\nAfter."
	lines := Render(src, 60)
	joined := strip(strings.Join(lines, "\n"))

	if strings.Contains(joined, "> ") {
		t.Fatalf("the blockquote marker is still rendered as a literal: %q", joined)
	}
	if !strings.Contains(joined, "│") {
		t.Fatalf("a blockquote must be drawn as a bordered block, got %q", joined)
	}
	// The bar must be on the quote's lines only — not on the surrounding prose.
	var barred, unbarred int
	for _, l := range plain(lines) {
		if strings.Contains(l, "│") {
			barred++
		} else if strings.TrimSpace(l) != "" {
			unbarred++
		}
	}
	if barred == 0 || unbarred == 0 {
		t.Fatalf("the border must mark the quote and not the prose (barred=%d unbarred=%d): %v", barred, unbarred, plain(lines))
	}
	// The quoted words are PRESERVED verbatim — the whole point of the construct.
	if !strings.Contains(joined, "\"the box at the bottom that you type for chats\"") {
		t.Fatalf("the quoted words were altered: %q", joined)
	}
	// And the quote is faint+italic (the GUI's `text-muted-foreground italic`).
	if !strings.Contains(strings.Join(lines, "\n"), "\x1b[") {
		t.Fatal("the quote carries no styling at all")
	}
}

// TestQuoteInsideAListKeepsBothMarkers: the screenshot's doubled look was "> -", i.e. a quote
// containing a bullet, NOT nested quotes. Both markers must survive as structure.
func TestQuoteInsideAListKeepsBothMarkers(t *testing.T) {
	lines := plain(Render("> - a quoted bullet\n> - another", 60))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "•") {
		t.Fatalf("the bullet was lost inside the quote: %q", joined)
	}
	if !strings.Contains(joined, "│") {
		t.Fatalf("the quote bar was lost: %q", joined)
	}
	if strings.Contains(joined, ">") {
		t.Fatalf("a literal '>' survived: %q", joined)
	}
}

// --- construct coverage -------------------------------------------------------------------------

func TestHeadingsAreBold(t *testing.T) {
	lines := Render("## A heading", 40)
	if len(lines) == 0 || !strings.Contains(lines[0], "\x1b[1m") {
		t.Fatalf("a heading must be bold, got %q", lines)
	}
	if !strings.Contains(strip(lines[0]), "A heading") {
		t.Fatalf("heading text lost: %q", lines)
	}
}

func TestInlineConstructsAreStyled(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{"bold", "a **b** c", "\x1b[1m"},
		{"italic", "a *b* c", "\x1b[3m"},
		{"strikethrough", "a ~~b~~ c", "\x1b[9m"},
		{"inline code", "a `b` c", "\x1b[7m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(Render(tc.src, 40), "\n")
			if !strings.Contains(got, tc.code) {
				t.Fatalf("%s: expected %q in %q", tc.name, tc.code, got)
			}
			// Markers are consumed: the source punctuation must not be visible.
			vis := strip(got)
			if strings.Contains(vis, "**") || strings.Contains(vis, "`") || strings.Contains(vis, "~~") || strings.Contains(vis, "*b*") {
				t.Fatalf("%s: markers leaked into the visible text: %q", tc.name, vis)
			}
		})
	}
}

// TestNoFullResetIsEverEmitted is the reason this package can be embedded inside the transcript's
// full-width background band. A full reset (\x1b[0m) would clear the band's background from that cell
// to the end of the line, leaving a hole in the fill. Only TARGETED off-codes are allowed.
func TestNoFullResetIsEverEmitted(t *testing.T) {
	src := "# H\n\ntext with **bold**, *italic*, `code`, [a](https://x.example), ~~gone~~\n\n> quote\n\n- list\n\n```\ncode\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	got := strings.Join(Render(src, 40), "\n")
	if strings.Contains(got, "\x1b[0m") {
		t.Fatalf("a full SGR reset was emitted — it would punch a hole in the host band: %q", got)
	}
	// No colour codes either: colour is the host's business (the theme already sets it).
	for _, bad := range []string{"\x1b[38;", "\x1b[48;", "\x1b[39m", "\x1b[49m"} {
		if strings.Contains(got, bad) {
			t.Fatalf("a colour code %q was emitted; colour belongs to the host surface", bad)
		}
	}
}

func TestTablesRenderAsAGrid(t *testing.T) {
	src := "| name | value |\n|---|---|\n| a | 1 |\n| bb | 22 |"
	lines := plain(Render(src, 40))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "name") || !strings.Contains(joined, "value") {
		t.Fatalf("header lost: %q", joined)
	}
	if !strings.Contains(joined, "│") {
		t.Fatalf("no column separators: %q", joined)
	}
	if !strings.Contains(joined, "─") {
		t.Fatalf("no header rule: %q", joined)
	}
	// Alignment: every row's separator must sit in the same CELL column. Measured as a visible column
	// (lipgloss.Width of the prefix), not a byte offset — the rows carry attribute codes.
	var seps []int
	for _, l := range lines {
		i := strings.Index(l, "│")
		if i < 0 {
			continue
		}
		seps = append(seps, lipgloss.Width(l[:i]))
	}
	if len(seps) < 3 {
		t.Fatalf("expected separators on every row, got %d: %v", len(seps), lines)
	}
	for _, s := range seps {
		if s != seps[0] {
			t.Fatalf("columns are not aligned: separator columns %v", seps)
		}
	}
	// Every value survives (wrapping, never truncation).
	for _, want := range []string{"a", "1", "bb", "22"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cell %q lost from %q", want, joined)
		}
	}
}

// TestNarrowTableFallsBackWithoutLosingValues: a grid has a minimum viable width. Below it, the table
// must degrade to a vertical record — losing the grid is acceptable, losing a VALUE is not.
func TestNarrowTableFallsBackWithoutLosingValues(t *testing.T) {
	src := "| alpha | bravo | charlie | delta |\n|---|---|---|---|\n| one | two | three | four |"
	lines := plain(Render(src, 12))
	joined := strings.Join(lines, "\n")
	// The values are checked with whitespace REMOVED, because at this width a cell legitimately wraps
	// mid-word ("thr" + "ee" on the next line). Asserting on the contiguous string would fail on a
	// correct render and push the fix toward truncation, which is the opposite of the requirement.
	squashed := strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(joined)
	for _, want := range []string{"one", "two", "three", "four"} {
		if !strings.Contains(squashed, want) {
			t.Fatalf("value %q lost at width 12: %q", want, joined)
		}
	}
	// And the labels survive too, so the record is still readable as key/value pairs.
	for _, want := range []string{"alpha", "bravo", "charlie", "delta"} {
		if !strings.Contains(squashed, want) {
			t.Fatalf("column label %q lost at width 12: %q", want, joined)
		}
	}
}

// THE BLOCK KEEPS ITS LANGUAGE AND ITS INDENTATION, on the plain path and on the surface path alike — the
// rename from "IsFramed" is the point: the frame is gone (see TestCodeBlockDrawsAFillAndNoFrameGlyphs).
func TestCodeBlockKeepsLanguageAndIndentation(t *testing.T) {
	src := "```go\nx := 1\n  indented()\n```"
	lines := plain(Render(src, 40))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "go") {
		t.Fatalf("the fence language is not shown: %q", joined)
	}
	if !strings.Contains(joined, "x := 1") {
		t.Fatalf("code content lost: %q", joined)
	}
	// Indentation inside code is MEANING and must survive.
	if !strings.Contains(joined, "  indented()") {
		t.Fatalf("leading whitespace in code was eaten: %q", joined)
	}
}

func TestLongCodeLineWrapsAndKeepsAllContent(t *testing.T) {
	long := strings.Repeat("token ", 20)
	lines := plain(Render("```\n"+long+"\n```", 30))
	joined := strings.Join(lines, "\n")
	if n := strings.Count(joined, "token"); n != 20 {
		t.Fatalf("wrapped code lost content: %d of 20 tokens survived in %q", n, joined)
	}
}

func TestListsKeepTheirMarkersAndHangTheirWraps(t *testing.T) {
	lines := plain(Render("- a bullet whose body is long enough to wrap onto a continuation line", 34))
	if len(lines) < 2 {
		t.Fatalf("expected the item to wrap, got %v", lines)
	}
	if !strings.Contains(lines[0], "•") {
		t.Fatalf("bullet marker missing: %q", lines[0])
	}
	// The continuation must be indented to hang under the TEXT, not under the bullet.
	if strings.HasPrefix(lines[1], "•") {
		t.Fatalf("continuation repeated the bullet: %q", lines[1])
	}
	if !strings.HasPrefix(lines[1], "  ") {
		t.Fatalf("continuation is not hanging-indented: %q", lines[1])
	}
}

func TestOrderedListNumbersIncrement(t *testing.T) {
	lines := plain(Render("1. one\n2. two\n3. three", 40))
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"1. one", "2. two", "3. three"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing from %q", want, joined)
		}
	}
}

func TestTaskListCheckboxes(t *testing.T) {
	joined := strings.Join(plain(Render("- [ ] open\n- [x] done", 40)), "\n")
	if !strings.Contains(joined, "☐") || !strings.Contains(joined, "☑") {
		t.Fatalf("task checkboxes not rendered: %q", joined)
	}
}

func TestLinksShowTheirTarget(t *testing.T) {
	joined := strings.Join(plain(Render("see [the docs](https://example.com/docs)", 60)), "\n")
	if !strings.Contains(joined, "the docs") {
		t.Fatalf("link text lost: %q", joined)
	}
	if !strings.Contains(joined, "https://example.com/docs") {
		t.Fatalf("link target not shown — nothing in a terminal can click: %q", joined)
	}
}

func TestSoftBreakBecomesASpaceLikeTheGUI(t *testing.T) {
	// react-markdown without remark-breaks turns a single newline into a space; the clients must agree.
	joined := strings.Join(plain(Render("one\ntwo", 40)), "\n")
	if joined != "one two" {
		t.Fatalf("a soft break must join with a space, got %q", joined)
	}
}

// --- the opt-in used by the callers -------------------------------------------------------------

func TestLooksLikeMarkdownSeparatesProseFromMachineDumps(t *testing.T) {
	markdown := []string{
		"# Title", "> quoted", "- bullet", "1. ordered", "```go", "has **bold**", "has `code`",
	}
	for _, s := range markdown {
		if !LooksLikeMarkdown(s) {
			t.Errorf("LooksLikeMarkdown(%q) = false, want true", s)
		}
	}
	// A machine dump must stay on the plain path: rendering it would CONSUME characters (emphasis and
	// link markers vanish), and these lines are frequently exact evidence.
	machine := []string{
		`{"level":"info","msg":"started"}`,
		"2026-09-16T18:25:53Z INFO listening on :8080",
		"a plain sentence with no markup at all",
		"git status --porcelain",
		strings.Repeat("x", 50),
	}
	for _, s := range machine {
		if LooksLikeMarkdown(s) {
			t.Errorf("LooksLikeMarkdown(%q) = true, want false (it is not markdown)", s)
		}
	}
}

// --- nesting and composition --------------------------------------------------------------------

func TestNestedQuoteStacksBars(t *testing.T) {
	lines := plain(Render("> outer\n>\n> > inner", 60))
	joined := strings.Join(lines, "\n")
	if strings.Count(joined, "│") < 1 {
		t.Fatalf("no quote bars at all: %q", joined)
	}
	// The inner quote must be indented past the outer one.
	var outer, inner string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "inner"):
			inner = l
		case strings.Contains(l, "outer"):
			outer = l
		}
	}
	if len(inner) <= len(outer) {
		t.Fatalf("nested quote is not indented deeper than its parent: outer=%q inner=%q", outer, inner)
	}
}

func TestRenderStringMatchesRender(t *testing.T) {
	src := "# H\n\nbody"
	if RenderString(src, 40) != strings.Join(Render(src, 40), "\n") {
		t.Fatal("RenderString must be Render joined with newlines")
	}
}

// TestNoOverflowAtEveryWidthInRange sweeps widths so a boundary case cannot hide between the widths a
// hand-picked test happens to use.
func TestNoOverflowAtEveryWidthInRange(t *testing.T) {
	src := "## H\n\n> quote text here\n\n| a | bb |\n|---|---|\n| 1 | 2 |\n\n- item\n\n```\ncode line\n```"
	for width := 1; width <= 120; width++ {
		for _, ln := range Render(src, width) {
			if w := lipgloss.Width(ln); w > width {
				t.Fatalf("width %d: line is %d cells: %q", width, w, strip(ln))
			}
		}
	}
}

// --- the code block's containment -------------------------------------------------------------------

// A CODE BLOCK IS A FILLED BLOCK WITH NO FRAME — the operator: "Code blocks aren't really contained
// codeblocks that allow me to easily copy items, it has weird pipes and characters and it screws up the
// copy."
//
// The frame glyphs are asserted ABSENT, which is the assertion that would have caught the original: `│`
// and `┌` are ordinary printable cells, so a selection over the block copies them into whatever the
// operator pastes into. And the FILL is asserted PRESENT as a real SGR background, so "no glyphs" cannot
// be satisfied by rendering nothing at all.
func TestCodeBlockDrawsAFillAndNoFrameGlyphs(t *testing.T) {
	SetCodeBlock("#c0c0c0", "#202020")
	defer SetCodeBlock("", "")
	lines := RenderOn("```go\nx := 1\n```", 40, SurfaceTokens(lipgloss.Color("#c0c0c0"), lipgloss.Color("#101010")))
	joined := strings.Join(lines, "\n")
	for _, glyph := range []string{"│", "┌", "└", "┃", "║"} {
		if strings.Contains(joined, glyph) {
			t.Errorf("the code block draws the frame glyph %q, which lands in a copy of it: %q", glyph, joined)
		}
	}
	if !strings.Contains(joined, "\x1b[48;2;") {
		t.Errorf("the block has no background fill, so it is not contained at all: %q", joined)
	}
	if !strings.Contains(joined, "x := 1") {
		t.Fatalf("code content lost: %q", joined)
	}
}

// AND WITHOUT A DECLARED SURFACE THE FILL IS NOT DRAWN, because it could not be closed — the fail-closed
// rule in blockActive. md.Render is the path several callers use, and a fill it cannot close would leave
// the rest of each row wearing the block's background.
func TestCodeBlockNeedsASurfaceToFill(t *testing.T) {
	SetCodeBlock("#c0c0c0", "#202020")
	defer SetCodeBlock("", "")
	joined := strings.Join(Render("```\nx := 1\n```", 40), "\n")
	if strings.Contains(joined, "\x1b[48;2;") {
		t.Errorf("a fill was drawn on the plain Render path, where there is no surface to close back to: %q", joined)
	}
	if !strings.Contains(joined, "x := 1") {
		t.Fatalf("code content lost: %q", joined)
	}
}
