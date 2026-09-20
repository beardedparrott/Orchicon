// Package screenkit is the shared list+detail scaffolding every orch
// screen composes: a scrollable list pane, a key-value detail pane,
// loading/error/empty states, next_page_token pagination, and mouse
// wiring (wheel scroll + click select). Hand-rolled instead of
// bubbles/list so mouse + focus behavior stay fully ours (plan §9 risk 2
// decision).
package screenkit

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/md"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Item is one list row.
type Item struct {
	ID    string
	Title string
	Meta  string // dim right-hand context, e.g. status or timestamp

	// Tree metadata (optional — only a tree view sets these). The list pane
	// draws the indent and the +/- collapse affordance from them, so a tree
	// row's Title must NOT pre-indent itself.
	Depth       int    // indent level (0 = root)
	Parent      string // parent row's ID ("" = root)
	HasChildren bool   // a parent: draws a toggle and can collapse
}

// Field is one key-value row in the detail pane.
type Field struct {
	Key   string
	Value string
}

// List is the scrollable list pane.
type List struct {
	Title    string
	Items    []Item
	Cursor   int
	Offset   int
	Width    int
	Height   int
	Loading  bool
	Err      string
	EmptyMsg string
	// NextPageToken carries the RPC pagination cursor ("" = no more).
	NextPageToken string
}

// SetItems replaces the list contents and resets cursor/scroll.
func (l *List) SetItems(items []Item, nextPageToken string) {
	l.Items = items
	l.NextPageToken = nextPageToken
	l.Cursor, l.Offset = 0, 0
}

// Selected returns the item under the cursor, or nil.
func (l *List) Selected() *Item {
	if l.Cursor >= 0 && l.Cursor < len(l.Items) {
		return &l.Items[l.Cursor]
	}
	return nil
}

// Move shifts the cursor by delta, keeping the window scrolled.
func (l *List) Move(delta int) {
	if len(l.Items) == 0 {
		return
	}
	l.Cursor += delta
	if l.Cursor < 0 {
		l.Cursor = 0
	}
	if l.Cursor >= len(l.Items) {
		l.Cursor = len(l.Items) - 1
	}
	l.clampOffset()
}

// clampOffset keeps the cursor inside the visible window.
func (l *List) clampOffset() {
	h := l.visibleRows()
	if l.Cursor < l.Offset {
		l.Offset = l.Cursor
	}
	if l.Cursor >= l.Offset+h {
		l.Offset = l.Cursor - h + 1
	}
	if l.Offset < 0 {
		l.Offset = 0
	}
}

func (l *List) visibleRows() int {
	h := l.Height - 2 // title + border overhead
	if h < 1 {
		h = 1
	}
	return h
}

// Wheel scrolls by delta rows.
func (l *List) Wheel(delta int) {
	h := l.visibleRows()
	max := len(l.Items) - h
	if max < 0 {
		max = 0
	}
	l.Offset += delta
	if l.Offset > max {
		l.Offset = max
	}
	if l.Offset < 0 {
		l.Offset = 0
	}
}

// Click selects the row at absolute terminal y (pane-relative y passed
// by the shell after layout math) when it is visible.
func (l *List) Click(row int) bool {
	idx := l.Offset + row
	if row >= 0 && idx >= 0 && idx < len(l.Items) {
		l.Cursor = idx
		return true
	}
	return false
}

// View renders the pane.
func (l *List) View(focused bool) string {
	var b strings.Builder
	title := l.Title
	if focused {
		b.WriteString(theme.ListTitle.Render(title))
	} else {
		b.WriteString(theme.ListMeta.Render(title))
	}
	b.WriteString("\n")

	if l.Err != "" {
		// Human-readable retry state, never a bare "error: unauthenticated"
		// cascade (Phase 2b — the friendly text already names the fix, e.g.
		// "run /connect"). The pane stays interactive: existing items are
		// kept on view and r still refreshes.
		b.WriteString(theme.ErrorText.Render("⚠ "+l.Err) + "\n")
		b.WriteString(theme.HintText.Render("  press r to refresh once the issue is resolved") + "\n")
		return b.String()
	}
	if l.Loading && len(l.Items) == 0 {
		b.WriteString(theme.HintText.Render("loading…") + "\n")
		return b.String()
	}
	if len(l.Items) == 0 {
		msg := l.EmptyMsg
		if msg == "" {
			msg = "nothing here"
		}
		b.WriteString(theme.HintText.Render(msg) + "\n")
		return b.String()
	}
	h := l.visibleRows()
	end := l.Offset + h
	if end > len(l.Items) {
		end = len(l.Items)
	}
	for i := l.Offset; i < end; i++ {
		it := l.Items[i]
		row := listRow(it, l.Width)
		if i == l.Cursor {
			b.WriteString(theme.ListItemSelected.Render(row))
		} else {
			b.WriteString(theme.ListItem.Render(row))
		}
		b.WriteString("\n")
	}
	// scroll position indicator
	b.WriteString(theme.HintText.Render(fmt.Sprintf("%d-%d/%d", l.Offset+1, end, len(l.Items))))
	return b.String()
}

// Detail is the scrollable key-value detail pane.
type Detail struct {
	Title    string
	Fields   []Field
	Body     string // free-form trailing text (description, events, …)
	Width    int
	Height   int
	vp       viewport.Model
	initOnce bool
	vpBody   string // body currently loaded into the viewport
	// vpWidth is the width the loaded body was RENDERED at. Markdown has to be laid out at a concrete
	// width, so a pane resize invalidates the rendered lines even though the SOURCE has not changed —
	// without this the pane would keep the old width's line breaks after a resize.
	vpWidth int
	// vpHeight is the HEIGHT the loaded body was laid out for, and it exists for the same reason vpWidth
	// does — a pane whose height changes must re-load, not just re-viewport.
	//
	// WITHOUT IT THE TAIL OF THE BODY IS CLIPPED FOR A FRAME. The height is recomputed every ensureVP from
	// the field count, and it changes whenever that count does (the Ask pane's header goes from a
	// two-field fallback to the fetched set as a conversation's detail lands, which is a row). The
	// staleness check below tracked the body, the width and the title but NOT the height, so with only the
	// height changed the viewport kept content laid out for the old one and DROPPED ITS LAST ROWS — and the
	// last row is where the Ask transcript puts its notice, so "Orchicon is thinking…" and "⚠ disconnected"
	// flickered out for exactly one frame.
	//
	// Measured before the fix: after a fetch the pane's body went 22 -> 21 rows, the stream followed to 21,
	// the notice was still set on the stream, and the painted frame did not contain it. One frame later it
	// was back, which is why this reads as a flicker rather than as a missing feature.
	vpHeight int
	// vpTitle is the TITLE the loaded body belongs to. It is the detail's
	// IDENTITY: a body that changes under the same title is the same item
	// updating (a live transcript appending), while a new title is a DIFFERENT
	// item — and a different item must start at the top.
	vpTitle string
	dirty   bool // Body changed since the viewport last loaded it
	// bodyLaidOut declares that the CALLER has already laid this body out — wrapped it, styled it,
	// punctuated it with its own newlines — so the pane must return it verbatim rather than treating it as
	// markdown source.
	//
	// WHY THIS MUST BE DECLARED AND CANNOT BE INFERRED. The pane shares one body slot across every
	// list+detail screen, and the two kinds of body are indistinguishable by CONTENT:
	//
	//   - a work item's description is markdown SOURCE (a blockquote quoting the operator's own wording),
	//     which must be rendered — that is what the markdown commit was for;
	//   - an execution's transcript is ALREADY RENDERED (collapsible blocks, a todo list, flow views, a
	//     build log), where every newline is meaningful.
	//
	// The heuristic gate (md.LooksLikeMarkdown) cannot separate them, and it fails LOUDLY when it guesses
	// wrong: markdown JOINS consecutive non-blank lines into one paragraph, so an already-laid-out body comes
	// out as a single scrunched line. Measured on a real execution body, the todo list:
	//
	//   todo (6/6 done) [x] Read run .orchicon files + verify repo/branch state (!) [x] Delete leftover ...
	//
	// — the operator's report exactly ("todo is scrunched and has no newlines"), caused by ONE `**success**`
	// further down in the same body making the WHOLE thing look like markdown. The collapse markers went with
	// it, which is why "tools and chats are no longer being collapsed": the blocks were still built, and then
	// flattened by the renderer.
	bodyLaidOut bool
	// pendingOffset/pendingOffsetSet pin the viewport to a line offset on the NEXT
	// render, overriding the usual "keep the reader's scroll" rule. An editor sets
	// it so the pane follows its cursor; a reader never does.
	pendingOffset    int
	pendingOffsetSet bool

	// footer is a FIXED band below the scrolling body (the execution detail's
	// message box). It is never part of what the viewport scrolls, so an input
	// placed here is always on screen — see SetFooter.
	footer string

	// Hero is the centered empty state (the GUI's "Ask Orchicon
	// anything…" block): rendered until real content arrives.
	Hero      bool
	HeroTitle string
	HeroBody  string
}

// SetContent replaces the detail content. Scroll state is PRESERVED: the
// viewport reloads lazily in View() and keeps the operator's offset,
// following the tail only when they were already at the bottom. The old
// code reset initOnce/vpBody here, so every live-chunk repaint (the
// shell calls SetDetailContent on each chat wake) rebuilt the viewport
// and snapped the transcript back to the top — scrolling a live
// conversation was effectively dead.
func (d *Detail) SetContent(title string, fields []Field, body string) {
	d.setContent(title, fields, body, false)
}

// SetContentLaidOut is SetContent for a body the CALLER has already laid out (a transcript, a log, a flow
// view): the pane returns it verbatim instead of re-rendering it as markdown. See bodyLaidOut.
func (d *Detail) SetContentLaidOut(title string, fields []Field, body string) {
	d.setContent(title, fields, body, true)
}

func (d *Detail) setContent(title string, fields []Field, body string, laidOut bool) {
	d.Title, d.Fields, d.Body = title, fields, body
	d.bodyLaidOut = laidOut
	d.Hero = false
	// The IDENTITY changed too, not just the body: a different item must reload
	// even when its body happens to be byte-identical (two workflows whose flows
	// render the same, say) — otherwise the pane keeps the previous item's scroll
	// offset and its stale vpTitle.
	if body != d.vpBody || title != d.vpTitle {
		d.dirty = true
	}
}

// SetHero installs the centered empty-state block, shown until real
// content replaces it via SetContent.
func (d *Detail) SetHero(title, body string) {
	d.Hero, d.HeroTitle, d.HeroBody = true, title, body
}

// SetFooter installs a FIXED band at the bottom of the pane, outside the
// scrolling region. An empty string removes it.
//
// WHY THIS IS A PANE FEATURE AND NOT ANOTHER BODY SECTION. The execution
// detail's message box was written into the BODY, at the end of the
// transcript — which reads correctly and is unusable: a real transcript is
// taller than the pane, the viewport opens at the TOP, and the input therefore
// sat ~65 lines below the fold with no way to reach it but scrolling. The
// operator: "I STILL don't see a chat prompt inside an execution in the TUI".
// The prompt was there the whole time; it was simply never on screen.
//
// A footer is the shape an input needs: always visible, never scrolled away,
// never part of what the transcript is measured against. The viewport's height
// is reduced by the band so the LAST line of the body still clears it rather
// than hiding behind it.
func (d *Detail) SetFooter(s string) {
	d.footer = s
}

// Footer returns the current fixed band ("" when none).
func (d *Detail) Footer() string { return d.footer }

// footerRows is how many rows the band occupies, including its separator.
func (d *Detail) footerRows() int {
	if d.footer == "" {
		return 0
	}
	return strings.Count(d.footer, "\n") + 2 // one blank separator row + the band
}

// ensureVP (re)initializes the viewport for the current pane size.
//
// THE VIEWPORT MUST FIT WHAT View() ACTUALLY EMITS. The pane is hosted by a Panel that renders
// exactly `Height-2` content rows and CLIPS anything beyond them — and it clips the TAIL. So content
// that overflows loses its LAST lines, which is precisely where a footer lives: the message box was
// installed, correct, and thrown away by the clip. (Before the footer existed the tail was the
// bottom of the scrollable body, where losing it merely looked like the transcript continuing past
// the fold.)
//
// So the budget is computed from the rows View() will write: the title, the fields, the body's
// separator + viewport, and the footer's separator + band. The viewport gets what is LEFT, which is
// what makes the band always visible — the property an input needs.
func (d *Detail) ensureVP() {
	w := d.Width
	if w < 1 {
		w = 1
	}
	h := d.BodyHeightFor(len(d.Fields), d.Body != "")
	if !d.initOnce {
		d.vp = viewport.New(w, h)
		d.initOnce = true
		d.dirty = true
	}
	d.vp.Width, d.vp.Height = w, h
}

// BodyHeightFor is the rows the pane gives its SCROLLING BODY, for the given field count and whether a
// body is present.
//
// Exported because a caller that renders an EXTERNAL widget INTO that body has to size the widget with
// it. The Ask transcript is one: the shell sizes its kit2.Stream from the pane, and sizing it from the
// content region instead makes the widget show more rows than the pane draws — and the rows it loses are
// the NEWEST ones, which is the operator's "Once we hit the bottom pane, I no longer see my messages
// popping up right away." One implementation, shared with ensureVP, so the two cannot drift.
func (d *Detail) BodyHeightFor(fieldRows int, hasBody bool) int {
	// Rows View() spends on everything that is not the scrolling body.
	overhead := 1 + fieldRows // title + fields
	if hasBody {
		overhead += 2 // the blank separator + at least something in the viewport
	}
	overhead += d.footerRows()
	h := d.Height - 2 - overhead
	if h < 1 {
		h = 1
	}
	return h
}

// Wheel scrolls the detail body. Positive delta = scroll down
// (LineDown), negative = scroll up (LineUp). The old code called
// LineDown for both directions, so scrolling up in the chat transcript
// appeared dead.
// SetScrollOffset pins the viewport to a line offset. Used by an editor whose
// pane must FOLLOW a cursor rather than preserve a reader's place.
func (d *Detail) SetScrollOffset(offset int) {
	d.ensureVP()
	d.pendingOffset = offset
	d.pendingOffsetSet = true
}

// Wheel scrolls the detail pane by delta lines.
func (d *Detail) Wheel(delta int) {
	d.ensureVP()
	if delta < 0 {
		d.vp.LineUp(-delta)
		return
	}
	d.vp.LineDown(delta)
}

// Update lets the viewport handle messages.
func (d *Detail) Update(msg tea.Msg) {
	d.ensureVP()
	d.vp.Update(msg)
}

// View renders the pane.
func (d *Detail) View() string {
	if d.Hero {
		return centerBlock(d.HeroTitle, d.HeroBody, d.Width, d.Height)
	}
	d.ensureVP()
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render(d.Title) + "\n")
	for _, f := range d.Fields {
		val := f.Value
		if val == "" {
			val = "—"
		}
		keyWidth := 18
		key := f.Key
		if len(key) > keyWidth {
			key = key[:keyWidth]
		}
		b.WriteString(theme.DetailKey.Render("  "+key) + " " + theme.DetailValue.Render(val) + "\n")
	}
	if d.Body != "" {
		b.WriteString("\n")
		if d.dirty || d.vpBody != d.Body || d.vpWidth != d.vp.Width || d.vpHeight != d.vp.Height {
			// Reload the viewport, and decide whether to KEEP the operator's scroll.
			//
			// Same title = the same item updating (a live transcript appending), so
			// the offset is restored — "reading history must not be yanked away by
			// the next chunk". A DIFFERENT title = a different item, and it must
			// open at the TOP: preserving the offset across a selection switch
			// landed the new item wherever the old one happened to be scrolled,
			// which is why a workflow's FLOW appeared to start at step 3 with an
			// approval on top (its first two steps were above the fold).
			sameItem := d.Title == d.vpTitle
			atBottom := d.vp.AtBottom()
			offset := d.vp.YOffset
			pinned, pinnedSet := d.pendingOffset, d.pendingOffsetSet
			d.pendingOffsetSet = false
			d.vp.SetContent(d.bodyContent())
			d.vpBody = d.Body
			d.vpWidth = d.vp.Width
			d.vpHeight = d.vp.Height
			d.vpTitle = d.Title
			d.dirty = false
			switch {
			case pinnedSet:
				// An editor asked for a specific line (its cursor's step).
				d.vp.SetYOffset(pinned)
			case !sameItem:
				d.vp.GotoTop()
			case atBottom:
				d.vp.GotoBottom()
			default:
				d.vp.SetYOffset(offset)
			}
		}
		b.WriteString(d.vp.View())
	}
	// The FIXED band goes last, below the scrolling body and outside it: whatever
	// the transcript does, the input stays where the operator expects a prompt to
	// be. It is emitted even when the body is empty, because an execution with no
	// transcript still accepts a question — that is exactly when a message box
	// matters most.
	if d.footer != "" {
		b.WriteString("\n" + d.footer)
	}
	return b.String()
}

// bodyContent is the detail body as it should be DISPLAYED: laid out to the pane's width.
//
// Two things are happening here, and both were needed for the work-item surface specifically:
//
//  1. MARKDOWN IS RENDERED. A work item's description and acceptance criteria are authored markdown —
//     the GUI renders them through markdown.tsx — and the TUI showed the source, so a quoted
//     operator requirement appeared as "> ...", which reads as punctuation noise and loses the
//     attribution the marker encodes.
//
//  2. THE BODY IS LAID OUT AT ALL. This pane never wrapped: it handed the raw string to a viewport,
//     which does not reflow, so a long description was CUT at the pane's edge. Rendering lays every
//     line to the width, so nothing can exceed the pane — which is the requirement, because there is
//     no horizontal scroll to reach what an over-wide line hides.
//
// The markdown path is GATED on the body actually containing markdown. That gate is the important
// safety property: this one pane serves every list+detail screen, including ones whose body is a log
// dump, a trace, a JSON blob or a pre-composed key/value block. Rendering those through markdown
// would CONSUME characters (emphasis and link markers vanish), and a log line is evidence. Bodies
// that are not markdown are passed through untouched, exactly as delivered.
func (d *Detail) bodyContent() string {
	// A BODY THE CALLER ALREADY LAID OUT IS RETURNED VERBATIM. Rendering it would JOIN its lines into
	// paragraphs and destroy the layout — see bodyLaidOut for the measured case.
	if d.bodyLaidOut {
		return d.Body
	}
	if d.vp.Width < 1 || !md.LooksLikeMarkdown(d.Body) {
		return d.Body
	}
	if out := md.RenderOnString(d.Body, d.vp.Width, md.SurfaceTokens(theme.Text, theme.Bg)); out != "" {
		return out
	}
	return d.Body
}

// centerBlock renders the centered empty state: title, a blank line, and
// the word-wrapped body, vertically + horizontally centered in w×h.
func centerBlock(title, body string, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	inner := w - 6
	if inner < 10 {
		inner = 10
	}
	lines := []string{theme.ListTitle.Render(title)}
	if body != "" {
		lines = append(lines, "")
		for _, l := range wrapText(body, inner) {
			lines = append(lines, theme.HintText.Render(l))
		}
	}
	top := (h - len(lines)) / 2
	if top < 0 {
		top = 0
	}
	out := make([]string, 0, top+len(lines))
	for i := 0; i < top; i++ {
		out = append(out, "")
	}
	for _, l := range lines {
		pad := (w - lipgloss.Width(l)) / 2
		if pad < 0 {
			pad = 0
		}
		out = append(out, strings.Repeat(" ", pad)+l)
	}
	return strings.Join(out, "\n")
}

// wrapText word-wraps s to width columns (paragraph-aware).
func wrapText(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			if line == "" {
				line = word
				continue
			}
			if len([]rune(line))+1+len([]rune(word)) <= width {
				line += " " + word
				continue
			}
			out = append(out, line)
			line = word
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// StatusBadge maps a status string to a themed colored string.
func StatusBadge(s string) string {
	st := theme.ListMeta
	switch strings.ToLower(s) {
	case "ok", "active", "ready", "running", "open", "published", "approved", "connected", "completed", "succeeded", "healthy":
		st = theme.StatusOK
	case "warning", "degraded", "paused", "pending", "reconnecting", "queued", "waiting":
		st = theme.StatusWarn
	case "error", "failed", "rejected", "disconnected", "cancelled", "canceled", "dead":
		st = theme.StatusErr
	case "in_progress", "in-progress", "busy", "dispatching", "starting":
		st = theme.StatusBusy
	}
	return st.Render(s)
}

// listRow composes ONE list row at a given width: the indent, the title, and the right-aligned Meta.
//
// THE META IS RESERVED FIRST AND THE TITLE IS TRUNCATED INTO WHAT REMAINS. That ordering IS the fix
// for the operator's report, and the previous ordering was the bug:
//
//	"Both Workflow Runs and Executions titles are too long and you can't see the status. I think we
//	 should have workflow name - title - status, but maybe cut off text to ensure they all show up on
//	 the screen visibly."
//
// The old code built `"  " + Title + pad + Meta` and then truncated the WHOLE string from the RIGHT
// (`truncate(row, l.Width)`). A title longer than the pane therefore consumed the padding AND THEN the
// status: the row read "Some Very Long Workflow Name · A Long Work Item Ti…" and the status — the one
// field the operator scans a run list for, and the only field that CHANGES while a run is live — was
// cut off the end. An executions row is `workflow · work item` beside `running · Worker`, and at 80
// columns the two overflow, so the status was the first thing lost.
//
// Reserving Meta first makes the status UNCONDITIONALLY VISIBLE, which is the invariant the operator
// asked for ("ensure they all show up on the screen visibly"). It also means a row's status can no
// longer be truncated away by the very data it describes — which is why the Execution list looked
// "frozen" while actually repainting correctly: the row WAS refreshing, but the changing field was the
// one being cut off.
//
// Degenerate widths are handled in order of what matters: the status outlives the title (it is the
// scan field), the indent goes first (pure chrome), and a title too wide for a very narrow pane is
// truncated from its END — never by eating Meta.
func listRow(it Item, width int) string {
	const indent = "  "
	if width <= 0 {
		// Unsized: no budget to honour, so nothing is truncated and nothing is invented.
		if it.Meta == "" {
			return indent + it.Title
		}
		return indent + it.Title + indent + it.Meta
	}
	// A row narrower than the indent alone: emit what fits and stop.
	if width <= len(indent) {
		return truncate(indent, width)
	}
	body := width - len(indent)

	if it.Meta == "" {
		return indent + truncate(it.Title, body)
	}

	// Minimum two spaces of breathing room between the title and the status, so the two never read as
	// one string ("...Item Tirunning").
	const gap = 2

	// THE STATUS WINS THE SPACE. If the meta cannot fit alongside the gap, print it in what there is —
	// the operator needs "running" more than a partial title.
	if len([]rune(it.Meta))+gap > body {
		return indent + truncate(it.Meta, body)
	}

	// The title gets everything left after the status, the gap, and the indent.
	titleRoom := body - gap - len([]rune(it.Meta))
	if titleRoom < 1 {
		return indent + truncate(it.Meta, body)
	}
	t := truncate(it.Title, titleRoom)
	pad := body - len([]rune(t)) - len([]rune(it.Meta))
	if pad < 1 {
		pad = 1
	}
	return indent + t + strings.Repeat(" ", pad) + it.Meta
}

// truncate shortens s to at most w RUNES, marking the cut with an ellipsis so a shortened value is
// never mistaken for the whole one. A wide glyph counts as one rune, which is why the row budget is
// counted in runes throughout (see listRow).
//
// TruncateRunes is the same function, EXPORTED, because a screen that composes a field INTO a row — an
// execution's work-item title, bounded so the row stays scannable — needs the same rule the row itself
// obeys. The alternative is a screen growing its own byte-slicing shortcut, which cuts UTF-8 in half.
func truncate(s string, w int) string { return TruncateRunes(s, w) }

// TruncateRunes shortens s to at most w runes, ending with an ellipsis when it had to cut. w <= 0
// returns s unchanged ("no budget stated" rather than "show nothing").
func TruncateRunes(s string, w int) string {
	if w <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:1])
	}
	return string(r[:w-1]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Frame is the screenkit frame helper: it normalizes a screen's composed
// block into EXACTLY h rows of w cells, painting the opaque theme
// background on every padding cell (ANSI-aware truncation when the block
// is wider than the region). Every screen returns Frame(...) from View()
// so its panes FILL the content region the shell budgeted — never a
// content-sized box floating at the top-left. A non-positive region is
// returned unchanged (an unsized screen must not collapse to 1×1).
func Frame(content string, w, h int) string {
	if w < 1 || h < 1 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		cols := lipgloss.Width(l)
		if cols > w {
			l = ansi.Truncate(l, w, "")
			cols = w
		}
		if cols < w {
			l += strings.Repeat(" ", w-cols)
		}
		lines[i] = theme.PanelBgStyle.Render(l)
	}
	for len(lines) < h {
		lines = append(lines, theme.PanelBgStyle.Render(strings.Repeat(" ", w)))
	}
	return strings.Join(lines, "\n")
}

// guard against unused import when styles evolve
var _ = lipgloss.NewStyle
var _ = fmt.Sprintf
