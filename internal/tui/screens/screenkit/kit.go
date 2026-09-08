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
		b.WriteString(theme.ErrorText.Render("error: "+l.Err) + "\n")
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
}

// SetContent replaces the detail content and rewinds scroll.
func (d *Detail) SetContent(title string, fields []Field, body string) {
	d.Title, d.Fields, d.Body = title, fields, body
	d.initOnce = false
}

// Wheel scrolls the detail body.
func (d *Detail) Wheel(delta int) { d.vp.LineDown(delta) } // negative scrolls up via LineDown? no — clamp below

// Update lets the viewport handle messages.
func (d *Detail) Update(msg tea.Msg) {
	if !d.initOnce {
		h := d.Height - 2
		if h < 1 {
			h = 1
		}
		d.vp = viewport.New(d.Width, h)
		d.initOnce = true
	}
	d.vp.Width, d.vp.Height = d.Width, max(1, d.Height-2)
	d.vp.Update(msg)
}

// View renders the pane.
func (d *Detail) View() string {
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
		line := "  " + key + strings.Repeat(" ", keyWidth-len(key)) + val
		b.WriteString(theme.DetailKey.Render("  "+key) + " " + theme.DetailValue.Render(val) + "\n")
		_ = line
	}
	if d.Body != "" {
		b.WriteString("\n")
		if !d.initOnce {
			h := d.Height - 2
			if h < 1 {
				h = 1
			}
			d.vp = viewport.New(d.Width, h)
			d.initOnce = true
		}
		d.vp.Width, d.vp.Height = d.Width, max(1, d.Height-2)
		d.vp.SetContent(d.Body)
		b.WriteString(d.vp.View())
	}
	return b.String()
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

// guard against unused import when styles evolve
var _ = lipgloss.NewStyle
var _ = fmt.Sprintf
