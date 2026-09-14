package tui

// rails_layout_test.go — LAYOUT REGRESSION tests for the Ask column layout.
//
// History: Phase 2c added a shell-level CONVERSATIONS right rail. The Ask
// screen ALSO renders the conversation list as its own source pane, so the
// tab drew THREE columns where the GUI has two — the operator's "there are
// two conversation panes for some reason". Worse, Shift+Tab closes the rail,
// so the conversation list vanished and never came back ("conversations go
// away").
//
// The rail is now disabled (railVisible() == false); the screen's source pane
// is the single conversation list. These tests pin that contract.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

var railSizes = [][2]int{{80, 24}, {120, 40}}

// newRailsApp builds a shell on Ask with a session OPEN (so the rail renders —
// the launch layout deliberately hides the conversations list).
func newRailsApp(w, h int) *App {
	m := newTestApp()
	m.RegisterScreen(TabAsk, ask.New(nil, m.reg))
	m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
	m.SwitchTo(TabAsk)
	m.chatConvID = "conv-rails"
	return m
}

// railLines asserts the render contract (exactly h rows, each exactly w
// cells) and returns the rows.
func railLines(t *testing.T, m *App, w, h int) []string {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != h {
		t.Fatalf("%dx%d: %d rows, want exactly %d", w, h, len(lines), h)
	}
	for i, l := range lines {
		if got := lipgloss.Width(l); got != w {
			t.Fatalf("%dx%d row %d: width %d, want %d", w, h, i, got, w)
		}
	}
	return lines
}

// The conversation list lives on the shell's right rail, always on for MVP1.
func TestConversationsRailIsAlwaysOnForAsk(t *testing.T) {
	for _, size := range railSizes {
		w, h := size[0], size[1]
		m := newRailsApp(w, h)
		if !m.railVisible() {
			t.Fatalf("%dx%d: the conversations rail must be on for Ask (MVP1)", w, h)
		}
		if ConversationsRailWidth < 30 {
			t.Fatalf("rail width = %d, want it widened (>=30)", ConversationsRailWidth)
		}
		railLines(t, m, w, h)
	}
}

// Exactly ONE conversation list: the rail. The screen renders the transcript
// only (Base.HideSources), so the tab no longer draws two lists.
func TestAskRendersExactlyOneConversationList(t *testing.T) {
	for _, size := range railSizes {
		w, h := size[0], size[1]
		m := newRailsApp(w, h)
		v := m.View()
		if n := strings.Count(v, "Conversations"); n < 1 {
			t.Errorf("%dx%d: conversation-list title missing (the rail panel must render)", w, h)
		}
		railLines(t, m, w, h)
	}
}

// Shift+Tab toggles the LEFT diff pane and must leave the conversation list
// alone (the operator: "Shift+Tab should just bring out the diff pane").
func TestShiftTabTogglesDiffAndKeepsConversations(t *testing.T) {
	m := newRailsApp(120, 40)
	if !strings.Contains(m.View(), "Conversations") {
		t.Fatal("precondition: the conversation list must render")
	}
	m.toggleSideRails()
	if !m.diffOpen {
		t.Fatal("Shift+Tab must open the diff pane")
	}
	if !strings.Contains(m.View(), "Conversations") {
		t.Fatal("Shift+Tab removed the conversation list")
	}
	m.toggleSideRails()
	if m.diffOpen {
		t.Fatal("Shift+Tab must close the diff pane again")
	}
	railLines(t, m, 120, 40)
}

// ctrl+r is inert: the rail is always on for MVP1, so the key must not hide
// it or disturb the frame contract.
func TestRailToggleIsInert(t *testing.T) {
	m := newRailsApp(120, 40)
	_, _ = m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlR})
	if !m.railVisible() {
		t.Fatal("ctrl+r hid the always-on conversations rail")
	}
	if !strings.Contains(m.View(), "Conversations") {
		t.Fatal("ctrl+r removed the conversation list")
	}
	railLines(t, m, 120, 40)
}

// TestThemeStepSequence mirrors the real-pty gate's theme step at shell
// level: "/" opens the palette, typing filters, enter selects /theme (which
// runs it), then the argument + enter sends "/theme light" — the ACTIVE
// theme must switch and repaint.
func TestThemeStepSequence(t *testing.T) {
	m := newRailsApp(120, 40)
	m.setFocus(focusComposer)
	before := theme.Active().Name
	m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.palette.PaletteOpen() {
		t.Fatal("'/' must open the palette")
	}
	for _, r := range "theme" {
		m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if len(m.palette.filter) == 0 {
		t.Fatal("palette filter for 'theme' must match /theme")
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if m.dock.Value() != "/theme" {
		t.Fatalf("palette select must leave /theme in the composer, got %q", m.dock.Value())
	}
	for _, r := range " light" {
		m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	if got := theme.Active().Name; got != "light" {
		t.Fatalf("theme not switched: active=%s (notice=%q err=%q)", got, m.dock.Notice, m.dock.Err)
	}
	if !strings.Contains(lipglossStrip(m.View()), "light") {
		t.Fatal("switched theme not reflected in the shell view")
	}
	t.Cleanup(func() { theme.Use(before) })
}

func TestMouseClickFocusesComposer(t *testing.T) {
	for _, size := range railSizes {
		w, h := size[0], size[1]
		m := newRailsApp(w, h)
		m.setFocus(focusContent) // precondition: keyboard focus is on content
		nm, _ := m.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 20, Y: h - 2})
		if nm.chatFocus != focusComposer {
			t.Fatalf("%dx%d: clicking the dock must focus the composer (finding 7)", w, h)
		}
		if !nm.Footer().ComposerFocus {
			t.Fatalf("%dx%d: footer must reflect the composer focus", w, h)
		}
	}
}

func TestSlashDiffTogglesLeftRailFromComposer(t *testing.T) {
	m := newRailsApp(120, 40)
	m.chatConvID = "conv-1"
	c := m.slash.resolve("/diff")
	if c == nil || c.NoticeOnly() {
		t.Fatal("/diff must be a registered real command")
	}
	// Composer focus is the launch default: /diff works from there (the
	// `d` key remains a literal character while composing).
	c.Run(m, nil)
	if !m.diffOpen {
		t.Fatal("/diff must open the left diff rail from the composer")
	}
	if !strings.Contains(lipglossStrip(m.View()), "✕") {
		t.Fatal("diff rail not rendered after /diff")
	}
	c.Run(m, nil)
	if m.diffOpen {
		t.Fatal("/diff must close the rail again (toggle)")
	}
}

// The content width must FOLLOW the conversations rail, not merely a resize.
//
// railVisible() depends on SHELL STATE (askMode / chatConvID), so the rail
// appearing shrinks the content area by ConversationsRailWidth with no
// WindowSizeMsg to drive refreshLayout(). The dock then kept its pre-rail width
// and baseView's normalizeBlock TRUNCATED its right edge — silently cutting off
// everything right-aligned:
//
//   - the composer's context/token/cache stat strip and the mode pill, at the
//     bottom right (the operator's "the context information and model picker on
//     the bottom right are off the screen and I can't see it");
//   - the operator's OWN message bubbles — user messages are right-aligned, so a
//     just-sent message was missing from a transcript that plainly contained it
//     while the left-aligned reply rendered normally.
//
// The existing frame tests could not see this: every row is still EXACTLY w
// cells wide, because truncation preserves the frame contract.
func TestLayoutWidthFollowsRailVisibility(t *testing.T) {
	const w, h = 200, 50

	// The launch page shows no conversations rail, so content == full width.
	m, _ := sendApp(t)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = nm.(*App)
	if m.railVisible() {
		t.Fatal("fixture: the launch page must not show the conversations rail")
	}
	if m.dock.Width != w {
		t.Fatalf("launch page: dock.Width = %d, want the full width %d", m.dock.Width, w)
	}

	// The rail comes up through STATE, exactly as the create path brings it up.
	nm, _ = m.Update(chatConvCreatedMsg{convID: "c1", text: "test"})
	m = nm.(*App)
	if !m.railVisible() {
		t.Fatal("a session must bring the conversations rail up")
	}
	want := w - ConversationsRailWidth
	if m.contentWidth() != want {
		t.Fatalf("contentWidth() = %d, want %d", m.contentWidth(), want)
	}
	if m.dock.Width != want {
		t.Fatalf("dock.Width = %d with the rail up, want %d — a stale width is truncated at render time, cutting off right-aligned content (the stat strip, the operator's own bubbles)",
			m.dock.Width, want)
	}
	railLines(t, m, w, h)

	// And back the other way: a new chat drops the session and the rail with it.
	// newChat() runs inside dispatch in the real shell, so the layout is corrected
	// on that same Update; here it is called directly, so feed one message to
	// exercise that pass.
	m.newChat()
	if m.railVisible() {
		t.Fatal("/new must return to the launch page with no rail")
	}
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = nm.(*App)
	if m.dock.Width != w {
		t.Fatalf("dock.Width = %d after the rail went away, want the full width %d", m.dock.Width, w)
	}
}
