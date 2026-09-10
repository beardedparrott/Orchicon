package tui

// rails_layout_test.go — Phase 2c (operator finding 9) LAYOUT REGRESSION
// tests for the two Ask rails at the 80×24 and 120×40 floors:
//
//   - the CONVERSATIONS rail is a PROPER right-side rail, open by default,
//     populated from the live API (never a floating box);
//   - an auth/API failure renders an explicit retry state, never a silent
//     empty rail;
//   - the diff pane is a PROPER left-side rail (toggle d / /diff) and both
//     rails FLANK the chat column with the widths adding up exactly.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

var railSizes = [][2]int{{80, 24}, {120, 40}}

// newRailsApp builds a shell on the Ask tab at the given size.
func newRailsApp(w, h int) *App {
	m := newTestApp()
	m.RegisterScreen(TabAsk, &tabBarScreenStub{body: "SRC"})
	m.dispatch(tea.WindowSizeMsg{Width: w, Height: h})
	m.SwitchTo(TabAsk)
	return m
}

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

// rightBand returns the last ConversationsRailWidth cells of a row.
func rightBand(row string, w int) string {
	r := []rune(row)
	if len(r) < ConversationsRailWidth {
		return row
	}
	return string(r[len(r)-ConversationsRailWidth:])
}

func TestConversationsRailIsAProperRightRail(t *testing.T) {
	for _, size := range railSizes {
		w, h := size[0], size[1]
		m := newRailsApp(w, h)
		m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{
			{ID: "c1", Title: "rail-alpha", MessageN: 3},
			{ID: "c2", Title: "rail-beta", MessageN: 7, TurnInFly: true},
		}})
		lines := railLines(t, m, w, h)

		header := lipglossStrip(lines[railTopRow])
		if !strings.Contains(header, "CONVERSATIONS") {
			t.Fatalf("%dx%d: rail header missing: %q", w, h, header)
		}
		if band := rightBand(header, w); !strings.Contains(band, "CONVERSATIONS") {
			t.Fatalf("%dx%d: rail is not on the RIGHT edge: %q", w, h, band)
		}
		// The rail's leftmost cell is the divider column (a docked rail, not
		// a floating box).
		if !strings.Contains(rightBand(header, w), railDividerText) {
			t.Fatalf("%dx%d: rail has no divider column: %q", w, h, rightBand(header, w))
		}
		body := lipglossStrip(lines[railTopRow+1])
		if !strings.Contains(body, "rail-alpha") {
			t.Fatalf("%dx%d: rail row not populated from the list: %q", w, h, body)
		}
		if strings.Contains(lipglossStrip(m.View()), "none yet") {
			t.Fatalf("%dx%d: populated rail must not render the empty state", w, h)
		}
	}
}

func TestConversationsRailAuthFailureShowsRetry(t *testing.T) {
	for _, size := range railSizes {
		w, h := size[0], size[1]
		m := newRailsApp(w, h)
		m.onConversations(chat.ConversationsMsg{Err: "rpc error: code = Unauthenticated desc = unauthenticated"})
		v := lipglossStrip(m.View())
		for _, want := range []string{"conversations unavailable", "click here to retry"} {
			if !strings.Contains(v, want) {
				t.Fatalf("%dx%d: failed rail missing %q\n%s", w, h, want, v)
			}
		}
		if strings.Contains(v, "none yet") {
			t.Fatalf("%dx%d: a failed load must never render as an empty rail", w, h)
		}
		// The dock error names the in-place fix (never exit and re-run).
		if !strings.Contains(v, "/connect") {
			t.Fatalf("%dx%d: auth failure must name the in-place re-auth path\n%s", w, h, v)
		}

		// Clicking the failed rail retries against the live API.
		nm, cmd := m.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
			X: w - ConversationsRailWidth + 5, Y: railTopRow + 2})
		if cmd == nil {
			t.Fatalf("%dx%d: clicking the failed rail must issue a retry cmd", w, h)
		}
		if !nm.convLoading {
			t.Fatalf("%dx%d: retry must mark the rail loading", w, h)
		}
	}
}

func TestBothRailsFlankTheChatColumn(t *testing.T) {
	m := newRailsApp(120, 40)
	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "c1", Title: "rail-alpha", MessageN: 2}}})
	m.chatConvID = "conv-1" // gives the diff rail an ask-conversation owner
	m.setFocus(focusContent)
	// Toggle D (the epic's locked key; ctrl+d is never bound).
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'D'}})
	m = nm
	if !m.diffOpen {
		t.Fatal("D must open the left diff rail when an owner exists")
	}
	if got, want := m.contentWidth(), 120-DiffPaneWidth-ConversationsRailWidth; got != want {
		t.Fatalf("content column = %d, want %d (both rails flanking)", got, want)
	}
	lines := railLines(t, m, 120, 40)
	strip := lipglossStrip(m.View())
	if !strings.Contains(strip, "✕") {
		t.Fatal("diff rail (left) missing from the shell view")
	}
	if band := rightBand(lipglossStrip(lines[railTopRow]), 120); !strings.Contains(band, "CONVERSATIONS") {
		t.Fatalf("conversations rail (right) missing: %q", band)
	}
	// The diff rail's content sits in the LEFT band of the body rows.
	left := string([]rune(lipglossStrip(lines[railTopRow+1]))[:DiffPaneWidth])
	if strings.TrimSpace(left) == "" {
		t.Fatalf("left band empty — diff rail not rendering in the left columns")
	}
}

// railAskStub records the detail the shell asks the Ask screen to open.
type railAskStub struct {
	stubScreen
	detailID string
	reqSrc   string
	reqID    string
}

func (s *railAskStub) RequestDetail(src, id string) tea.Cmd {
	s.reqSrc, s.reqID = src, id
	return nil
}
func (s *railAskStub) DetailID() string { return s.detailID }

// TestRailRowClickOpensConversationDetail pins operator finding 9's click
// contract: a rail row click must open the conversation's DETAIL (its
// transcript surface) as well as the chat target — otherwise the click
// changed invisible state and looked like a dead click (finding 7).
func TestRailRowClickOpensConversationDetail(t *testing.T) {
	m := newTestApp()
	ask := &railAskStub{}
	m.RegisterScreen(TabAsk, ask)
	m.dispatch(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.SwitchTo(TabAsk)
	m.onConversations(chat.ConversationsMsg{Convs: []chat.Conversation{{ID: "conv-7", Title: "rail-seven", MessageN: 4}}})
	_, cmd := m.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: 120 - ConversationsRailWidth + 5, Y: railTopRow + 1})
	if cmd == nil {
		t.Fatal("a rail row click must issue a command")
	}
	if ask.reqSrc != "conversations" || ask.reqID != "conv-7" {
		t.Fatalf("rail row click must open the conversation detail, got %q/%q", ask.reqSrc, ask.reqID)
	}
	if m.chatConvID != "conv-7" {
		t.Fatalf("rail row click must pin the chat target, got %q", m.chatConvID)
	}
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

func TestWheelScrollsConversationsRail(t *testing.T) {
	m := newRailsApp(120, 40)
	convs := make([]chat.Conversation, 0, 60)
	for i := 0; i < 60; i++ {
		convs = append(convs, chat.Conversation{ID: "c", Title: "rail-conv", MessageN: 1})
	}
	m.onConversations(chat.ConversationsMsg{Convs: convs})
	nm, _ := m.dispatch(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown,
		X: 120 - 5, Y: 10})
	if nm.convScroll == 0 {
		t.Fatal("wheel over the rail must scroll the conversation list")
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
