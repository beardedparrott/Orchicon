package tui

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/config"
	tea "github.com/charmbracelet/bubbletea"
)

// TestShellFirstLineRendersTabBar pins the app-shell layout contract: the
// first visible line of the shell view is the seven-area nav tab bar (Ask
// Orchicon, Overview, Work, Execution, Automation, Enforcement, Control),
// mirroring the GUI nav order. Regression for the QA pass on the TUI
// foundation (caught against a real PTY render of the shell).
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
		!strings.Contains(lines[0], "Overview") ||
		!strings.Contains(lines[0], "Work") ||
		!strings.Contains(lines[0], "Execution") ||
		!strings.Contains(lines[0], "Automation") ||
		!strings.Contains(lines[0], "Enforcement") ||
		!strings.Contains(lines[0], "Control") {
		t.Errorf("tab bar (all six areas) missing from first shell line: %q", lines[0])
	}
}

// TestQuitRouteIssuesQuitCmd pins the quit contract: the global quit route (ctrl+c) must return a tea.Quit
// command from dispatch, not merely blank the view. QA on the TUI foundation found q/ctrl+c hanging the TUI
// because the route only set the quitting flag.
//
// `q` IS NO LONGER PART OF IT — the operator: "if you hit it outside of the composer then it still quits.
// We should remove that completely." The route is ctrl+c alone; TestQDoesNotQuitFromAnyFocus asserts the
// other half of that, from every focus level.
func TestQuitRouteIssuesQuitCmd(t *testing.T) {
	app := NewApp(nil, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "b"})
	app.setFocus(focusContent)

	got, cmd := app.dispatch(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !got.quitting {
		t.Error("ctrl+c: quitting flag not set")
	}
	if cmd == nil {
		t.Fatal("ctrl+c: dispatch returned nil cmd — TUI would hang on quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c: cmd did not produce tea.QuitMsg")
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
