package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// panelApp builds an app on a NON-Ask tab with the composer focused, which is
// where the slide-out strip lives.
func panelApp(t *testing.T) *App {
	t.Helper()
	m := newTestApp()
	m.RegisterScreen(TabWork, &stubScreen{id: "work"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabWork)
	m.setFocus(focusComposer)
	return m
}

// Engaging the composer on a non-Ask screen slides the strip out, so a send's
// destination is visible instead of the message landing somewhere off-screen.
func TestSlideOutPanelOpensOnTyping(t *testing.T) {
	m := panelApp(t)
	if m.panelVisible() {
		t.Fatal("the strip must start minimised")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("typing must slide the conversation strip out")
	}
	if rows := m.panelRows(); rows < 3 {
		t.Fatalf("strip rows = %d, want a usable height", rows)
	}
}

// It slides out for a click on the composer too (a mouse-first operator).
func TestSlideOutPanelOpensOnComposerClick(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 20, Y: m.height - 2,
	})
	m = nm.(*App)
	if !m.panelVisible() {
		t.Fatal("clicking the composer must slide the strip out")
	}
	if m.chatFocus != focusComposer {
		t.Fatal("clicking the composer must focus it")
	}
}

// Esc minimises it back into the prompt — the operator's ask — and a SECOND esc
// then moves focus to the content.
func TestSlideOutPanelMinimisesWithEsc(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("precondition: the strip is out")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.panelVisible() {
		t.Fatal("esc must minimise the strip")
	}
	if m.chatFocus != focusComposer {
		t.Fatal("minimising must leave the composer focused (back into the prompt)")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyEsc})
	m = nm
	if m.chatFocus != focusContent {
		t.Fatal("a second esc must move focus to the content")
	}
}

// It is NOT drawn on Ask (the real transcript is already on screen) nor while a
// running execution is selected (that reply streams into the session view).
func TestSlideOutPanelHiddenOnAsk(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	m.setFocus(focusComposer)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if m.panelVisible() {
		t.Fatal("the strip must not duplicate the Ask transcript")
	}
}

// The strip takes its rows from the screen's budget (never overlaying content),
// and the frame stays exact with it out at both floor sizes.
func TestSlideOutPanelTakesScreenRowsAndKeepsFrameExact(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		m := newTestApp()
		m.RegisterScreen(TabWork, &stubScreen{id: "work"})
		m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
		m.SwitchTo(TabWork)
		m.setFocus(focusComposer)

		before := m.screenRows()
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
		m = nm
		if !m.panelVisible() {
			t.Fatalf("%dx%d: strip did not open", w, h)
		}
		if after := m.screenRows(); after != before-m.panelRows() {
			t.Fatalf("%dx%d: screen rows %d -> %d, want %d (strip takes its rows)",
				w, h, before, after, before-m.panelRows())
		}
		lines := strings.Split(m.View(), "\n")
		if len(lines) != h {
			t.Fatalf("%dx%d: frame has %d rows, want exactly %d", w, h, len(lines), h)
		}
		for i, l := range lines {
			if got := len([]rune(lipglossStrip(l))); got != w {
				t.Fatalf("%dx%d: row %d visible width %d, want %d", w, h, i, got, w)
			}
		}
	}
}

// ctrl+z escalates: Ask → Conversations, with the strip's conversation selected
// so the operator lands mid-stream in the real view. The escalation must also
// minimise the strip.
func TestSlideOutPanelCtrlZEscalates(t *testing.T) {
	m := panelApp(t)
	m.chatConvID = "conv-panel"
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	if !m.panelVisible() {
		t.Fatal("precondition: the strip is out")
	}
	nm, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlZ})
	m = nm
	if m.panelVisible() {
		t.Fatal("escalating must minimise the strip")
	}
	if m.ActiveTab() != TabAsk {
		t.Fatalf("escalating must land on Ask, got %q", m.ActiveTab())
	}
	if m.askMode != askConversations {
		t.Fatal("escalating must land in the Conversations view")
	}
	if m.chatConvID != "conv-panel" {
		t.Fatalf("escalating must keep the conversation selected, got %q", m.chatConvID)
	}
}

// Clicking the bracketed affordance escalates, and the strip consumes clicks in
// its own rows rather than letting them fall through to the pane beneath.
func TestSlideOutPanelClickEscalatesAndConsumes(t *testing.T) {
	m := panelApp(t)
	m.chatConvID = "conv-click"
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	top := m.panelTopRow()
	if top < 0 {
		t.Fatal("strip has no top row while visible")
	}
	_, _, x0, x1 := m.panelButtonsX(m.width)
	upd, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: (x0 + x1) / 2, Y: top,
	})
	m = upd.(*App)
	if m.ActiveTab() != TabAsk || m.askMode != askConversations {
		t.Fatalf("clicking the affordance must escalate (tab=%q mode=%v)", m.ActiveTab(), m.askMode)
	}
}

// A body click in the strip must not be treated as a pane click (it is part of
// the compose area).
func TestSlideOutPanelBodyClickDoesNotReachThePane(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	top := m.panelTopRow()
	// A body row, centre column — would be a pane click without the guard.
	upd2, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 40, Y: top + 2,
	})
	m = upd2.(*App)
	if m.ActiveTab() != TabWork {
		t.Fatalf("a body click must not navigate away (tab=%q)", m.ActiveTab())
	}
	if m.chatFocus != focusComposer {
		t.Fatal("a body click keeps the composer focus (it is the compose area)")
	}
}

// Regression: the header's buttons must SURVIVE layout. The first cut built the
// header left-to-right and then truncated from the right, which cut the
// escalate button off entirely (the operator's "the continue button was there
// and now it's not even there"). Both affordances must be present and their
// returned hit columns must line up with where they are actually drawn.
func TestSlideOutPanelHeaderKeepsBothButtons(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	for _, w := range []int{80, 100, 120, 160} {
		m.dock.Width = w - ConversationsRailWidth
		view := m.chatPanelView(w)
		lines := strings.Split(view, "\n")
		if len(lines) == 0 {
			t.Fatalf("w=%d: no panel rendered", w)
		}
		header := lipglossStrip(lines[0])
		if !strings.Contains(header, panelMinimiseLabel) {
			t.Errorf("w=%d: header is missing %s: %q", w, panelMinimiseLabel, header)
		}
		if !strings.Contains(header, panelEscalateLabel) {
			t.Errorf("w=%d: header is missing %s: %q", w, panelEscalateLabel, header)
		}
		// The hit columns must bracket the drawn buttons.
		visible := []rune(header)
		minX0, minX1, eX0, eX1 := m.panelButtonsX(w)
		if eX1 > len(visible) {
			t.Fatalf("w=%d: escalate hit range %d..%d exceeds the header width %d", w, eX0, eX1, len(visible))
		}
		guessMin := string(visible[minX0:min(visible, minX1)])
		guessEsc := string(visible[eX0:min(visible, eX1)])
		if !strings.Contains(lipglossStrip(guessMin), "minim") {
			t.Errorf("w=%d: minimise hit columns %d..%d land on %q", w, minX0, minX1, guessMin)
		}
		if !strings.Contains(lipglossStrip(guessEsc), "continue") {
			t.Errorf("w=%d: escalate hit columns %d..%d land on %q", w, eX0, eX1, guessEsc)
		}
	}
}

func min(r []rune, n int) int {
	if n > len(r) {
		return len(r)
	}
	return n
}

// The minimise affordance collapses the strip (the operator asked for an
// explicit minimise on the box, not only Esc).
func TestSlideOutPanelMinimiseButtonCollapses(t *testing.T) {
	m := panelApp(t)
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = nm
	top := m.panelTopRow()
	minX0, minX1, _, _ := m.panelButtonsX(m.width)
	upd, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: (minX0 + minX1) / 2, Y: top,
	})
	m = upd.(*App)
	if m.panelVisible() {
		t.Fatal("clicking [minimize] must collapse the strip")
	}
}

// On the Ask tab a click in the composer is only a request to place the CURSOR:
// it must not slide a conversation on screen — on the launch page OR with a
// conversation already open.
//
// The operator: "Clicking into the text box on first load opens up a full
// conversation. I don't want that. Clicking into the composer on that first load
// should just focus the cursor into the chat so I can send a new message." —
// reported again while a conversation was open, which is why the suppression is
// scoped to the whole Ask tab rather than only the launch page.
//
// The strip's behaviour on OTHER tabs is a separate, deliberate intent and stays
// pinned by TestSlideOutPanelOpensOnComposerClick — this pins the Ask-tab
// exception, not a removal of that feature.
func TestComposerClickDoesNotOpenAConversationOnLaunchPage(t *testing.T) {
	m := newTestApp()
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	if !m.welcomeMode() {
		t.Fatal("precondition: the Ask launch page (hero, no conversation)")
	}

	nm, _ := m.dispatch(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 20, Y: m.height - 2,
	})
	m = nm

	if m.panelVisible() {
		t.Fatal("a click to place the cursor must not slide the conversation strip out on the launch page")
	}
	if m.chatConvID != "" {
		t.Fatalf("the click opened conversation %q — it must only focus the composer", m.chatConvID)
	}
	if m.chatFocus != focusComposer {
		t.Fatal("the click must still focus the composer")
	}
}

// The composer must be RENDERED at the width it is laid out to.
//
// baseView lays the dock into contentWidth() (which shrinks when the right rail
// appears) but dock.Width is assigned in refreshLayout, which does not re-run on
// that change. The stale, wider value made the composer build its rows for more
// columns than it was given, and normalizeBlock trimmed the TAIL of every row —
// so the stat row's numbers and the mode pill were cut off (the operator's "the
// context is off the screen").
func TestComposerRendersAtItsLaidOutWidth(t *testing.T) {
	m := panelApp(t)
	m.active = TabAsk
	// Open a conversation through the REAL path: that is what reveals the right
	// rail and therefore shrinks contentWidth(). Setting chatConvID directly
	// would skip the re-layout and hide the bug.
	m.chatConvID = ""
	m.OpenAskConversation("conv-1")
	if !m.railVisible() {
		t.Fatal("precondition: the rail is visible with a conversation open")
	}
	m.metrics = sessionMetrics{have: true, model: "orchicon/deepseek/deepseek-flash"}
	m.syncComposerStats()

	v := ansi.Strip(m.View())
	// The right edge of the stat row must survive: the cost and the mode pill.
	for _, want := range []string{"ctx ", "$0.0000", "[brainstorm]"} {
		if !strings.Contains(v, want) {
			t.Errorf("the stat row lost %q — the composer was rendered wider than its slot (dock.Width=%d, contentWidth=%d)",
				want, m.dock.Width, m.contentWidth())
		}
	}
	if m.dock.Width != m.contentWidth() {
		t.Errorf("dock.Width=%d but the body lays out to %d: the composer will be truncated",
			m.dock.Width, m.contentWidth())
	}
}

// ctrl+g must START the caret blink, not just move focus.
//
// The route returns nil, so the blink starter captured by dock.Focus() had to be
// drained centrally (App.Update). Without that the loop never ran and the caret
// sat solid — the operator's "it only blinks if I type something then backspace
// it to nothing".
func TestCtrlGStartsTheCaretBlink(t *testing.T) {
	m := panelApp(t)
	m.setFocus(focusContent)
	// Update, not dispatch: the drain lives at the single entry point the
	// runtime drives, which is what must hand the command out.
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	nm, ok := next.(*App)
	if !ok {
		t.Fatalf("Update returned %T, want *App", next)
	}
	if nm.chatFocus != focusComposer {
		t.Fatal("ctrl+g must focus the composer")
	}
	if cmd == nil {
		t.Fatal("ctrl+g returned no command — the caret blink loop can never start")
	}
}

// On the launch page the composer is CENTERED, so it is not in the dock rows and
// a click used to fall through to "focus the content" — the composer looked
// focused but typing did nothing (the operator's "it is almost like there is a
// blocker there"). There is nothing else to click on that page.
func TestLaunchPageClickFocusesTheComposer(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask"})
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	if !m.welcomeMode() {
		t.Fatal("precondition: the Ask launch page")
	}
	m.setFocus(focusContent)

	nm, cmd := m.dispatch(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 40, Y: m.height / 2, // the centred composer
	})
	m = nm
	if m.chatFocus != focusComposer {
		t.Fatal("a click on the launch page must focus the composer so typing works")
	}
	if m.panelVisible() {
		t.Fatal("and it must not slide a conversation out")
	}
	if cmd == nil {
		t.Error("the click should also start the caret blink")
	}
}
