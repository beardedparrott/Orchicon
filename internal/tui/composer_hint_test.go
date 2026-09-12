package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// hintScreen is a screen that publishes a shortcut list (HintLine) — the shape
// the shell reads to build the composer's affordance row.
func (s *hintScreen) HintLine() string { return s.hint }

type hintScreen struct {
	stubScreen
	hint string
}

// The operator: "The shortcut guidance should be context driven ... Depending
// on what page you are on, the text below in the text box should show a list of
// all shortcut commands depending on what page you are on. Also it is NOT
// showing the ctrl+g shortcut advice that should be on every page."
func TestComposerHintFollowsTheActiveScreen(t *testing.T) {
	m := newTestApp()
	m.RegisterScreen(TabWork, &hintScreen{stubScreen: stubScreen{id: "work"}, hint: "n new · e edit · O collapse all"})
	m.RegisterScreen(TabControl, &hintScreen{stubScreen: stubScreen{id: "control"}, hint: "e edit · x delete"})
	m.dispatch(tea.WindowSizeMsg{Width: 200, Height: 40})

	m.SwitchTo(TabWork)
	h := m.dock.Hint()
	if !strings.Contains(h, "O collapse all") {
		t.Fatalf("the work page's shortcuts must appear in the composer hint: %q", h)
	}
	if !strings.Contains(h, "ctrl+g") {
		t.Fatalf("ctrl+g must be present on every page: %q", h)
	}
	if !strings.HasPrefix(h, "ctrl+g") {
		t.Fatalf("ctrl+g must LEAD so a narrow pane truncates the page keys, not the focus chord: %q", h)
	}

	// Switching pages swaps the shortcut list.
	m.SwitchTo(TabControl)
	h = m.dock.Hint()
	if !strings.Contains(h, "e edit · x delete") {
		t.Fatalf("the control page's shortcuts must replace the work ones: %q", h)
	}
	if strings.Contains(h, "O collapse all") {
		t.Fatalf("the previous page's shortcuts must not persist: %q", h)
	}
}
