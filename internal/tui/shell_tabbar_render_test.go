package tui

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
	tea "github.com/charmbracelet/bubbletea"
)

// TestShellFirstLineRendersTabBar pins the app-shell layout contract: the
// first visible line of the shell view is the six-area nav tab bar (Ask
// Orchicon, Work, Execution, Automation, Enforcement, Control), mirroring
// the GUI nav order. Regression for the QA pass on the TUI foundation
// (caught against a real PTY render of the shell).
type tabBarScreenStub struct{ body string }

func (f *tabBarScreenStub) Init() tea.Cmd                    { return nil }
func (f *tabBarScreenStub) Update(tea.Msg) (Screen, tea.Cmd) { return f, nil }
func (f *tabBarScreenStub) View() string                     { return f.body }
func (f *tabBarScreenStub) SetSize(int, int)                 {}
func (f *tabBarScreenStub) Close()                           {}
func (f *tabBarScreenStub) Name() string                     { return "stub" }

func TestShellFirstLineRendersTabBar(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "SCREENBODY"})
	app.dispatch(tea.WindowSizeMsg{Width: 140, Height: 44})
	app.SwitchTo(TabAsk)
	v := app.View()
	lines := strings.Split(v, "\n")
	if len(lines) < 3 {
		t.Fatalf("shell view has only %d lines", len(lines))
	}
	t.Logf("LINE1=%q", lines[0])
	if !strings.Contains(lines[0], "Ask Orchicon") ||
		!strings.Contains(lines[0], "Work") ||
		!strings.Contains(lines[0], "Execution") ||
		!strings.Contains(lines[0], "Automation") ||
		!strings.Contains(lines[0], "Enforcement") ||
		!strings.Contains(lines[0], "Control") {
		t.Errorf("tab bar (all six areas) missing from first shell line: %q", lines[0])
	}
}

// TestQuitRouteIssuesQuitCmd pins the quit contract: the global quit route
// (q / ctrl+c) must return a tea.Quit command from dispatch, not merely
// blank the view. QA on the TUI foundation found q/ctrl+c hanging the TUI
// because the route only set the quitting flag.
func TestQuitRouteIssuesQuitCmd(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})

	for name, key := range map[string]tea.KeyMsg{
		"q":      {Type: tea.KeyRunes, Runes: []rune{'q'}},
		"ctrl+c": {Type: tea.KeyCtrlC},
	} {
		got, cmd := app.dispatch(key)
		if !got.quitting {
			t.Errorf("%s: quitting flag not set", name)
		}
		if cmd == nil {
			t.Fatalf("%s: dispatch returned nil cmd — TUI would hang on quit", name)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: cmd did not produce tea.QuitMsg", name)
		}
	}
}

// TestNonQuitRoutesDoNotQuit guards the flip side: an unrelated global
// route must not terminate the program.
func TestNonQuitRoutesDoNotQuit(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabWork, &tabBarScreenStub{body: "b"})
	_, cmd := app.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Errorf("'r' (reconnect) must not issue tea.Quit")
		}
	}
	if app.quitting {
		t.Errorf("'r' must not set the quitting flag")
	}
}
