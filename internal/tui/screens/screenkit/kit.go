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
		row := "  " + it.Title
		if it.Meta != "" {
			pad := l.Width - 4 - len([]rune(it.Title)) - len([]rune(it.Meta))
			if pad < 1 {
				pad = 1
			}
			row += strings.Repeat(" ", pad) + it.Meta
		}
		if i == l.Cursor {
			b.WriteString(theme.ListItemSelected.Render(truncate(row, l.Width)))
		} else {
			b.WriteString(theme.ListItem.Render(truncate(row, l.Width)))
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
	// vpTitle is the TITLE the loaded body belongs to. It is the detail's
	// IDENTITY: a body that changes under the same title is the same item
	// updating (a live transcript appending), while a new title is a DIFFERENT
	// item — and a different item must start at the top.
	vpTitle string
	dirty   bool // Body changed since the viewport last loaded it
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
	d.Title, d.Fields, d.Body = title, fields, body
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
	// Rows View() spends on everything that is not the scrolling body.
	overhead := 1 + len(d.Fields) // title + fields
	if d.Body != "" {
		overhead += 2 // the blank separator + at least something in the viewport
	}
	overhead += d.footerRows()
	h := d.Height - 2 - overhead
	if h < 1 {
		h = 1
	}
	if !d.initOnce {
		d.vp = viewport.New(w, h)
		d.initOnce = true
		d.dirty = true
	}
	d.vp.Width, d.vp.Height = w, h
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
		if d.dirty || d.vpBody != d.Body {
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
			d.vp.SetContent(d.Body)
			d.vpBody = d.Body
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

func truncate(s string, w int) string {
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
		lines[i] = theme.ScreenBg.Render(l)
	}
	for len(lines) < h {
		lines = append(lines, theme.ScreenBg.Render(strings.Repeat(" ", w)))
	}
	return strings.Join(lines, "\n")
}

// guard against unused import when styles evolve
var _ = lipgloss.NewStyle
var _ = fmt.Sprintf
