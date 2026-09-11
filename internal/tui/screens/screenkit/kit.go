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
	if l.NextPageToken != "" {
		title += theme.HintText.Render("  (more pages: press f)")
	}
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
	dirty    bool   // Body changed since the viewport last loaded it

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
	if body != d.vpBody {
		d.dirty = true
	}
}

// SetHero installs the centered empty-state block, shown until real
// content replaces it via SetContent.
func (d *Detail) SetHero(title, body string) {
	d.Hero, d.HeroTitle, d.HeroBody = true, title, body
}

// ensureVP (re)initializes the viewport for the current pane size.
func (d *Detail) ensureVP() {
	w, h := d.Width, d.Height-2
	if w < 1 {
		w = 1
	}
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
			// Reload the viewport but KEEP the operator's scroll: follow
			// the tail only when they were already at the bottom (a live
			// chat), otherwise restore the exact line offset (reading
			// history must not be yanked away by the next chunk).
			atBottom := d.vp.AtBottom()
			offset := d.vp.YOffset
			d.vp.SetContent(d.Body)
			d.vpBody = d.Body
			d.dirty = false
			if atBottom {
				d.vp.GotoBottom()
			} else {
				d.vp.SetYOffset(offset)
			}
		}
		b.WriteString(d.vp.View())
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
