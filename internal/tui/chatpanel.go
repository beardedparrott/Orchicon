package tui

// chatpanel.go — the slide-out conversation strip.
//
// WHY THIS EXISTS: the composer is context-aware and lives on every screen, so
// a message sent from Work / Execution / Control reaches Ask Orchicon without
// the operator being able to see it. That "invisible send" is the worst kind of
// failure — the message and its reply are real but off-screen. This panel makes
// the destination VISIBLE: engaging the composer on any screen other than Ask
// slides out a small conversation window just above the input, pinned to that
// screen's context, and the reply streams into it in place.
//
// It is deliberately NOT shown when:
//   - the active screen is Ask (the real transcript is already on screen), or
//   - a running execution is selected (the interjection's reply streams into
//     the execution session view the operator is already watching).
//
// Esc minimises it back into the prompt; [continue in conversations] (or ctrl+z)
// escalates to Ask → Conversations with THIS conversation selected.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// panelDefaultRows is the slide-out panel's height in rows. It is bounded to
// half the content region so it can never starve the screen it sits over.
const panelDefaultRows = 9

// panelEscalateLabel is the affordance that jumps to the full conversation
// view. Bracketed per the operator's ask; clickable, and bound to ctrl+z.
const panelEscalateLabel = "[continue in conversations]"

// panelVisible reports whether the conversation strip should be drawn.
func (m *App) panelVisible() bool {
	if !m.panelOpen {
		return false
	}
	// No active screen yet, or the Ask tab: nothing to slide the strip over.
	// (Ask shows the real transcript, so a second copy would be redundant.)
	if m.active == "" || m.active == TabAsk {
		return false
	}
	if _, ok := m.runningExecutionID(); ok {
		return false // an interjection's reply streams into the session view
	}
	return true
}

// panelRows returns the rows the strip occupies (0 when hidden), bounded to
// half the content region.
func (m *App) panelRows() int {
	if !m.panelVisible() {
		return 0
	}
	h := panelDefaultRows
	region := m.contentHeight()
	if region > 0 && h > region/2 {
		h = region / 2
	}
	if h < 3 {
		h = 3
	}
	return h
}

// screenRows is the display budget for the active screen: the content region
// minus the strip when it is out. Screens are sized to THIS so the strip takes
// its rows from the screen (rather than overlaying content being read).
func (m *App) screenRows() int {
	h := m.contentHeight() - m.panelRows()
	if h < 1 {
		h = 1
	}
	return h
}

// panelTopRow returns the absolute terminal row of the strip's FIRST line, or
// -1 when it is hidden. Used by the mouse router.
func (m *App) panelTopRow() int {
	if !m.panelVisible() {
		return -1
	}
	return tabBarRows + 1 + m.screenRows()
}

// panelEscalateX returns the [start, end) columns of the escalate affordance on
// the strip's header row, for hit-testing.
func (m *App) panelEscalateX(w int) (int, int) {
	label := panelEscalateLabel
	end := w - 2
	start := end - lipgloss.Width(label)
	if start < 0 {
		start = 0
	}
	return start, end
}

// openPanel slides the strip out (idempotent).
func (m *App) openPanel() {
	if m.panelOpen {
		return
	}
	m.panelOpen = true
	m.panelScroll = 0
	m.refreshLayout()
}

// closePanel minimises the strip back into the prompt (idempotent).
func (m *App) closePanel() {
	if !m.panelOpen {
		return
	}
	m.panelOpen = false
	m.panelScroll = 0
	m.refreshLayout()
}

// escalateToConversation jumps to Ask → Conversations with the panel's
// conversation selected, so the operator lands mid-stream in the real view.
func (m *App) escalateToConversation() {
	id := m.chatConvID
	m.closePanel()
	m.askMode = askConversations
	if m.active != TabAsk {
		m.SwitchTo(TabAsk)
	}
	m.EnsureSubscriptions(TabAsk)
	if id != "" {
		// Open it explicitly so the transcript pane follows the selection.
		m.chatConvID = ""
		m.pendingScreenCmd = tea.Batch(m.pendingScreenCmd, m.OpenAskConversation(id))
	}
	m.refreshLayout()
}

// chatPanelView renders the strip at width w and its allotted height.
func (m *App) chatPanelView(w int) string {
	h := m.panelRows()
	if w < 8 || h < 2 {
		return ""
	}
	inner := w - 4 // a 1-cell gutter either side, like the other panels

	// Header: the conversation identity on the left, the escalate affordance
	// right-aligned (the clickable target).
	title := "Conversation"
	if m.chatConvID == "" {
		title = "New conversation"
	}
	left := theme.ListTitle.Render(" " + title)
	bx0, bx1 := m.panelEscalateX(w)
	btn := theme.StatusOK.Render(panelEscalateLabel)
	pad := bx0 - lipgloss.Width(left)
	if pad < 1 {
		pad = 1
	}
	header := left + strings.Repeat(" ", pad) + btn
	if lipgloss.Width(header) < bx1 {
		header += strings.Repeat(" ", bx1-lipgloss.Width(header))
	}

	// The context that will be attached to the next send — so the operator can
	// see what the conversation is pinned to.
	chip := m.activeContextLabel()
	if m.contextOverride != "" {
		chip = m.contextOverride + " (pinned)"
	}
	chipLine := theme.ListMeta.Render(truncateSingle(" [context: "+chip+"]", inner))
	if chip == "" {
		chipLine = theme.HintText.Render(truncateSingle(" no screen context attached", inner))
	}

	// Body: the live transcript, scrolled (panelScroll = lines up from the end).
	body := m.panelTranscript(inner, h-2)

	out := []string{truncateRight(header, w), chipLine}
	out = append(out, body...)
	return strings.Join(out, "\n")
}

// panelTranscript renders the conversation's items into EXACTLY n rows, scrolled
// by panelScroll lines up from the bottom (0 = follow the tail).
func (m *App) panelTranscript(w, n int) []string {
	if n < 1 {
		return nil
	}
	if m.chatConvID == "" {
		lines := []string{theme.HintText.Render(truncateSingle(" your message starts a new conversation", w))}
		for len(lines) < n {
			lines = append(lines, "")
		}
		return lines[:n]
	}
	items := m.chatStore.snapshot(m.chatConvID)
	rendered := chat.RenderItems(chat.GroupByPhase(items), w)
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	// The live tail is the interesting end; scroll is measured from there.
	maxScroll := len(lines) - n
	if maxScroll < 0 {
		maxScroll = 0
	}
	off := maxScroll - m.panelScroll
	if off < 0 {
		off = 0
	}
	end := off + n
	if end > len(lines) {
		end = len(lines)
	}
	out := append([]string{}, lines[off:end]...)
	for len(out) < n {
		out = append(out, "")
	}
	return out[:n]
}

// truncateSingle clips one line to w cells (ANSI-aware).
func truncateSingle(s string, w int) string {
	if w < 1 {
		return s
	}
	return truncateRight(s, w)
}
