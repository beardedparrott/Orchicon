package tui

// rightrail.go — the Ask screen's CONVERSATIONS right rail (GUI Ask sidebar).

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// ConversationsRailWidth is the right rail's width (cells).
const ConversationsRailWidth = 38

// railDividerText is the rail's one-cell left border column.
const railDividerText = "│"

// railTopRow is the absolute terminal row of the rail's FIRST line (its header).
const railTopRow = tabBarRows + 1

// railVisible reports whether the Ask right rail should be rendered.
func (m *App) railVisible() bool {
	if m.active != TabAsk {
		return false
	}
	if m.chatConvID != "" {
		return true // continuing a session
	}
	// rightRailOpen IS CONSULTED, and that is the whole point of ctrl+r: its route and its help entry
	// both say "toggle conversations rail", but railVisible ignored the flag.
	return m.askMode == askConversations && m.rightRailOpen
}

// toggleRightRail collapses/expands the Ask conversations rail (ctrl+r).
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

// railRetryHit reports whether an absolute Y is inside the rail's retry area.
func (m *App) railRetryHit(absoluteY int) bool {
	return m.convErr != "" && absoluteY > railTopRow
}

// rightRailView renders the CONVERSATIONS rail as a REAL bordered pane (the same kit2.Panel the
// screens use) at ConversationsRailWidth, joined horizontally to the main column.
func (m *App) rightRailView() string {
	w := ConversationsRailWidth
	h := m.contentHeight() + m.dock.Lines()
	innerW := w - 4 // panel border (2 cells) + a 1-cell gutter each side
	if innerW < 8 {
		innerW = 8
	}

	title := "Conversations"
	// THE ACTIVE WORKSPACE IS THE RAIL'S TITLE, always — the operator: "In the conversations list rail
	// at the top, it should say the project name that is currently selected." A scope you cannot see.
	label := projectScopeLabel(m.projectScope, projectScopeOptions(m.railProjects, m.conversations))
	title = "Conversations · " + label
	if n := len(m.scopedConversations()); n > 0 && m.convErr == "" {
		title = title + fmt.Sprintf(" (%d)", n)
	}

	var body []string
	switch {
	case m.convErr != "":
		// Explicit retry state — never a silent empty rail (finding 9).
		why := firstLine(m.convErr)
		if isAuthErrText(m.convErr) {
			why = "/connect to re-auth"
		}
		body = append(body,
			theme.ErrorText.Render(truncateRight("⚠ conversations unavailable", innerW)),
			theme.HintText.Render(truncateRight(why, innerW)),
			theme.HintText.Render(truncateRight("[ click to retry ]", innerW)),
		)
	case m.convLoading && len(m.conversations) == 0:
		body = append(body, theme.HintText.Render(truncateRight("loading conversations…", innerW)))
	case len(m.conversations) == 0:
		body = append(body, theme.HintText.Render(truncateRight("none yet — type below", innerW)))
	case len(m.scopedConversations()) == 0:
		// A SCOPE THAT HOLDS NOTHING IS NOT AN EMPTY RAIL.
		body = append(body,
			theme.HintText.Render(truncateRight("none in "+label, innerW)),
			theme.HintText.Render(truncateRight("/project to switch", innerW)),
		)
	default:
		// THE RAIL RENDERS ITS ROWS, not m.conversations: a grouping is a row of its own, and the members
		// of a collapsed folder are not rows at all.
		all := m.railRows()
		drawn := 0
		for i := m.convScroll; i < len(all) && drawn < h-5; i++ {
			body = append(body, m.railLine(all[i], i, innerW))
			drawn++
		}
		end := m.convScroll + drawn
		if end > len(all) {
			end = len(all)
		}

		body = append(body, theme.HintText.Render(truncateRight(fmt.Sprintf("%d-%d/%d", m.convScroll+1, end, len(all)), innerW)))
		// NO CHORD LIST HERE.
	}

	p := kit2.NewPanel(title, w, h)
	// THE RAIL SHOWS FOCUS.
	p.Focused = m.railFocused()
	p.SetContent(strings.Join(body, "\n"))
	return p.View()
}

// railLine renders one rail row: a grouping folder (with its arrow and count) or a conversation.
func (m *App) railLine(r railRow, i, w int) string {
	if r.folder {
		arrow := "▾"
		if m.convCollapsed[r.catID] {
			arrow = "▸"
		}
		// The count is the FOLDER's total, not its visible members: collapsing must not make the number
		// change, or the operator would think items vanished.
		meta := fmt.Sprintf("%d", r.count)
		title := arrow + " " + r.title
		avail := w - 1 - lipgloss.Width(meta)
		if lipgloss.Width(title) > avail {
			title = truncateRight(title, avail)
		}
		line := " " + title
		pad := w - 1 - lipgloss.Width(title) - lipgloss.Width(meta)
		if pad < 1 {
			pad = 1
		}
		line = truncateRight(line+strings.Repeat(" ", pad)+meta, w)
		if i == m.convSel {
			return theme.ListItemSelected.Render(line)
		}
		return theme.ListItem.Render(line)
	}
	if r.conv < 0 || r.conv >= len(m.conversations) {
		return ""
	}
	row := m.conversationRow(m.conversations[r.conv], i, w)
	// A conversation nests one cell deeper than the old single level when it sits under a project, so
	// the hierarchy is visible on the rows too — not only on the folders above them.
	if r.depth > 1 {
		row = truncateRight(strings.Repeat(" ", r.depth-1)+row, w)
	}
	return row
}

// conversationRow renders one rail conversation row (title + meta).
func (m *App) conversationRow(c chat.Conversation, i, w int) string {
	meta := fmt.Sprintf("%d msgs", c.MessageN)
	// THE UNION, not the polled field alone.
	if m.runningFor(c.ID) {
		meta = "running"
	}
	if m.projectScope == projectScopeAll {
		if label := m.projLabelForConv(c); label != "" {
			meta = label + " · " + meta
		}
	}
	marker := " "
	if m.convMarked[c.ID] {
		marker = "✓"
	}
	title := c.Title
	avail := w - 1 - lipgloss.Width(meta)
	if lipgloss.Width(title) > avail {
		title = truncateRight(title, avail)
	}
	row := marker + title
	pad := w - 1 - lipgloss.Width(title) - lipgloss.Width(meta)
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

// railRowAt maps an ABSOLUTE terminal Y to a conversation index.
func (m *App) railRowAt(absoluteY int) (int, bool) {
	line := absoluteY - railTopRow // 0 = header, 1 = first row
	if line < 1 {
		return 0, false
	}
	idx := m.convScroll + (line - 1)
	if idx >= 0 && idx < len(m.railRows()) {
		return idx, true
	}
	return 0, false
}

// railHeaderHit reports whether an absolute Y hit the rail header row
// (the collapse toggle).
func (m *App) railHeaderHit(absoluteY int) bool {
	return absoluteY == railTopRow
}

// openRailConversation opens the selected rail conversation (deliberate navigation — never
// auto-opened at launch).
func (m *App) openRailConversation(row int) tea.Cmd {
	// A FOLDER ROW TOGGLES.
	if f := m.railFolderAt(row); f != nil {
		m.toggleConvFolder(f.catID)
		return nil
	}
	idx := m.railConvIndexAt(row)
	if idx < 0 {
		return nil
	}
	m.convSel = row
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

// selectRailConversation moves the rail selection (up/down keys while the rail is focused) and
// keeps it on screen.
func (m *App) selectRailConversation(delta int) {
	rows := m.railRows()
	if len(rows) == 0 {
		return
	}
	m.convSel += delta
	if m.convSel < 0 {
		m.convSel = 0
	}
	if m.convSel >= len(rows) {
		m.convSel = len(rows) - 1
	}
	m.railFollowSelection()
}

// railFollowSelection scrolls the rail so the highlighted conversation stays inside the window the
// rail actually renders.
func (m *App) railFollowSelection() {
	rows := m.railVisibleRows()
	if rows < 1 {
		rows = 1
	}
	if m.convSel < m.convScroll {
		m.convScroll = m.convSel
	}
	if m.convSel >= m.convScroll+rows {
		m.convScroll = m.convSel - rows + 1
	}
	maxOff := len(m.railRows()) - rows
	if maxOff < 0 {
		maxOff = 0
	}
	if m.convScroll > maxOff {
		m.convScroll = maxOff
	}
	if m.convScroll < 0 {
		m.convScroll = 0
	}
}

// openSelectedRailConversation opens the highlighted rail conversation — the Enter/Space gesture on
// the Ask tab (the operator's "space or enter selects").
func (m *App) openSelectedRailConversation() tea.Cmd {
	if len(m.railRows()) == 0 {
		return nil
	}
	return m.openRailConversation(m.convSel)
}

// railFocused reports whether the conversations rail holds the keyboard on the Ask tab.
func (m *App) railFocused() bool {
	return m.active == TabAsk && m.railVisible() && m.askPane == askPaneRail
}

// railVisibleRows is the number of CONVERSATION rows the rail actually renders.
func (m *App) railVisibleRows() int {
	h := m.contentHeight() + m.dock.Lines()
	if h-5 < 1 {
		return 1
	}
	return h - 5
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

// railSyncSelectionToOpenConversation points the rail's highlight at the conversation
// CURRENTLY ON SCREEN.
//
// THE BUG THIS FIXES. convSel is the rail's own cursor — an INDEX into railRows() — and the
// highlight is rendered purely as `i == convSel`. Nothing ever re-derived it from chatConvID,
// the conversation the transcript is actually showing. So every route that opens a
// conversation WITHOUT going through the rail left the two disagreeing:
//
//   - a conversation created by sending (chatConvCreatedMsg sets chatConvID directly),
//   - opening one from a search hit, a mention, or /switch,
//   - a conversation opened while the rail was on a different row and the list later reloaded
//     (a rename or a new message reorders the rows, so the SAME index now points at a
//     different conversation).
//
// The operator's report: "the highlighted bar on the conversation is not always in sync with
// the current conversation on the screen and it's very confusing." Exactly that — the
// highlight meant "where the cursor is", the screen meant "what I opened", and nothing kept
// them together.
//
// IT IS A NO-OP WHEN THE RAIL ALREADY AGREES, and it never moves the highlight to a hidden
// row: a conversation the rail cannot currently show (filtered out, or inside a collapsed
// folder) leaves the cursor where it is rather than being silently relocated. That case is
// genuinely ambiguous, and moving the highlight somewhere arbitrary would be a second
// confusion in place of the first.
func (m *App) railSyncSelectionToOpenConversation() {
	if m.chatConvID == "" {
		return
	}
	rows := m.railRows()
	if len(rows) == 0 {
		return
	}
	// Already correct: leave it alone, so an explicit cursor move is never undone.
	if m.convSel >= 0 && m.convSel < len(rows) && !rows[m.convSel].folder &&
		rows[m.convSel].conv >= 0 && rows[m.convSel].conv < len(m.conversations) &&
		m.conversations[rows[m.convSel].conv].ID == m.chatConvID {
		return
	}
	for i, r := range rows {
		if r.folder || r.conv < 0 || r.conv >= len(m.conversations) {
			continue
		}
		if m.conversations[r.conv].ID == m.chatConvID {
			m.convSel = i
			m.railFollowSelection()
			return
		}
	}
	// The open conversation is not visible in the rail — leave the cursor where it is.
}
