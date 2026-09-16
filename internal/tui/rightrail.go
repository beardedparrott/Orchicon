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
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// ConversationsRailWidth is the right rail's width (cells). A real bordered
// pane now (not a divider column), widened 26 -> 32 per the operator's
// "keep it on the right side and make it a tad wider".
const ConversationsRailWidth = 32

// railDividerText is the rail's one-cell left border column.
const railDividerText = "│"

// railTopRow is the absolute terminal row of the rail's FIRST line (its
// header). The body region begins after the tab bar (row 0), the underline
// rule (row 1) and the one-row gap (row 2) — so the rail header is row 3
// and the first conversation row is row 4. (Phase 2c finding 7: the old
// hit-test used the tab-bar height 2, so a REAL click on the header landed
// on the gap row / first conversation instead of toggling the rail.)
const railTopRow = tabBarRows + 1

// railVisible reports whether the Ask right rail should be rendered.
//
// The operator wants the conversation list on the RIGHT and always on for
// MVP1 ("conversations on the right side … make it a tad smaller and always
// have it on"). The Ask screen therefore renders its transcript only — the
// rail owns the list, its selection and its mouse hit-test, so there is
// exactly ONE conversation list (the old duplicate drew three columns).
// railVisible reports whether the Ask right rail should be rendered.
//
// Drawn when Ask is showing the conversation view: while a session is open
// (continuing it), or when the operator explicitly chose Conversations from
// the tab's menu. The launch page ("New") deliberately shows no list — the
// operator asked for a clean first screen.
func (m *App) railVisible() bool {
	if m.active != TabAsk {
		return false
	}
	if m.chatConvID != "" {
		return true // continuing a session
	}
	return m.askMode == askConversations
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

// rightRailView renders the CONVERSATIONS rail as a REAL bordered pane (the
// same kit2.Panel the screens use) at ConversationsRailWidth, joined
// horizontally to the main column.
//
// Line semantics are unchanged for the hit-tests: rail line 0 is the panel's
// top border + title (the header/collapse row), and line 1 is the first
// conversation row — which is exactly what railRowAt/railHeaderHit assume.
func (m *App) rightRailView() string {
	w := ConversationsRailWidth
	h := m.contentHeight() + m.dock.Lines()
	innerW := w - 4 // panel border (2 cells) + a 1-cell gutter each side
	if innerW < 8 {
		innerW = 8
	}

	title := "Conversations"
	if n := len(m.conversations); n > 0 && m.convErr == "" {
		title = fmt.Sprintf("Conversations (%d)", n)
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
	default:
		rows := 0
		for i := m.convScroll; i < len(m.conversations) && rows < h-5; i++ {
			body = append(body, m.conversationRow(m.conversations[i], i, innerW))
			rows++
		}
		end := m.convScroll + rows
		if end > len(m.conversations) {
			end = len(m.conversations)
		}

		body = append(body, theme.HintText.Render(truncateRight(fmt.Sprintf("%d-%d/%d", m.convScroll+1, end, len(m.conversations)), innerW)))
		// NO CHORD LIST HERE. The rail advertises its actions in the COMPOSER's affordance row now
		// (shell.railHintLine): this pane is 32 cells wide with 28 of inner text, and the chord list
		// was TRUNCATED mid-word inside it — "ctrl+n: rename · ctrl+t: ca…" in the operator's
		// screenshot — while the composer is where the rail's keys are actually driven from.
		//
		// The marked count stays: it is rail state, it fits, and it is the one number the operator
		// needs while building a selection.
	}

	p := kit2.NewPanel(title, w, h)
	p.Focused = false
	p.SetContent(strings.Join(body, "\n"))
	return p.View()
}

// conversationRow renders one rail conversation row (title + meta).
//
// A MARKED row shows a marker in the gutter, so a selection is visible while it is being built — the
// cursor alone cannot express "and these four as well". The marker is one cell and the title is
// truncated to the remaining width, so the meta column stays aligned on marked and unmarked rows
// alike (a marker that shifted the numbers would make the list jump as the operator spaces down it).
func (m *App) conversationRow(c chat.Conversation, i, w int) string {
	meta := fmt.Sprintf("%d msgs", c.MessageN)
	if c.TurnInFly {
		meta = "running"
	}
	marker := " "
	if m.convMarked[c.ID] {
		marker = "✓"
	}
	title := c.Title
	// w-1 for the gutter, and the marker takes one cell of it: the title keeps whatever is left.
	avail := w - 1
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
// rail is focused) and keeps it on screen. Opening is the Enter/Space
// gesture (openSelectedRailConversation), not the arrow keys.
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
	m.railFollowSelection()
}

// railFollowSelection scrolls the rail so the highlighted conversation stays
// inside the window the rail actually renders.
//
// Without this the selection index moved but convScroll stayed put, so on a
// list longer than the window the highlight walked off the bottom and the
// rail looked frozen — the operator's "the conversation changes, but the
// highlight on the current conversation in the list does not".
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
	maxOff := len(m.conversations) - rows
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

// openSelectedRailConversation opens the highlighted rail conversation — the
// Enter/Space gesture on the Ask tab (the operator's "space or enter
// selects"). No-op when the list is empty.
func (m *App) openSelectedRailConversation() tea.Cmd {
	if len(m.conversations) == 0 {
		return nil
	}
	return m.openRailConversation(m.convSel)
}

// railVisibleRows is the number of CONVERSATION rows the rail actually
// renders. rightRailView lays out `rows < h-5`: the panel's two border rows,
// the title row, and the one-row "n-m/total" footer. The scroll-follow and
// the wheel clamp both use this, so the visible window and the scroll maths
// cannot disagree (they did: this used to claim h-2 rows the rail never drew).
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
