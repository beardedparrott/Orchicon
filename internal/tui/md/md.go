// Package md renders GitHub-flavoured markdown into width-bounded, STYLED terminal lines.
//
// WHY THIS EXISTS. The GUI renders every prose surface through react-markdown (with remark-gfm and
// remark-emoji), so a message containing a blockquote, a table or a fenced block reads correctly
// there. The TUI printed the same bytes verbatim, so the operator saw the SOURCE: "The thing that
// concerned me was those greater than signs. What are those representing? It seemed kind of ugly."
//
// They were markdown blockquote markers — and they were doing real work: the assistant used them to
// quote a draft item body, and work items use them to quote the operator's VERBATIM words
// ("Operator requirement (verbatim intent)"). Printed raw, attribution becomes punctuation soup, and
// the operator's own words stop being distinguishable from the model's. Measured across the tenant:
// 1,786 executions carry bold markers, 1,315 bullet lists, 524 headings, 177 code fences, 85 TABLES,
// 57 blockquotes; 17 stored work-item descriptions carry blockquotes.
//
// WHY GOLDMARK AND NOT GLAMOUR. Both are MIT. glamour is a fine renderer, but it brings ~9 modules
// (chroma, bluemonday, gorilla/css, douceur, reflow…) and — the part that decided it — it PINS
// `charmbracelet/lipgloss v1.1.1-0.<untagged commit>`, which sorts ABOVE the released v1.1.0 this
// repository is on, so MVS would silently move the whole TUI onto an unreleased lipgloss commit.
// This session had just landed code that derives the tab bar's CLICK COLUMNS from lipgloss's
// MEASURED style padding, so a silent padding change would shift every click target. goldmark's
// go.mod has NO require block at all, ships GFM (tables, strikethrough, autolinks, task lists) in
// module, and we already own the rendering primitives.
//
// WHY ATTRIBUTE-ONLY SGR, AND NOT LIPGLOSS STYLES. Output from here is embedded inside a host style:
// the Ask transcript wraps each line in a full-width BACKGROUND BAND (theme.BubbleModel). A span
// that opened with a colour or a full reset (`\x1b[0m`) would clear that band's background from that
// cell to the end of the line — a band with a hole punched in it wherever a bold word appeared. So
// the renderer emits ATTRIBUTES ONLY — bold, faint, italic, underline, strikethrough, reverse — each
// with its TARGETED off-code (`\x1b[22m`, not `\x1b[0m`), which composes with whatever surface hosts
// it and leaves the colours to the theme. It is also closer to the GUI than a coloured renderer
// would be: markdown.tsx gives h1/h2/h3 weight and size but NO colour, and mutes the blockquote.
//
// WIDTH IS A HARD PROMISE, AND THERE IS NO HORIZONTAL SCROLL. The operator: "A table or codeblock can
// stretch horizontally but only to the end of a pane's current width, otherwise we need to use a word
// wrap and render it properly." Every returned line satisfies lipgloss.Width(line) <= width: prose
// wraps, code wraps, tables shrink their columns and wrap their cells (or fall back to a vertical
// record when even that cannot fit), and a token longer than the line is hard-split. An over-wide
// line is not cosmetic here — the detail viewport TRUNCATES, so content would vanish with no error.
//
// WHAT THE GUI USES MARKDOWN FOR, which this mirrors rather than overshoots: the model's prose, the
// operator's own message, reasoning, and an artifact only when its type is "markdown" or its name
// ends in .md. TOOL output is NOT markdown in the GUI (ToolCard renders a <pre>) and is not here
// either — see LooksLikeMarkdown for the opt-in that keeps machine dumps on the plain path.
package md

import (
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// --- the inline-code chip ------------------------------------------------------------------------
//
// An inline code span used to render as REVERSE VIDEO (SGR 7), described in the attribute table as
// "the theme-agnostic stand-in for an inline chip". That is the problem with it: reverse video is
// relative, so it inverts whatever it lands in and cannot be made legible by choosing colours — the
// operator, on the Work detail pane and the Ask transcript: the highlighted text "makes it kind of
// hard to read as well ... we should enhance legibility of text in general".
//
// So a code span gets REAL colours: a chip foreground and background chosen by the theme.
//
// WHY THE SURFACE IS A PARAMETER AND NOT A GLOBAL. A colour span has to END by returning to
// whatever it was drawn inside — the Ask transcript wraps every line in a full-width background band,
// and closing with the terminal default (`\x1b[49m`) leaves the rest of that line on the WRONG
// background. Measured:
//
//	band:            \x1b[48;2;255;0;0mplain\x1b[0m
//	chip closing 49: \x1b[48;2;255;0;0maaa\x1b[48;5;240mcode\x1b[49mbbb\x1b[0m
//	                  └─ band bg ────┘        └─ chip ─┘   └─ terminal default, NOT the band ─┘
//
// The surface differs per CALLER (a bubble band, a detail pane, a form preview), so it cannot be
// package state — two concurrent renders would fight over it. It is passed per render, and renders
// are serialized by renderMu so the chip state below is consistent for the duration of one.
type Surface struct {
	Fg string // the colour text returns to after the chip (e.g. the band's bubble text)
	Bg string // the background the chip sits on, and returns to
}

var (
	// renderMu guards the chip state for the duration of a Render.
	renderMu sync.Mutex

	chipFg, chipBg string // set by SetCodeChip (the theme)
	chipSurface    Surface
	chipOn         bool

	// accentFg/accentRestore are the STRUCTURAL ACCENT and the colour text returns to after it, both
	// set by SetAccentColor (the theme).
	//
	// The restore exists for the same reason the chip's does: `\x1b[39m` would drop the rest of the
	// line to the terminal's default foreground, so the close re-asserts the colour the accent was
	// drawn inside rather than resetting.
	accentFg, accentRestore string

	// blockFg/blockBg are the CODE BLOCK's colours, set by SetCodeBlock (the theme). The restore after a
	// block line re-uses chipSurface for the same reason the chip does: `\x1b[39m` would drop the rest of the
	// line — and the bubble band behind it — to the terminal's defaults.
	blockFg, blockBg string
	// It is scoped per render (see RenderOnSpans) rather than configured once, because it must appear
	// ONLY on a surface where clicking a block actually copies it — md is a leaf several callers share.
	blockGlyph string

	// hardBreaks, when true for the duration of a render, keeps a SOURCE NEWLINE inside a paragraph as
	// a line break instead of collapsing it to a space.
	hardBreaks bool
)

// SetCodeChip sets the inline-code chip's colours.
func SetCodeChip(fg, bg string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipFg, chipBg = fg, bg
}

// SetAccentColor sets the STRUCTURAL ACCENT — the colour of a heading and a list marker — and the
// colour text returns to after it.
func SetAccentColor(fg, restore string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	accentFg, accentRestore = fg, restore
}

// SetCodeBlock sets the code block's colours: the code's foreground and the raised fill behind it.
func SetCodeBlock(fg, bg string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	blockFg, blockBg = fg, bg
}

// Search-highlight state, set by SetSearchHighlight and armed per render by setSearchQuery.
var (
	hlFg        string
	hlBg        string
	hlRestoreFg string
	hlRestoreBg string
	searchQ     string
)

// blockActive reports whether a code block can be drawn on its fill.
func blockActive() bool {
	if blockFg == "" || blockBg == "" {
		return false
	}
	if sgr(38, chipSurface.Fg) == "" || sgr(48, chipSurface.Bg) == "" {
		return false
	}
	return sgr(38, blockFg) != "" && sgr(48, blockBg) != ""
}

// blockOpen starts a code line's fill; blockClose restores the surface it was drawn inside.
func blockOpen() string  { return sgr(38, blockFg) + sgr(48, blockBg) }
func blockClose() string { return chipClose() }

// blockLabelOpen/blockLabelClose style a code block's LABEL row — the `<lang> · ⧉ copyable` line
// above the fill.
func blockLabelOpen() string  { return "\x1b[1m" + accentOpen() }
func blockLabelClose() string { return accentClose() + "\x1b[22m" }

// accentActive reports whether a structural accent can be drawn.
func accentActive() bool {
	if accentFg == "" || accentRestore == "" {
		return false
	}
	return sgr(38, accentFg) != "" && sgr(38, accentRestore) != ""
}

// accentOpen starts the accent; accentClose returns to the colour it was drawn inside.
func accentOpen() string  { return sgr(38, accentFg) }
func accentClose() string { return sgr(38, accentRestore) }

// SetSearchHighlight sets the in-transcript search MATCH colours: the band drawn behind a hit, and
// the colour the text returns to after it.
func SetSearchHighlight(fg, bg, restoreFg, restoreBg string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	hlFg, hlBg, hlRestoreFg, hlRestoreBg = fg, bg, restoreFg, restoreBg
}

// setSearchQuery arms the highlight for one render: every occurrence of q in rendered text is
// marked.
func setSearchQuery(q string) {
	searchQ = strings.ToLower(strings.TrimSpace(q))
}

// highlightActive reports whether a match can be drawn: a non-empty query and a complete, parseable
// colour set.
func highlightActive() bool {
	if searchQ == "" || hlFg == "" || hlBg == "" {
		return false
	}
	if sgr(38, hlRestoreFg) == "" || sgr(48, hlRestoreBg) == "" {
		return false
	}
	return sgr(38, hlFg) != "" && sgr(48, hlBg) != ""
}

func highlightOpen() string  { return sgr(38, hlFg) + sgr(48, hlBg) }
func highlightClose() string { return sgr(38, hlRestoreFg) + sgr(48, hlRestoreBg) }

// markSearchHits wraps every occurrence of the active query in the match band.
func markSearchHits(text string) string {
	if !highlightActive() || text == "" {
		return text
	}
	lower := strings.ToLower(text)
	var b strings.Builder
	from := 0
	for {
		at := strings.Index(lower[from:], searchQ)
		if at < 0 {
			break
		}
		at += from
		b.WriteString(text[from:at])
		b.WriteString(highlightOpen())
		b.WriteString(text[at : at+len(searchQ)])
		b.WriteString(highlightClose())
		from = at + len(searchQ)
	}
	if from == 0 {
		return text
	}
	b.WriteString(text[from:])
	return b.String()
}

// AccentMarker wraps a run of text — a list marker, which is written into an INDENT rather than
// emitted as a span — in the structural accent.
func AccentMarker(text string) string {
	if !accentActive() {
		return text
	}
	return accentOpen() + text + accentClose()
}

// sgr builds an SGR sequence for a #rrggbb colour with the given selector (38 foreground, 48
// background).
func sgr(sel int, hex string) string {
	if len(hex) != 7 || hex[0] != '#' {
		return ""
	}
	r, err1 := strconv.ParseUint(hex[1:3], 16, 8)
	g, err2 := strconv.ParseUint(hex[3:5], 16, 8)
	b, err3 := strconv.ParseUint(hex[5:7], 16, 8)
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	return "\x1b[" + strconv.Itoa(sel) + ";2;" +
		strconv.FormatUint(r, 10) + ";" + strconv.FormatUint(g, 10) + ";" + strconv.FormatUint(b, 10) + "m"
}

// chipOpen is the sequence that starts a chip: its colours, both set explicitly so the span cannot
// inherit a stale one.
func chipOpen() string { return sgr(38, chipFg) + sgr(48, chipBg) }

// chipClose RESTORES the surface rather than resetting.
func chipClose() string { return sgr(38, chipSurface.Fg) + sgr(48, chipSurface.Bg) }

// chipActive reports whether a chip can be drawn: a surface background is known (so the close can
// restore it) and the theme has supplied colours THAT PARSE.
func chipActive() bool {
	return chipOn && chipSurface.Bg != "" && sgr(38, chipFg) != "" && sgr(48, chipBg) != ""
}

// SurfaceOf reads the surface from the lipgloss style the output will be drawn INSIDE, so a caller
// passes the style it is already using rather than restating its colours.
func SurfaceOf(st lipgloss.Style) Surface {
	return Surface{Fg: colorHex(st.GetForeground()), Bg: colorHex(st.GetBackground())}
}

// SurfaceTokens builds a Surface from two theme tokens directly.
func SurfaceTokens(fg, bg lipgloss.TerminalColor) Surface {
	return Surface{Fg: colorHex(fg), Bg: colorHex(bg)}
}

// colorHex renders a lipgloss colour as a #rrggbb string, or "" for anything else (a style may carry
// an adaptive or no colour at all).
func colorHex(c lipgloss.TerminalColor) string {
	if cc, ok := c.(lipgloss.Color); ok {
		return string(cc)
	}
	return ""
}

// parser is the GFM parser: the same dialect the GUI renders (remark-gfm), so tables,
// strikethrough, autolinks and task lists agree between the clients.
var parser = goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()

// attr is the set of character attributes a span carries.
type attr uint8

const (
	aBold attr = 1 << iota
	aItalic
	aStrike
	aFaint
	aUnderline
	aCode   // an inline "chip": themed colours when a surface is declared, reverse video otherwise
	aAccent // a STRUCTURAL run — a heading, a list marker — in the theme's accent colour
)

// sgrCodes maps each attribute to its SGR code.
var sgrCodes = []struct {
	a       attr
	on, off string
}{
	{aBold, "1", "22"},
	{aFaint, "2", "22"},
	{aItalic, "3", "23"},
	{aUnderline, "4", "24"},
	{aCode, "7", "27"},
	{aStrike, "9", "29"},
}

// open returns the SGR sequence that turns on every attribute in the set, and off returns the
// matching sequence that turns them off again.
func (a attr) open() string {
	var codes []string
	for _, c := range sgrCodes {
		if a&c.a != 0 {
			codes = append(codes, c.on)
		}
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func (a attr) off() string {
	var codes []string
	for _, c := range sgrCodes {
		if a&c.a != 0 {
			codes = append(codes, c.off)
		}
	}
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

// span is a run of text with one attribute set.
type span struct {
	text string
	a    attr
}

func (s span) width() int { return lipgloss.Width(s.text) }

// render applies the span's attributes. Spans with no attributes pass through untouched, so plain
// prose carries no escape codes at all.
func (s span) render() string {
	if s.text == "" {
		return s.text
	}
	// PLAIN PROSE IS THE COMMON CASE AND MUST STILL BE MARKED.
	if s.a == 0 {
		return markSearchHits(s.text)
	}
	// A CODE SPAN GETS THE CHIP when one is configured, and reverse video otherwise.
	if s.a&aCode != 0 && chipActive() {
		rest := s.a &^ aCode
		return chipOpen() + rest.open() + s.text + rest.off() + chipClose()
	}
	// A STRUCTURAL ACCENT SPAN — a heading — is drawn in the theme's accent and then restored to the
	// colour it sits inside, exactly as the chip does.
	if s.a&aAccent != 0 && accentActive() {
		rest := s.a &^ aAccent
		return accentOpen() + rest.open() + s.text + rest.off() + accentClose()
	}
	// SEARCH HITS ARE MARKED LAST, on the fully-styled text, so a hit is marked whether it lands in
	// plain prose, a bold run, a heading or a code chip.
	return markSearchHits(s.a.open() + s.text + s.a.off())
}

func spansText(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.text)
	}
	return b.String()
}

func spansWidth(spans []span) int {
	w := 0
	for _, s := range spans {
		w += s.width()
	}
	return w
}

func spansRender(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		b.WriteString(s.render())
	}
	return b.String()
}

// Render parses src as GFM and returns styled lines, each no wider than width columns.
func Render(src string, width int) []string {
	return render(src, width)
}

// render is the shared body: `Render` and `RenderOn` differ only in whether the chip state is set,
// which the span renderer reads.
func render(src string, width int) []string {
	lines, _ := renderSpans(src, width)
	return lines
}

// renderSpans is render plus the geometry of every code block it emitted. The CLAMP applies to the lines
// only and cannot change their COUNT, so the row indexes the blocks recorded stay valid.
func renderSpans(src string, width int) ([]string, []CodeSpan) {
	if width < 1 {
		width = 1
	}
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	srcBytes := []byte(src)
	doc := parser.Parse(text.NewReader(srcBytes))
	r := &renderer{width: width, src: srcBytes}
	r.blocks(doc, indent{})
	return clampWidth(r.out, width), r.codeSpans
}

// RenderOn renders with a declared SURFACE: the colours the output is drawn inside.
func RenderOn(src string, width int, sf Surface) []string {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipSurface, chipOn = sf, true
	defer func() { chipOn, chipSurface = false, Surface{} }()
	return render(src, width)
}

// RenderOnSearch is RenderOn with the in-transcript SEARCH armed: every occurrence of query in the
// rendered text is drawn in the match band.
func RenderOnSearch(src string, width int, sf Surface, query string) []string {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipSurface, chipOn = sf, true
	prev := searchQ
	setSearchQuery(query)
	defer func() { chipOn, chipSurface = false, Surface{}; searchQ = prev }()
	return render(src, width)
}

// RenderOnSpansSearch is RenderOnSpans with the in-transcript SEARCH armed — see RenderOnSearch for
// why the query is render-scoped.
func RenderOnSpansSearch(src string, width int, sf Surface, query string, copyGlyph ...string) ([]string, []CodeSpan) {
	return renderWithSearch(src, width, sf, false, query, copyGlyph...)
}

// RenderUserOnSpansSearch is RenderUserOnSpans with the search armed.
func RenderUserOnSpansSearch(src string, width int, sf Surface, query string, copyGlyph ...string) ([]string, []CodeSpan) {
	return renderWithSearch(src, width, sf, true, query, copyGlyph...)
}

// renderWithSearch is renderWith plus the search query, armed and restored around the render. One body, so a
// layout fix cannot land on one path and miss the other — the same reason renderWith exists.
func renderWithSearch(src string, width int, sf Surface, userBreaks bool, query string, copyGlyph ...string) ([]string, []CodeSpan) {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipSurface, chipOn = sf, true
	blockGlyph = ""
	hardBreaks = userBreaks
	prevQ := searchQ
	setSearchQuery(query)
	if len(copyGlyph) > 0 {
		blockGlyph = copyGlyph[0]
	}
	defer func() {
		chipOn, chipSurface = false, Surface{}
		blockGlyph = ""
		hardBreaks = false
		searchQ = prevQ
	}()
	return renderSpans(src, width)
}

// RenderOnSpans is RenderOn plus where each code block landed and what its source was, and an
// optional COPY AFFORDANCE.
func RenderOnSpans(src string, width int, sf Surface, copyGlyph ...string) ([]string, []CodeSpan) {
	return renderWith(src, width, sf, false, copyGlyph...)
}

// RenderUserOnSpans is RenderOnSpans for text the OPERATOR TYPED: a newline in it is a LINE BREAK.
func RenderUserOnSpans(src string, width int, sf Surface, copyGlyph ...string) ([]string, []CodeSpan) {
	return renderWith(src, width, sf, true, copyGlyph...)
}

// renderWith is the shared body of the two span entry points: they differ only in whether a source newline
// inside a paragraph is a line break. One body, so a fix to layout cannot land on one path and miss the other.
func renderWith(src string, width int, sf Surface, userBreaks bool, copyGlyph ...string) ([]string, []CodeSpan) {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipSurface, chipOn = sf, true
	blockGlyph = ""
	hardBreaks = userBreaks
	if len(copyGlyph) > 0 {
		blockGlyph = copyGlyph[0]
	}
	defer func() {
		chipOn, chipSurface = false, Surface{}
		blockGlyph = ""
		hardBreaks = false
	}()
	return renderSpans(src, width)
}

// RenderOnString is RenderOn joined with newlines.
func RenderOnString(src string, width int, sf Surface) string {
	return strings.Join(RenderOn(src, width, sf), "\n")
}

// RenderString is Render joined with newlines, for hosts that take a single string body.
func RenderString(src string, width int) string {
	return strings.Join(Render(src, width), "\n")
}

// LooksLikeMarkdown reports whether src contains a construct worth rendering.
func LooksLikeMarkdown(src string) bool {
	for _, ln := range strings.Split(src, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t, ">"), strings.HasPrefix(t, "#"),
			strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "+ "),
			strings.HasPrefix(t, "```"), strings.HasPrefix(t, "~~~"):
			return true
		case strings.Contains(t, "**"), strings.Contains(t, "`"):
			return true
		}
		// An ordered-list start: "1. " or "12) ".
		if i := strings.IndexAny(t, ".)"); i > 0 && i < 4 {
			if isDigits(t[:i]) && len(t) > i+1 && t[i+1] == ' ' {
				return true
			}
		}
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// indent carries the decoration a nested block draws at the start of each of its lines.
type indent struct {
	first string
	rest  string
	// depth counts enclosing blockquote levels, so a nested quote can stack its bars.
	depth int
}

func (in indent) with(first, rest string) indent {
	return indent{first: first, rest: rest, depth: in.depth}
}

func (in indent) quoted() indent {
	// The bar is FAINT so it reads as structure rather than content. A nested quote stacks bars.
	bar := "\x1b[2m│\x1b[22m "
	return indent{
		first: in.first + bar,
		rest:  in.rest + bar,
		depth: in.depth + 1,
	}
}

// prefixWidth is the columns the decoration consumes. The wider of the two is used for the whole
// block, so a wrapped line can never outgrow its first line's budget.
func (in indent) prefixWidth() int {
	a, b := lipgloss.Width(in.first), lipgloss.Width(in.rest)
	if a > b {
		return a
	}
	return b
}

// Copying the rendered form cannot be made clean: the band the block sits in indents every line by
// a cell, and unlike trailing padding a leading space CANNOT be trimmed.
type CodeSpan struct {
	FirstRow, LastRow int    // row indexes into the []string the render returned
	Lang              string // the fence's info string, when it had one
	Source            string // the fence's contents, verbatim
}

// ContainsRow reports whether a rendered row belongs to this block.
func (c CodeSpan) ContainsRow(row int) bool { return row >= c.FirstRow && row <= c.LastRow }

// renderer holds one render's state.
type renderer struct {
	width int
	src   []byte
	out   []string
	// codeSpans are the blocks this render emitted, for a caller that wants the click geometry. Recorded on
	// every render (the cost is one append per block); the callers that do not want them discard them.
	codeSpans []CodeSpan
}

// --- line emission -------------------------------------------------------------------------------

func (r *renderer) blank() {
	if n := len(r.out); n > 0 && r.out[n-1] != "" {
		r.out = append(r.out, "")
	}
}

// emit wraps spans to the width left over after in's decoration and writes them out.
func (r *renderer) emit(in indent, spans []span) {
	avail := r.width - in.prefixWidth()
	if avail < 1 {
		avail = 1
	}
	lines := wrapSpans(spans, avail)
	for i, ln := range lines {
		p := in.rest
		if i == 0 {
			p = in.first
		}
		r.out = append(r.out, p+spansRender(ln))
	}
}

// raw writes an already-composed line, bounded to the renderer's width. It is how the bounded
// structures (rules, table frames, code borders) get out.
func (r *renderer) raw(line string) { r.out = append(r.out, line) }

// --- block rendering -----------------------------------------------------------------------------

// blocks renders a node's block-level children, separating them with a blank line so consecutive
// paragraphs read as distinct (the GUI's paragraph margins).
func (r *renderer) blocks(n ast.Node, in indent) {
	first := true
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if !first {
			// A blank BETWEEN blocks, never before the first: a body that opens with a blank line
			// wastes a row at the top of every pane.
			r.blank()
		}
		first = false
		r.block(c, in)
	}
}

func (r *renderer) block(n ast.Node, in indent) {
	switch n := n.(type) {
	case *ast.Heading:
		// Headings are BOLD plus the blank-line separation above — AND the theme's accent colour.
		spans := inline(n, r.src, aBold|aAccent)
		r.emit(in, spans)

	case *ast.Paragraph, *ast.TextBlock:
		r.emit(in, inline(n, r.src, 0))

	case *ast.Blockquote:
		r.blockquote(n, in)

	case *ast.List:
		r.list(n, in)

	case *ast.FencedCodeBlock:
		r.codeBlock(n, in, n.Language(r.src))

	case *ast.CodeBlock:
		r.codeBlock(n, in, nil)

	case *ast.ThematicBreak:
		w := r.width - in.prefixWidth()
		if w > 40 {
			w = 40
		}
		if w < 3 {
			w = 3
		}
		r.raw(in.first + "\x1b[2m" + strings.Repeat("─", w) + "\x1b[22m")

	case *east.Table:
		r.table(n, in)

	case *ast.HTMLBlock:
		// Raw HTML is written through as text.
		for i := 0; i < n.Lines().Len(); i++ {
			seg := n.Lines().At(i)
			r.emit(in, []span{{text: string(seg.Value(r.src))}})
		}
		if n.HasClosure() {
			seg := n.ClosureLine
			r.emit(in, []span{{text: string(seg.Value(r.src))}})
		}

	default:
		// Anything unhandled that has block children is descended into rather than dropped: losing
		// content silently is worse than rendering it plainly.
		if n.FirstChild() != nil {
			r.blocks(n, in)
			return
		}
		if txt := strings.TrimSpace(string(n.Text(r.src))); txt != "" {
			r.emit(in, []span{{text: txt}})
		}
	}
}

// blockquote renders a quote as an indented, bar-prefixed, faint-italic block — the terminal
// analogue of the GUI's `border-l-2 … italic text-muted-foreground`.
func (r *renderer) blockquote(n ast.Node, in indent) {
	q := in.quoted()
	first := true
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if !first {
			// A blank row INSIDE the quote keeps the BAR, so the block reads continuous rather than looking
			// like two separate quotes.
			r.raw(strings.TrimRight(q.first, " "))
		}
		first = false
		// The quote's own children are blocks; the faint-italic is applied to inline content only, so
		// nested structures (a list inside a quote) keep their own styling for their markers.
		switch c.Kind() {
		case ast.KindParagraph, ast.KindTextBlock, ast.KindHeading:
			r.emit(q, faintItalic(inline(c, r.src, 0)))
		default:
			r.block(c, q)
		}
	}
}

// faintItalic applies the quote's own styling to every span, preserving attributes already set
// (bold inside a quote stays bold AND becomes faint-italic).
func faintItalic(spans []span) []span {
	out := make([]span, len(spans))
	for i, s := range spans {
		s.a |= aFaint | aItalic
		out[i] = s
	}
	return out
}

// list renders bullets or numbers with a HANGING indent, so a wrapped item's continuation lines line
// up under its text rather than under its marker.
func (r *renderer) list(n *ast.List, in indent) {
	num := n.Start
	if num == 0 {
		num = 1
	}
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		marker := "• "
		if n.IsOrdered() {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		if box, ok := taskCheckbox(item); ok {
			if box {
				marker = "☑ "
			} else {
				marker = "☐ "
			}
		}
		hang := strings.Repeat(" ", lipgloss.Width(marker))
		itemIn := in.with(in.first+AccentMarker(marker), in.rest+hang)
		// An item's own blocks are rendered at the item's indent; the sibling blank-line policy does
		// not apply INSIDE an item, so a two-paragraph item stays tight.
		for c := item.FirstChild(); c != nil; c = c.NextSibling() {
			r.block(c, itemIn)
			itemIn = itemIn.with(in.first+hang, in.rest+hang)
		}
	}
}

// taskCheckbox reports whether a list item opens with a GFM task checkbox, and whether it is checked.
func taskCheckbox(item ast.Node) (checked bool, ok bool) {
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		for gc := c.FirstChild(); gc != nil; gc = gc.NextSibling() {
			if cb, is := gc.(*east.TaskCheckBox); is {
				return cb.IsChecked, true
			}
		}
	}
	return false, false
}

// codeBlock renders a fenced/indented block as a FILLED BLOCK — a raised background, no border
// glyphs.
func (r *renderer) codeBlock(n ast.Node, in indent, lang []byte) {
	avail := r.width - in.prefixWidth()
	if avail < 4 {
		avail = 4
	}
	// THE SPAN STARTS AT THE LABEL, so a click on the dim `bash` line copies the block too — it is the block's
	// own header, and a label would otherwise be a dead spot one row above a live one.
	start := len(r.out)
	// THE LABEL ROW CARRIES THE COPY AFFORDANCE, and the row is emitted whenever EITHER a language or
	// an affordance is present.
	label := string(lang)
	if blockGlyph != "" {
		if label != "" {
			label += " · "
		}
		label += blockGlyph + " copyable"
	}
	if label != "" {
		r.raw(in.first + blockLabelOpen() + truncate(label, avail) + blockLabelClose())
	}
	// THE LINES ARE COLLECTED FIRST, because the band's width is derived from them — see below.
	lines := make([]string, 0, n.Lines().Len())
	widest := 0
	for i := 0; i < n.Lines().Len(); i++ {
		// Bound to a local first: At returns a Segment VALUE, and Value has a pointer receiver.
		seg := n.Lines().At(i)
		line := strings.TrimRight(string(seg.Value(r.src)), "\n")
		lines = append(lines, line)
		if w := codeWidth(line); w > widest {
			widest = w
		}
	}
	// The band: as wide as the widest line, never wider than the pane.
	band := widest
	if band > avail {
		band = avail
	}
	if band < 4 {
		band = 4
		if band > avail {
			band = avail
		}
	}
	inner := band
	if inner < 1 {
		inner = 1
	}
	for _, line := range lines {
		for _, part := range wrapCode(line, inner) {
			r.raw(in.rest + codeBlockLine(part, band))
		}
	}
	// THE SOURCE IS THE FENCE'S CONTENT, reassembled from the lines this block read — not scraped back
	// out of r.out, which would carry the fill's padding.
	r.codeSpans = append(r.codeSpans, CodeSpan{
		FirstRow: start,
		LastRow:  len(r.out) - 1,
		Lang:     string(lang),
		Source:   strings.Join(lines, "\n"),
	})
}

// codeBlockLine renders one code line inside the block's fill, padded on the RIGHT to the BAND's
// width so the fill is a rectangle rather than a ragged edge.
func codeBlockLine(part string, avail int) string {
	if !blockActive() {
		return part
	}
	pad := avail - codeWidth(part)
	if pad < 0 {
		pad = 0
	}
	return blockOpen() + part + strings.Repeat(" ", pad) + blockClose()
}

// --- inline extraction ---------------------------------------------------------------------------

// inline flattens a node's inline children into styled spans, inheriting base (so bold inside a
// heading stays bold).
func inline(n ast.Node, src []byte, base attr) []span {
	var out []span
	appendText := func(text string, a attr) {
		if text == "" {
			return
		}
		// Merge with the previous span when the attributes match, so a paragraph stays as few spans
		// as possible (fewer codes in the output, and wrapping has less to split).
		if k := len(out) - 1; k >= 0 && out[k].a == a && !strings.Contains(out[k].text, "\n") {
			out[k].text += text
			return
		}
		out = append(out, span{text: text, a: a})
	}

	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch c := c.(type) {
		case *ast.Text:
			appendText(string(c.Segment.Value(src)), base)
			// A SOFT break is a source newline INSIDE a paragraph.
			if c.SoftLineBreak() {
				if hardBreaks {
					appendText("\n", base)
				} else {
					appendText(" ", base)
				}
			}
			if c.HardLineBreak() {
				appendText("\n", base)
			}
		case *ast.String:
			appendText(string(c.Value), base)
		case *ast.CodeSpan:
			appendText(spansText(inline(c, src, 0)), base|aCode)
		case *ast.Emphasis:
			a := base | aItalic
			if c.Level >= 2 {
				a = base | aBold
			}
			out = append(out, inline(c, src, a)...)
		case *east.Strikethrough:
			out = append(out, inline(c, src, base|aStrike)...)
		case *ast.Link:
			text := spansText(inline(c, src, base|aUnderline))
			dest := string(c.Destination)
			// A link whose TEXT is not already the URL gets the destination appended, so the target is usable
			// from a terminal (nothing here can click).
			if dest != "" && dest != text {
				out = append(out, span{text: text, a: base | aUnderline})
				appendText(" ("+dest+")", base|aFaint)
				continue
			}
			appendText(text, base|aUnderline)
		case *ast.AutoLink:
			appendText(string(c.Label(src)), base|aUnderline)
		case *ast.Image:
			alt := spansText(inline(c, src, 0))
			if alt == "" {
				alt = "image"
			}
			// No image protocol is available in a terminal (kitty/sixel are out of scope), so an
			// image is NAMED rather than dropped — dropping loses the fact that one was there.
			appendText("[image: "+alt+"]", base|aFaint)
		case *ast.RawHTML:
			for i := 0; i < c.Segments.Len(); i++ {
				seg := c.Segments.At(i)
				appendText(string(seg.Value(src)), base)
			}
		default:
			out = append(out, inline(c, src, base)...)
		}
	}
	return out
}

// --- wrapping ------------------------------------------------------------------------------------

// wrapSpans greedily wraps spans to width, collapsing runs of spaces and honouring explicit
// newlines.
func wrapSpans(spans []span, width int) [][]span {
	if width < 1 {
		width = 1
	}
	type atom struct {
		text string
		a    attr
		brk  bool
		sp   bool
	}
	var atoms []atom
	for _, s := range spans {
		rest := s.text
		for rest != "" {
			switch {
			case strings.HasPrefix(rest, "\n"):
				atoms = append(atoms, atom{brk: true, a: s.a})
				rest = rest[1:]
			case rest[0] == ' ' || rest[0] == '\t':
				// Collapse a whitespace run into one pending space.
				i := 0
				for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
					i++
				}
				atoms = append(atoms, atom{sp: true, a: s.a})
				rest = rest[i:]
			default:
				i := 0
				for i < len(rest) && rest[i] != ' ' && rest[i] != '\t' && rest[i] != '\n' {
					i++
				}
				atoms = append(atoms, atom{text: rest[:i], a: s.a})
				rest = rest[i:]
			}
		}
	}

	var lines [][]span
	var cur []span
	curW := 0
	flush := func() {
		lines = append(lines, cur)
		cur = nil
		curW = 0
	}
	// push appends text to the current line, merging with the previous span when attributes match.
	push := func(text string, a attr) {
		if k := len(cur) - 1; k >= 0 && cur[k].a == a {
			cur[k].text += text
		} else {
			cur = append(cur, span{text: text, a: a})
		}
	}
	pendingSpace := false
	for _, at := range atoms {
		switch {
		case at.brk:
			flush()
			pendingSpace = false
		case at.sp:
			pendingSpace = true
		default:
			w := lipgloss.Width(at.text)
			// A word longer than the line: hard-split it rather than let it overflow. Losing the
			// width bound here would defeat the whole package.
			if w > width {
				if curW > 0 {
					flush()
				}
				for _, chunk := range hardSplit(at.text, width) {
					push(chunk, at.a)
					flush()
				}
				pendingSpace = false
				continue
			}
			sp := 0
			if pendingSpace && curW > 0 {
				sp = 1
			}
			if curW+sp+w > width {
				flush()
				sp = 0
			}
			if sp == 1 {
				push(" ", at.a)
				curW++
			}
			push(at.text, at.a)
			curW += w
			pendingSpace = false
		}
	}
	if len(cur) > 0 || len(lines) == 0 {
		flush()
	}
	// Drop a single trailing empty line: prose never ends with a visible blank.
	if n := len(lines); n > 1 && spansWidth(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return lines
}

// hardSplit breaks a token wider than the line into chunks of at most width columns.
func hardSplit(s string, width int) []string {
	var out []string
	var cur strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
			w = 0
		}
		cur.WriteRune(r)
		w += rw
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// codeTabStop is the column a tab advances to: the terminal default, every eight columns.
const codeTabStop = 8

// codeWidth is the VISUAL width of a code line: lipgloss.Width, EXCEPT that a tab counts for the cells it
// actually paints.
//
// lipgloss.Width scores a tab as ZERO — anstyle treats it as a control character — which is right for prose
// and wrong for code. A terminal expands a tab to the next tab stop, so a tab-indented line is among the
// WIDEST things in a block while measuring as the narrowest, and anything sized from lipgloss.Width is short
// by one tab stop per tab. Measured on a Python body:
//
//	"\tif x > 0:"   lipgloss.Width = 9   painted = 17
//
// so the fill ended eight cells before the text did — ragged on exactly the lines most likely to be
// tab-indented, which is the shape the operator's "tab/spaces" remark was about. The band starts at column 0
// in the common case, which is where the tab-stop assumption is exact.
func codeWidth(s string) int {
	w := 0
	for _, r := range s {
		w += codeRuneWidth(r, w)
	}
	return w
}

// codeRuneWidth is one rune's advance from a given column — a tab's depends on where it begins, which is why
// this cannot be a per-rune constant.
func codeRuneWidth(r rune, col int) int {
	if r == '\t' {
		return codeTabStop - col%codeTabStop
	}
	return lipgloss.Width(string(r))
}

// wrapCode wraps one code line at width, breaking at WORD boundaries where possible and hard at the
// boundary when a single token cannot fit (the operator asked for word wrap.
func wrapCode(line string, width int) []string {
	if width < 1 {
		width = 1
	}
	if codeWidth(line) <= width {
		return []string{line}
	}
	// Break at the last space that fits; fall back to a hard split when there is none.
	var out []string
	rest := line
	for codeWidth(rest) > width && rest != "" {
		cut, sp := -1, -1
		w := 0
		for i, r := range rest {
			rw := codeRuneWidth(r, w)
			if w+rw > width {
				break
			}
			w += rw
			if r == ' ' || r == '\t' {
				sp = i
			}
			cut = i + len(string(r))
		}
		if sp > 0 && sp < cut {
			out = append(out, strings.TrimRight(rest[:sp], " \t"))
			rest = strings.TrimLeft(rest[sp:], " \t")
			continue
		}
		if cut <= 0 {
			cut = len(rest)
		}
		out = append(out, rest[:cut])
		rest = rest[cut:]
	}
	if rest != "" {
		out = append(out, rest)
	}
	return out
}

// truncate clips a plain string to width columns.
func truncate(s string, width int) string {
	if width < 1 || lipgloss.Width(s) <= width {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if w+rw > width {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String()
}

// clampWidth is the LAST-RESORT guarantee that no returned line exceeds the pane. Each block
// renderer already bounds itself; this makes Render total regardless, because a single over-wide
// line is silently truncated by the detail viewport — content disappearing with no error, which is
// the exact failure mode this package was written to remove.
func clampWidth(lines []string, width int) []string {
	for i, l := range lines {
		if lipgloss.Width(l) > width {
			lines[i] = truncate(l, width)
		}
	}
	return lines
}
