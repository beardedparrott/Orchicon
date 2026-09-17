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
	// renderMu guards the chip state for the duration of a Render. Renders are microseconds and
	// already cheap; correctness under the "two transcripts on two goroutines" contract the parser
	// comment describes is worth more than parallel rendering of a message.
	renderMu sync.Mutex

	chipFg, chipBg string // set by SetCodeChip (the theme)
	chipSurface    Surface
	chipOn         bool
)

// SetCodeChip sets the inline-code chip's colours. The theme calls it on every switch, so the chip
// follows the palette like any other token.
//
// An empty background (the zero value) leaves the chip OFF, and a code span then renders as reverse
// video exactly as before — the safe default for a caller that has not declared a surface.
func SetCodeChip(fg, bg string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipFg, chipBg = fg, bg
}

// sgr builds an SGR sequence for a #rrggbb colour with the given selector (38 foreground, 48
// background). It returns "" for anything it cannot parse, so a malformed token degrades to no
// colour rather than emitting a broken sequence.
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

// chipClose RESTORES the surface rather than resetting. `\x1b[39m`/`\x1b[49m` would drop the rest of
// the line to the terminal's defaults — the band hole measured above — so the close re-asserts the
// colours the chip was drawn inside. A surface with no background cannot be restored, which is why
// chipActive requires one.
func chipClose() string { return sgr(38, chipSurface.Fg) + sgr(48, chipSurface.Bg) }

// chipActive reports whether a chip can be drawn: a surface background is known (so the close can
// restore it) and the theme has supplied colours THAT PARSE.
//
// Both colours must be usable, not merely non-empty: a chip that drew its background but not its
// foreground would put the theme's text on a fill chosen without checking the pair, which is exactly
// the illegibility this exists to remove. Anything unusable falls back to reverse video, which is
// always self-consistent.
func chipActive() bool {
	return chipOn && chipSurface.Bg != "" && sgr(38, chipFg) != "" && sgr(48, chipBg) != ""
}

// SurfaceOf reads the surface from the lipgloss style the output will be drawn INSIDE, so a caller
// passes the style it is already using rather than restating its colours — which is how the two
// would drift. A style with no background yields an empty Surface, and the chip stays off.
func SurfaceOf(st lipgloss.Style) Surface {
	return Surface{Fg: colorHex(st.GetForeground()), Bg: colorHex(st.GetBackground())}
}

// SurfaceTokens builds a Surface from two theme tokens directly. It exists because the theme's
// package-level mirrors are lipgloss.TerminalColor (an interface), not the string a caller can hand
// Surface, so `Surface{string(theme.Bg)}` does not compile.
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

// parser is the GFM parser: the same dialect the GUI renders (remark-gfm), so tables, strikethrough,
// autolinks and task lists agree between the clients. goldmark documents Parser.Parse as safe for
// concurrent use, which matters: the Ask transcript and an execution transcript can be rendered from
// different goroutines.
var parser = goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()

// attr is the set of character attributes a span carries. It is a BIT SET rather than a style,
// because attributes COMPOSE (bold inside a quote inside a list) and because each span emits its own
// complete open/close pair — see the package comment on nesting.
type attr uint8

const (
	aBold attr = 1 << iota
	aItalic
	aStrike
	aFaint
	aUnderline
	aCode // an inline "chip": themed colours when a surface is declared, reverse video otherwise
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
// matching sequence that turns them off again. Both are TARGETED resets: `off` never contains a full
// reset, so a span cannot clobber the host's colours or background.
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
	if s.a == 0 || s.text == "" {
		return s.text
	}
	// A CODE SPAN GETS THE CHIP when one is configured, and reverse video otherwise. The chip is
	// stripped out of the attribute set first: it is drawn with colours, not with SGR 7, so leaving
	// the bit in would emit both.
	if s.a&aCode != 0 && chipActive() {
		rest := s.a &^ aCode
		return chipOpen() + rest.open() + s.text + rest.off() + chipClose()
	}
	return s.a.open() + s.text + s.a.off()
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
//
// Every returned line satisfies lipgloss.Width(line) <= width: callers lay these out inside a pane,
// so an over-wide line is not a cosmetic problem — the detail viewport TRUNCATES, which silently
// hides content (see the package comment on why the transcript needed this at all).
func Render(src string, width int) []string {
	return render(src, width)
}

// render is the shared body: `Render` and `RenderOn` differ only in whether the chip state is set,
// which the span renderer reads. Keeping one body means a fix to layout cannot land in one path and
// miss the other.
func render(src string, width int) []string {
	if width < 1 {
		width = 1
	}
	if strings.TrimSpace(src) == "" {
		return nil
	}
	srcBytes := []byte(src)
	doc := parser.Parse(text.NewReader(srcBytes))
	r := &renderer{width: width, src: srcBytes}
	r.blocks(doc, indent{})
	return clampWidth(r.out, width)
}

// RenderOn renders with a declared SURFACE: the colours the output is drawn inside. It enables the
// inline-code chip, whose close RESTORES those colours rather than resetting to the terminal defaults
// (see the chip section for the band hole a reset leaves).
//
// A caller that does not know its surface keeps Render, which draws code spans as reverse video — the
// behaviour before the chip — so nothing degrades by omission.
func RenderOn(src string, width int, sf Surface) []string {
	renderMu.Lock()
	defer renderMu.Unlock()
	chipSurface, chipOn = sf, true
	defer func() { chipOn, chipSurface = false, Surface{} }()
	return render(src, width)
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
//
// Callers use it to keep NON-markdown bodies on the plain path, which matters because rendering is
// not free: emphasis and link markers are CONSUMED, so the characters disappear. A log line reading
// "*** error ***" or a JSON dump containing "[a](b)" would come back subtly altered. The GUI draws
// the same line (prose is markdown, tool output is a <pre>), so this is parity too.
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
//
// first is drawn on the block's FIRST line and rest on every continuation, so a list item can hang
// its wrapped text under its text (rather than under its bullet).
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

type renderer struct {
	width int
	src   []byte
	out   []string
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
		// Headings are BOLD plus the blank-line separation above. A terminal cannot do the GUI's
		// type scale (h1 20px → h3 16px), and the GUI gives headings no colour either — weight and
		// spacing are the signals both clients have, and a heading rendered flat would lose the
		// structure the author wrote.
		spans := inline(n, r.src, aBold)
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
		// Raw HTML is written through as text. react-markdown without rehype-raw would DROP it, but
		// silently dropping stored content is the one outcome worse than showing markup, and an
		// HTML block in a transcript is still information the operator may need to see.
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
//
// This is the construct the operator reported. A quote in this product is not decoration: it is how a
// proposal marks the passage it is quoting, and how work items mark the operator's VERBATIM words.
// Leaving it as a literal ">" both looks like noise and destroys the attribution it encodes.
func (r *renderer) blockquote(n ast.Node, in indent) {
	q := in.quoted()
	first := true
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if !first {
			// A blank row INSIDE the quote keeps the BAR, so the block reads continuous rather than
			// looking like two separate quotes. This is the quote equivalent of the blank line
			// blocks() puts between top-level blocks — without it two paragraphs in one quote run
			// together and the author's paragraph break is lost.
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
		itemIn := in.with(in.first+marker, in.rest+hang)
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

// codeBlock renders a fenced/indented block inside a light border, which is the terminal analogue of
// the GUI's padded, rounded `\<pre\>`.
//
// LONG LINES ARE WRAPPED, NOT TRUNCATED. The operator's instruction — "we can scroll horizontally
// some and also use word wrap when needed as long as it only takes up the width of the pane it is
// currently in" — is satisfied in the only way a terminal allows here: there is no horizontal scroll
// inside a viewport whose width it cannot exceed, and TRUNCATING code silently destroys it, which is
// the failure this package exists to undo. A line that already fits is emitted VERBATIM (leading
// whitespace is meaning in code); only an over-wide line is wrapped, at word boundaries with a hard
// split for a token that cannot fit at all.
func (r *renderer) codeBlock(n ast.Node, in indent, lang []byte) {
	avail := r.width - in.prefixWidth() - 2 // the "│ " frame
	if avail < 4 {
		avail = 4
	}
	head := "┌─"
	if len(lang) > 0 {
		head += " " + string(lang)
	}
	r.raw(in.first + "\x1b[2m" + truncate(head, r.width-in.prefixWidth()) + "\x1b[22m")

	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		line := strings.TrimRight(string(seg.Value(r.src)), "\n")
		for _, part := range wrapCode(line, avail) {
			r.raw(in.rest + "\x1b[2m│\x1b[22m " + part)
		}
	}
	r.raw(in.rest + "\x1b[2m└─\x1b[22m")
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
			// A SOFT break is a source newline INSIDE a paragraph. CommonMark (and therefore
			// react-markdown, which the GUI uses) treats it as a space, so the clients agree.
			if c.SoftLineBreak() {
				appendText(" ", base)
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
			// A link whose TEXT is not already the URL gets the destination appended, so the target
			// is usable from a terminal (nothing here can click). A link that IS the URL is left
			// alone. The GUI shows only the text because a browser supplies the href; a cell grid has
			// no href, so an invisible target would be information lost.
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

// wrapSpans greedily wraps spans to width, collapsing runs of spaces and honouring explicit newlines.
//
// Wrapping happens on the PLAIN text, before styling, so no line can be measured wrongly by counting
// escape bytes — the mistake the old transcript path made (it measured with len([]rune(s)), which
// counts SGR bytes as columns and would shred styled output).
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

// wrapCode wraps one code line at width, breaking at WORD boundaries where possible and hard at the
// boundary when a single token cannot fit (the operator asked for word wrap; a URL or a minified blob
// still has to fit).
//
// Leading whitespace is significant in code, so a line that already fits is returned untouched by the
// caller — this is only reached for a line that overflows.
func wrapCode(line string, width int) []string {
	if width < 1 {
		width = 1
	}
	if lipgloss.Width(line) <= width {
		return []string{line}
	}
	// Break at the last space that fits; fall back to a hard split when there is none.
	var out []string
	rest := line
	for lipgloss.Width(rest) > width && rest != "" {
		cut, sp := -1, -1
		w := 0
		for i, r := range rest {
			rw := lipgloss.Width(string(r))
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
