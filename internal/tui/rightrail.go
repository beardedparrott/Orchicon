package tui

// rightrail.go — the Ask screen's CONVERSATIONS right rail (GUI Ask
// sidebar). OPEN by default; collapsible via ctrl+r (documented key) and
// a mouse click on the rail header. State persists for the session. This
// is the RIGHT rail; the diff pane is the LEFT rail. The center column
// (chat + composer) reflows between the two.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// ConversationsRailWidth is the right rail's width (cells). Mirrors the
// GUI Ask sidebar (~360px) proportionally at a typical 96-col terminal.
const ConversationsRailWidth = 30

// railVisible reports whether the Ask right rail should be rendered. It is
// only drawn on the Ask screen (the GUI Ask sidebar lives there; Overview
// may reuse it but the shell keeps the rail Ask-scoped for now).
func (m *App) railVisible() bool {
	return m.active == TabAsk && m.rightRailOpen
}

// toggleRightRail collapses/expands the Ask conversations rail (ctrl+r).
func (m *App) toggleRightRail() {
	m.rightRailOpen = !m.rightRailOpen
	m.refreshLayout()
}

// refreshLayout re-applies the current width/height to the active screen
// and dock (the right rail and diff pane both consume width). Called after
// toggling a rail so content reflows immediately.
func (m *App) refreshLayout() {
	if s := m.screens[m.active]; s != nil && m.width > 0 {
		s.SetSize(m.contentWidth(), m.contentHeight())
	}
	if m.width > 0 {
		m.dock.Width = m.contentWidth()
	}
	if m.diffPane != nil {
		m.diffPane.SetSize(DiffPaneWidth, m.contentHeight()+m.dock.Lines())
	}
}

// rightRailView renders the CONVERSATIONS rail (header + scrollable list)
// at the fixed ConversationsRailWidth. The rail is joined horizontally to
// the main column (joining at Top), so its first line sits at the same
// terminal row as the main screen's first line (row 2 after the tab bar).
func (m *App) rightRailView() string {
	w := ConversationsRailWidth
	h := m.contentHeight() + m.dock.Lines()
	var b strings.Builder

	// Header row (rail line 0). Clicking it toggles collapse.
	b.WriteString(theme.TabActive.Render(" CONVERSATIONS ▾"))
	b.WriteString("\n")

	// Conversation rows (rail lines 1..).
	rows := 0
	for i := m.convScroll; i < len(m.conversations) && rows < h-2; i++ {
		c := m.conversations[i]
		meta := fmt.Sprintf("%d msgs", c.MessageN)
		if c.TurnInFly {
			meta = "running"
		}
		row := " " + c.Title
		pad := w - 3 - len([]rune(c.Title)) - len([]rune(meta))
		if pad < 1 {
			pad = 1
		}
		row += strings.Repeat(" ", pad) + meta
		if i == m.convSel {
			b.WriteString(theme.ListItemSelected.Render(truncateRight(row, w)))
		} else {
			b.WriteString(theme.ListItem.Render(truncateRight(row, w)))
		}
		b.WriteString("\n")
		rows++
	}
	if len(m.conversations) == 0 {
		b.WriteString(theme.HintText.Render(" none — start a chat") + "\n")
	}

	// Scroll indicator (rail line after the list).
	end := m.convScroll + rows
	if end > len(m.conversations) {
		end = len(m.conversations)
	}
	if len(m.conversations) > 0 {
		b.WriteString(theme.HintText.Render(fmt.Sprintf(" %d-%d/%d", m.convScroll+1, end, len(m.conversations))))
	}

	// The header/list got themed at width; pad each line to the rail width
	// so the horizontal join stays aligned (no ragged right edge).
	return alignRail(b.String(), h)
}

// alignRail pads each rail line to ConversationsRailWidth cells so the
// joined layout keeps a clean right boundary.
func alignRail(s string, h int) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		delta := ConversationsRailWidth - lipgloss.Width(lines[i])
		if delta > 0 {
			lines[i] += strings.Repeat(" ", delta)
		}
	}
	return strings.Join(lines, "\n")
}

// railRowAt maps an ABSOLUTE terminal Y to a conversation index. The rail
// is joined to the main at Top, so the rail's line 0 is the header which
// sits at absolute row 2 (after the 2-row tab bar); conversation rows
// start at absolute row 3.
func (m *App) railRowAt(absoluteY int) (int, bool) {
	const tabBarRows = 2
	line := absoluteY - tabBarRows // 0 = header, 1 = first conversation
	if line < 1 {
		return 0, false
	}
	idx := m.convScroll + (line - 1)
	if idx >= 0 && idx < len(m.conversations) {
		return idx, true
	}
	return 0, false
}

// railHeaderHit reports whether an absolute Y hit the rail header row
// (the collapse toggle).
func (m *App) railHeaderHit(absoluteY int) bool {
	const tabBarRows = 2
	return absoluteY == tabBarRows
}

// openRailConversation opens the selected rail conversation (deliberate
// navigation — never auto-opened at launch).
func (m *App) openRailConversation(idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.conversations) {
		return nil
	}
	m.convSel = idx
	return m.OpenAskConversation(m.conversations[idx].ID)
}

// selectRailConversation moves the rail selection (up/down keys while the
// rail is focused) and opens the detail on enter.
func (m *App) selectRailConversation(delta int) {
	if len(m.conversations) == 0 {
		return
	}
	m.convSel += delta
	if m.convSel < 0 {
		m.convSel = 0
	}
	if m.convSel >= len(m.conversations) {
		m.convSel = len(m.conversations) - 1
	}
}

// railVisibleRows is the number of list rows visible in the rail.
func (m *App) railVisibleRows() int {
	h := m.contentHeight() + m.dock.Lines()
	if h-2 < 1 {
		return 1
	}
	return h - 2
}

// truncateRight truncates s to width w cells, appending an ellipsis.
func truncateRight(s string, w int) string {
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
