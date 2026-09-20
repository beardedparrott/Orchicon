package tui

// screen_shell_freshness_test.go — A SCREEN'S SHELL REFERENCE MUST POINT AT THE LIVE APP.
//
// `App.Update` has a VALUE receiver, so bubbletea copies the App for every message and the model it keeps is a
// NEW address each time. A screen constructed during one of those Updates is handed a pointer to THAT copy —
// so from the next message onward it talks to an App that no longer exists.
//
// The consequence the operator hit: switching a theme from the Control→Themes PANE called `App.SetTheme` on the
// stale copy. `theme.Use` is global, so the PALETTE changed correctly — which is why the switch looked
// half-working — while `m.dock.ApplyTheme()` re-pinned the DEAD copy's composer, leaving the live one on the
// palette it was built with. Their words: "I switched from a light theme to a dark theme and now the composer
// is a white box. When switching between light and dark themes, the composer never switches over properly."
//
// `/theme` never showed this because it runs on the live App.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// themesPaneApp lands on the Control tab with the Themes pane focused and loaded, and returns the App that
// SURVIVED those messages — including one extra message after the screen was built, so the reference it was
// handed at construction is already a dead copy. That is the state the operator is always in.
func themesPaneApp(t *testing.T) *App {
	t.Helper()
	m := phase3App(120, 40)
	if !m.SetTheme("catppuccin-latte") {
		t.Fatal("SetTheme(catppuccin-latte) failed")
	}
	// LAND ON THE CONTROL TAB FIRST, which is what builds the screen and hands it a shell reference.
	m.SwitchTo(TabControl)
	m = runApp(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	// ONE MORE MESSAGE, so the reference the screen was handed at construction is already a dead copy.
	m = runApp(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.active != TabControl {
		t.Fatalf("fixture: the active tab is %v, want Control", m.active)
	}

	scr := m.screens[TabControl]
	if scr == nil {
		t.Fatal("fixture: no Control screen")
	}
	type sourceSelector interface{ SelectSource(string) bool }
	type itemLoader interface {
		LoadItems(string, []kit2.Item, string) bool
	}
	ss, ok := scr.(sourceSelector)
	if !ok {
		t.Fatal("fixture: the Control screen cannot select a source")
	}
	if !ss.SelectSource("themes") {
		t.Fatal("fixture: could not focus the Themes pane")
	}
	ld, ok := scr.(itemLoader)
	if !ok {
		t.Fatal("fixture: the Control screen cannot load items")
	}
	// The rows the pane would have fetched, in its own shape (a section heading carries an EMPTY id).
	if !ld.LoadItems("themes", []kit2.Item{
		{ID: "", Title: "DARK", Meta: ""},
		{ID: "tokyo-night", Title: "tokyo-night", Meta: "available"},
		{ID: "", Title: "LIGHT", Meta: ""},
		{ID: "catppuccin-latte", Title: "catppuccin-latte", Meta: "available"},
	}, "") {
		t.Fatal("fixture: could not load the themes rows")
	}
	// The LIST owns the arrows when the content pane is focused.
	m.setFocus(focusContent)
	return m
}

// runApp feeds one message through the live App and returns the surviving one.
func runApp(t *testing.T, m *App, msg tea.Msg) *App {
	t.Helper()
	nm, _ := m.Update(msg)
	next, ok := nm.(*App)
	if !ok || next == nil {
		t.Fatalf("Update returned %T, want *App", nm)
	}
	return next
}

// THE OPERATOR'S SEQUENCE, through the pane they were in.
func TestSwitchingAThemeFromTheThemesPaneReachesTheComposer(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	m := themesPaneApp(t)

	lightFill := paintSeq(theme.Lookup("catppuccin-latte").Surface)
	darkFill := paintSeq(theme.Lookup("tokyo-night").Surface)
	if lightFill == "" || darkFill == "" || lightFill == darkFill {
		t.Fatalf("fixture: the two fills do not differ (%q / %q)", lightFill, darkFill)
	}

	// ONE DOWN ARROW: the cursor leaves the DARK heading and lands on tokyo-night, which applies it. This is
	// the operator's gesture — no Enter anywhere.
	m = runApp(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if got := theme.Active().Name; got != "tokyo-night" {
		t.Fatalf("fixture: arrowing in the Themes pane did not switch the palette (active %q)", got)
	}

	m.dock.SetValue("hello")
	m.refreshLayout()
	rows := strings.Join(composerLines(t, m), "\n")
	if !strings.Contains(rows, darkFill) {
		t.Errorf("after switching to a darker palette FROM THE THEMES PANE the composer is not painted with "+
			"that palette's fill %q — this is the operator's \"I switched from a light theme to a dark theme "+
			"and now the composer is a white box\"", darkFill)
	}
	if strings.Contains(rows, lightFill) {
		t.Errorf("the composer still carries the LIGHT palette's fill %q after switching to a dark one",
			lightFill)
	}
	// AND THE GENERAL FORM, which is the assertion that actually catches this: the check above can pass while
	// the composer's own TEXT CELLS hold a third palette's fill, because the box's chrome follows the new
	// palette through package state while a stale textarea keeps whatever it was built with.
	assertComposerPaintsOnlyTheActivePalette(t, m, "after switching from the Themes pane")
}

// THE INVARIANT, asserted directly: every screen's shell reference IS the App that survived the last message.
//
// The behavioural test above can only cover what a screen happens to push through the reference. This one
// covers the REFERENCE ITSELF, so it fails for any screen that goes stale — including a future screen that
// needs its shell for something other than theming (a notice, a mutation sink, a refresh).
func TestEveryScreensShellReferenceIsTheLiveApp(t *testing.T) {
	m := phase3App(120, 40)
	for _, tab := range []TabID{TabAsk, TabWork, TabExecution, TabControl} {
		m.SwitchTo(tab)
		// The App returned by THIS Update is the live one, and every screen must be pointing at it afterwards.
		live := runApp(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
		m = live
		for id, s := range live.screens {
			sh, ok := s.(interface{ Shell() any })
			if !ok {
				continue
			}
			if got := sh.Shell(); got != any(live) {
				t.Errorf("the %s screen's shell reference is a STALE App, not the live one — anything it "+
					"pushes through that reference (a theme, a notice, a mutation) lands on a dead copy", id)
			}
		}
	}
}
