package dock

// newline_windows_test.go — THE CHORD THAT WORKS WHERE THE OBVIOUS TWO DO NOT.
//
// The operator, testing orch on Windows: "shift+enter is still not working, it's sending the message instead.
// alt+enter hides the powershell tabs in the terminal so it's already being captured by something else."
//
// Both reports are correct and NEITHER is ours to fix:
//
//   - SHIFT+ENTER arrives as a bare Enter. Windows Terminal emits no distinct VT sequence for it by default,
//     and the Windows console decoder (bubbletea's key_windows.go) maps VK_RETURN to KeyEnter reading only
//     Alt — Shift is never consulted for Enter.
//   - ALT+ENTER IS TAKEN BY THE TERMINAL. `alt+enter` is Windows Terminal's toggleFullscreen, so the tab bar
//     disappearing IS the key being consumed before orch ever sees it.
//
// Ctrl+J arrives as 0x0A on both input paths and Windows Terminal leaves it unbound. These tests pin it as a
// WORKING chord rather than a setting that only changes the hint — the failure mode this codebase keeps
// producing, and the one an earlier "documented but dead" chord produced.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCtrlJInsertsNewlineOnWindows is the reported case: the chord must actually insert a line, not send.
func TestCtrlJInsertsNewlineOnWindows(t *testing.T) {
	m := New()
	m.Focus()
	m.Newlines = NewlineCtrlJ
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})

	if got := m.Value(); got != "one\ntwo" {
		t.Fatalf("value = %q, want \"one\\ntwo\" — ctrl+j must insert a newline", got)
	}
	if s := m.SendRequest(); s != "" {
		t.Fatalf("ctrl+j SENT the message (%q) instead of inserting a newline — this is precisely the reported "+
			"symptom, one chord over", s)
	}
}

// TestCtrlJSurvivesTheEnterArms is the ordering proof, and the reason this test exists rather than trusting the
// one above: `enterKey` treats ctrl+j as Enter on purpose (so a terminal that reports Enter as LF still sends).
// If the ctrl+j newline branch were placed BELOW the enterKey arms it would never be reached, and the mode would
// change the hint while the key kept sending.
func TestCtrlJSurvivesTheEnterArms(t *testing.T) {
	for _, mode := range []NewlineMode{NewlineCtrlJ, NewlineBoth} {
		m := New()
		m.Focus()
		m.Newlines = mode
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("keep me")})
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})

		if s := m.SendRequest(); s != "" {
			t.Errorf("mode %q: ctrl+j sent %q — the chord is dead, so the setting is a lie", mode, s)
		}
		if !strings.HasSuffix(m.Value(), "\n") {
			t.Errorf("mode %q: ctrl+j did not insert a newline; value = %q", mode, m.Value())
		}
	}
}

// TestPlatformDefaultPicksAChordThatCanWork pins the default per platform, because the right default differs by
// what the HOST allows rather than by preference: naming alt+enter on Windows names a key the terminal owns.
func TestPlatformDefaultPicksAChordThatCanWork(t *testing.T) {
	if got := defaultNewlineModeFor("windows"); got != NewlineCtrlJ {
		t.Errorf("windows default = %q, want ctrl+j — alt+enter is Windows Terminal's toggleFullscreen and "+
			"shift+enter is not decodable, so both of the other chords are dead there", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := defaultNewlineModeFor(goos); got != NewlineAltEnter {
			t.Errorf("%s default = %q, want alt+enter — it is delivered there and needs no change", goos, got)
		}
	}
}

// TestUnknownNewlineSettingFallsBackToThePlatformDefault: a typo must not silently select a dead chord on the
// platform where it cannot work.
func TestUnknownNewlineSettingFallsBackToThePlatformDefault(t *testing.T) {
	for _, bad := range []string{"", "nonsense", "shift+enter", "ctrl+shift+j"} {
		if got, want := ParseNewlineMode(bad), DefaultNewlineMode(); got != want {
			t.Errorf("ParseNewlineMode(%q) = %q, want the platform default %q", bad, got, want)
		}
	}
}

// TestCtrlJIsParsedFromItsSpellings keeps config written by hand working.
func TestCtrlJIsParsedFromItsSpellings(t *testing.T) {
	for _, s := range []string{"ctrl+j", "Ctrl+J", " ctrl+j ", "ctrl-j"} {
		if got := ParseNewlineMode(s); got != NewlineCtrlJ {
			t.Errorf("ParseNewlineMode(%q) = %q, want ctrl+j", s, got)
		}
	}
}

// TestTheHintNamesTheChordActuallyInUse: a composer that advertises a chord the platform has taken is how the
// operator lost several rounds to a dead key. On Windows the hint must say ctrl+j.
func TestTheHintNamesTheChordActuallyInUse(t *testing.T) {
	cases := []struct {
		mode NewlineMode
		want string
		not  string
	}{
		{NewlineCtrlJ, "ctrl+j", ""},
		{NewlineAltEnter, "alt+enter", ""},
		{NewlineBackslashEnter, "\\+enter", ""},
		// "both" names the one that is always deliverable FIRST, so a Windows operator reads a working chord
		// even though alt+enter appears second.
		{NewlineBoth, "ctrl+j", ""},
	}
	for _, c := range cases {
		t.Run(string(c.mode), func(t *testing.T) {
			m := New()
			m.Newlines = c.mode
			hint := m.Hint()
			if !strings.Contains(hint, c.want) {
				t.Errorf("hint for %q does not name %q: %s", c.mode, c.want, hint)
			}
			if c.mode == NewlineCtrlJ && strings.Contains(hint, "alt+enter") {
				t.Errorf("hint still advertises alt+enter for the ctrl+j mode — on Windows that chord is "+
					"consumed by the terminal, so the hint would be advertising a dead key: %s", hint)
			}
		})
	}
}

// TestOtherModesAreUnaffected is the regression guard: adding a chord must not change how the existing ones
// behave on the platforms where they work.
func TestOtherModesAreUnaffected(t *testing.T) {
	// alt+enter still newlines under alt+enter mode…
	m := New()
	m.Focus()
	m.Newlines = NewlineAltEnter
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if s := m.SendRequest(); s != "" || !strings.HasSuffix(m.Value(), "\n") {
		t.Errorf("alt+enter mode: alt+enter did not insert a newline (value=%q sent=%q)", m.Value(), s)
	}
	// …and plain Enter still SENDS in every mode, including ctrl+j, because the contract is "enter send".
	for _, mode := range []NewlineMode{NewlineAltEnter, NewlineCtrlJ, NewlineBoth, NewlineBackslashEnter} {
		m := New()
		m.Focus()
		m.Newlines = mode
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("send me")})
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if got := m.SendRequest(); got != "send me" {
			t.Errorf("mode %q: plain Enter did not send (got %q) — 'enter send' is the documented contract", mode, got)
		}
	}
}
