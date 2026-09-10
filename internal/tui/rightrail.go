package tui

// rightrail.go — the Ask screen's CONVERSATIONS right rail (GUI Ask
// sidebar). OPEN by default; collapsible via ctrl+r (documented key) and
// a mouse click on the rail header. State persists for the session. This
// is the RIGHT rail; the diff pane is the LEFT rail. The center column
// (chat + composer) reflows between the two.
//
// Phase 2c (operator finding 9): the rail renders as a PROPER rail — a
// one-cell left divider column so it reads as a docked rail rather than a
// floating box — and it is never a SILENT empty rail: while a load is in
// flight it shows a loading row, on an auth/API failure it shows an
// explicit retry state (error text + a clickable retry area), and its
// list comes from the LIVE API (chat.LoadConversations).

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// ConversationsRailWidth is the right rail's width (cells), INCLUDING the
// one-cell left divider that makes it read as a docked rail. Mirrors the
// GUI Ask sidebar (~360px) proportionally at a typical 96-col terminal.
const ConversationsRailWidth = 30

// railDividerText is the rail's one-cell left border column.
const railDividerText = "│"

// railTopRow is the absolute terminal row of the rail's FIRST line (its
// header). The body region begins after the tab bar (row 0), the underline
// rule (row 1) and the one-row gap (row 2) — so the rail header is row 3
// and the first conversation row is row 4. (Phase 2c finding 7: the old
// hit-test used the tab-bar height 2, so a REAL click on the header landed
// on the gap row / first conversation instead of toggling the rail.)
const railTopRow = tabBarRows + 1

// railVisible reports whether the Ask right rail should be rendered. It is
// only drawn on the Ask screen (the GUI Ask sidebar lives there; Overview
// may reuse it but the shell keeps the rail Ask-scoped for now).
func (m *App) railVisible() bool {
	return m.active == TabAsk && m.rightRailOpen
}

// toggleRightRail collapses/expands the Ask conversations rail (ctrl+r).
// When the rail is up but its last load FAILED, ctrl+r retries in place
// instead of hiding the failure (never a silent empty rail). When the rail
// is (re)opened and was never populated — or failed — it fetches the list
// from the live API.
func (m *App) toggleRightRail() {
	if m.rightRailOpen && m.convErr != "" {
		m.pendingRailCmd = m.reloadConversations()
		return
	}
	m.rightRailOpen = !m.rightRailOpen
	if m.rightRailOpen && (m.convErr != "" || !m.convLoaded) {
		m.pendingRailCmd = m.reloadConversations()
	}
	m.refreshLayout()
}

// railRetryHit reports whether an absolute Y is inside the rail's retry
// area. An ERRORED rail retries on any click below its header — there is
// no list to click, so the whole body is the retry affordance.
func (m *App) railRetryHit(absoluteY int) bool {
	return m.convErr != "" && absoluteY > railTopRow
}

// rightRailView renders the CONVERSATIONS rail (header + scrollable list)
// at the fixed ConversationsRailWidth. The rail is joined horizontally to
// the main column (joining at Top), so its first line sits at the same
// terminal row as the main screen's first line (row 2 after the tab bar).
func (m *App) rightRailView() string {
	contentW := ConversationsRailWidth - lipgloss.Width(railDividerText)
	h := m.contentHeight() + m.dock.Lines()

	// Header row (rail line 0). Clicking it toggles collapse.
	title := " CONVERSATIONS"
	if n := len(m.conversations); n > 0 && m.convErr == "" {
		title += fmt.Sprintf(" (%d)", n)
	}
	title += " ▾"
	lines := []string{theme.TabActive.Render(truncateRight(title, contentW))}

	switch {
	case m.convErr != "":
		// Explicit retry state — never a silent empty rail (finding 9):
		// the header says what failed, the middle line says WHY (or the
		// in-place re-auth path for a 401), the last line is the retry.
		why := " " + firstLine(m.convErr)
		if isAuthErrText(m.convErr) {
			why = " /connect to re-auth"
		}
		retry := "[ click here to retry ]"
		if !isAuthErrText(m.convErr) {
			retry = "[ click / ctrl+r to retry ]"
		}
		lines = append(lines,
			theme.ErrorText.Render(truncateRight(" ⚠ conversations unavailable", contentW)),
			theme.HintText.Render(truncateRight(why, contentW)),
			theme.HintText.Render(truncateRight(" "+retry, contentW)),
		)
	case m.convLoading && len(m.conversations) == 0:
		lines = append(lines, theme.HintText.Render(truncateRight(" loading conversations…", contentW)))
	case len(m.conversations) == 0:
		lines = append(lines, theme.HintText.Render(truncateRight(" none yet — type below to start a chat", contentW)))
	default:
		rows := 0
		for i := m.convScroll; i < len(m.conversations) && rows < h-2; i++ {
			lines = append(lines, m.conversationRow(m.conversations[i], i, contentW))
			rows++
		}
		end := m.convScroll + rows
		if end > len(m.conversations) {
			end = len(m.conversations)
		}
		lines = append(lines, theme.HintText.Render(truncateRight(fmt.Sprintf(" %d-%d/%d", m.convScroll+1, end, len(m.conversations)), contentW)))
	}

	// Prefix the divider column, then pad every line to the rail width so
	// the horizontal join stays aligned (no ragged right edge).
	prefixed := make([]string, len(lines))
	div := theme.HintText.Render(railDividerText)
	for i, l := range lines {
		prefixed[i] = div + l
	}
	return alignRail(strings.Join(prefixed, "\n"), h)
}

// conversationRow renders one rail conversation row (title + meta).
func (m *App) conversationRow(c chat.Conversation, i, w int) string {
	meta := fmt.Sprintf("%d msgs", c.MessageN)
	if c.TurnInFly {
		meta = "running"
	}
	row := " " + c.Title
	pad := w - 1 - len([]rune(c.Title)) - len([]rune(meta))
	if pad < 1 {
		pad = 1
	}
	row = truncateRight(row+strings.Repeat(" ", pad)+meta, w)
	if i == m.convSel {
		return theme.ListItemSelected.Render(row)
	}
	return theme.ListItem.Render(row)
}

// isAuthErrText reports whether an error text is a 401/UNAUTHENTICATED
// failure (the re-auth shape that names /connect).
func isAuthErrText(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "unauthenticated") || strings.Contains(l, "unauthorized")
}

// firstLine returns the first line of a (possibly multi-line) error text.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
// sits at absolute row 3 (after the tab chrome + the gap row); conversation
// rows start at absolute row 4.
func (m *App) railRowAt(absoluteY int) (int, bool) {
	line := absoluteY - railTopRow // 0 = header, 1 = first conversation
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
	return absoluteY == railTopRow
}

// openRailConversation opens the selected rail conversation (deliberate
// navigation — never auto-opened at launch). It opens BOTH the chat target
// (live chunks follow it) AND the Ask screen's conversation detail — the
// detail pane is where the transcript renders, so without it a rail click
// changed invisible state and the operator saw "nothing happened"
// (operator finding 9 + 7).
func (m *App) openRailConversation(idx int) tea.Cmd {
	if idx < 0 || idx >= len(m.conversations) {
		return nil
	}
	m.convSel = idx
	id := m.conversations[idx].ID
	var cmds []tea.Cmd
	if s := m.screens[TabAsk]; s != nil {
		if rd, ok := s.(interface {
			RequestDetail(src, id string) tea.Cmd
		}); ok {
			cmds = append(cmds, rd.RequestDetail("conversations", id))
		}
	}
	if cmd := m.OpenAskConversation(id); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
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
