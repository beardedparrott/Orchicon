package tui

// palette_dismiss_test.go — A DISPATCHED SLASH COMMAND DISMISSES THE CHOOSER.
//
// The operator: "in the TUI, some slash commands leaves a box up after I initiate the command and I have
// to hit Esc to get rid of it."
//
// REPRODUCED. The palette's `enter` case closed itself ONLY when it had a selected candidate
// (palette.go: `c := m.paletteSelected(); if c == nil { return false, nil }`). The filter is computed
// from the WHOLE composer line, so typing a command WITH AN ARGUMENT matches nothing — "find foo" is not
// a command name — and the candidate list empties. Enter then fell through to the composer, the dock
// sent the line, dispatchSlash RAN the command, and the palette was never closed:
//
//	composer:  /find foo
//	filter:    [] (0 candidates — the argument is not part of any name)
//	enter:     c == nil -> fall through -> command runs -> BOX STAYS UP over a sent composer
//
// That is why it was only SOME commands: the argument-taking ones. A bare "/help" matched and closed
// correctly, which is exactly why this survived a suite that only ever drove the palette to a bare
// prefix.
//
// THE FIX IS IN dispatchSlash, NOT IN THE PALETTE'S ENTER ARM, and the difference matters: the palette
// forwards every key it does not case itself to the composer (paletteHandleKey's default arm), and that
// set includes ctrl+j — the dock's SECOND spelling of Enter, the LF some terminals send. That route never
// reaches the `enter` case at all, so fixing the fall-through there would have left the identical box
// behind for an operator whose terminal sends LF. Every route to a dispatched command passes through
// dispatchSlash, so closing it there makes "a command dismisses the chooser" true by construction rather
// than for one key.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// paletteBoxUp reports whether the chooser is open OR still drawn in the frame.
// Both, deliberately: the operator's report is about what is ON SCREEN, and a
// state flag that disagrees with the render is the same bug wearing a different
// shape.
func paletteBoxUp(m *App) bool {
	return m.palette.PaletteOpen() || strings.Contains(m.View(), "command palette")
}

// THE REPORTED CASE: a command WITH AN ARGUMENT runs and the box goes away.
//
// Every one of these empties the candidate list (the argument matches no
// command name), which is precisely the condition that used to leave the box up.
func TestSlashCommandWithAnArgumentDismissesThePalette(t *testing.T) {
	for _, line := range []string{
		"/find foo",
		"/theme forest",
		"/model a/b",
		"/wi 01ABC",
		"/skills /tmp/a",
		"/attach /tmp/a.png",
		"/context pin something",
	} {
		t.Run(line, func(t *testing.T) {
			m := phase3App(120, 40)
			m = typeInto(t, m, line)
			if !m.palette.PaletteOpen() {
				t.Fatalf("fixture: the palette must be open after typing %q", line)
			}
			if len(m.palette.filter) != 0 {
				t.Fatalf("fixture: %q must empty the candidate list for this test to be the reported "+
					"case (got %d candidates)", line, len(m.palette.filter))
			}

			nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
			m = nm

			if paletteBoxUp(m) {
				t.Errorf("the box is still up after %q ran — the operator's \"some slash commands leaves "+
					"a box up after I initiate the command and I have to hit Esc to get rid of it\"\n"+
					"frame:\n%s", line, m.View())
			}
		})
	}
}

// THE OTHER SPELLING OF ENTER. ctrl+j is the LF some terminals send; the palette
// forwards it to the composer, so it never reaches the palette's own `enter`
// case. The fix has to hold for it too, or the box returns for those operators.
func TestCtrlJDismissesThePaletteToo(t *testing.T) {
	m := phase3App(120, 40)
	m = typeInto(t, m, "/find foo")
	if !m.palette.PaletteOpen() {
		t.Fatal("fixture: palette not open")
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m = nm
	if paletteBoxUp(m) {
		t.Errorf("ctrl+j left the box up — the palette forwards it, so dispatchSlash is the only place "+
			"that can close it\nframe:\n%s", m.View())
	}
}

// THE BARE-PREFIX CASE MUST KEEP WORKING (the existing contract): selecting a
// command from the list runs it and closes the box.
func TestSelectingFromThePaletteStillDismisses(t *testing.T) {
	m := phase3App(120, 40)
	m = typeInto(t, m, "/he")
	if sel := m.paletteSelected(); sel == nil || sel.Name != "/help" {
		t.Fatalf("fixture: first match for /he = %v, want /help", sel)
	}
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if paletteBoxUp(m) {
		t.Errorf("selecting /help from the list left the box up\nframe:\n%s", m.View())
	}
	if !strings.Contains(m.dock.Notice, "commands") {
		t.Errorf("the selected command did not run (notice=%q)", m.dock.Notice)
	}
}

// AN UNKNOWN COMMAND STILL DISMISSES AND EXPLAINS. The chooser has served its
// purpose once the operator has committed to a line; a box left floating over
// the error it produced hides the very explanation the operator needs.
func TestUnknownCommandDismissesAndExplains(t *testing.T) {
	m := phase3App(120, 40)
	m = typeInto(t, m, "/nosuchthing")
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm
	if paletteBoxUp(m) {
		t.Errorf("an unknown command left the box up over its own error\nframe:\n%s", m.View())
	}
	if !strings.Contains(m.dock.Err, "unknown command") {
		t.Errorf("the operator was not told the command is unknown (error=%q)", m.dock.Err)
	}
}
