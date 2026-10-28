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

// --- a profile written on another platform ----------------------------------------------------------

// TestADeadChordIsRefusedPerPlatform pins the cross-platform case, which defaulting alone does NOT cover.
//
// A profile saved on Linux carries `newline = "alt+enter"`. Opened on Windows that setting would activate a chord
// Windows Terminal has already claimed (toggleFullscreen), leaving the operator with NO working newline chord and
// a hint naming a key that resizes their window. The shell therefore asks ChordReachesOrch before assigning it.
func TestADeadChordIsRefusedPerPlatform(t *testing.T) {
	cases := []struct {
		mode NewlineMode
		goos string
		want bool
		why  string
	}{
		// Windows: only the terminal-independent chords get through.
		{NewlineAltEnter, "windows", false, "alt+enter is Windows Terminal's toggleFullscreen"},
		{NewlineCtrlJ, "windows", true, "ctrl+j is delivered on both input paths and unbound"},
		{NewlineBackslashEnter, "windows", true, "its Shift produces a rune, which IS decoded normally"},
		{NewlineBoth, "windows", true, "its first chord is ctrl+j, so a working chord remains"},
		// Every chord is delivered elsewhere, so nothing may be refused there — refusing would fight a config
		// the operator can see working on that platform.
		{NewlineAltEnter, "linux", true, "delivered normally"},
		{NewlineAltEnter, "darwin", true, "delivered normally"},
		{NewlineCtrlJ, "linux", true, "delivered normally"},
		{NewlineBackslashEnter, "darwin", true, "delivered normally"},
	}
	for _, c := range cases {
		if got := chordReachesOrchFor(c.mode, c.goos); got != c.want {
			t.Errorf("chordReachesOrchFor(%q, %q) = %v, want %v (%s)", c.mode, c.goos, got, c.want, c.why)
		}
	}
}

// TestOnlyAltEnterIsLostOnWindows is the precise claim, so the refusal above cannot be over-broad by accident:
// exactly one mode is undeliverable there, and naming it keeps the reason honest.
func TestOnlyAltEnterIsLostOnWindows(t *testing.T) {
	for _, mode := range []NewlineMode{NewlineCtrlJ, NewlineBackslashEnter, NewlineBoth} {
		if !chordReachesOrchFor(mode, "windows") {
			t.Errorf("%q was refused on Windows — over-broad refusals fight configs that DO work there", mode)
		}
	}
	if chordReachesOrchFor(NewlineAltEnter, "windows") {
		t.Error("alt+enter was accepted on Windows, but the terminal consumes it")
	}
}

// --- HOW WINDOWS ACTUALLY DELIVERS IT --------------------------------------------------------------

// windowsCtrlJ is the Ctrl+J key message the WINDOWS CONSOLE reader produces, and it is not KeyCtrlJ.
//
// The console reports the CHARACTER a chord produced, not a key code: Ctrl+J produces LF (0x0A), so the message
// arrives as KeyRunes("\n"). The ANSI byte reader — Linux, macOS — decodes the same character to KeyCtrlJ
// instead. Same keystroke, two message shapes, decided by the input path.
func windowsCtrlJ() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("\n")}
}

// TestCtrlJAsWindowsDeliversIt is the test the earlier change was missing, and the operator found the gap the
// only way it can be found: by pressing the key.
//
// "I did try ctrl+J already and it still submitted enter instead of a newline."
//
// The previous test built KeyMsg{Type: KeyCtrlJ} — the ANSI path's shape — so it proved the BRANCH works rather
// than that the chord is recognised on the platform it was written for. A key test that constructs the message it
// expects can only ever agree with itself.
func TestCtrlJAsWindowsDeliversIt(t *testing.T) {
	m := New()
	m.Focus()
	m.Newlines = NewlineCtrlJ
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})

	// The Windows shape.
	m.Update(windowsCtrlJ())

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})

	if got := m.Value(); got != "one\ntwo" {
		t.Fatalf("value = %q, want \"one\\ntwo\" — on Windows ctrl+j arrives as a RUNE (LF), not as KeyCtrlJ, "+
			"so a branch that only matches KeyCtrlJ does nothing for the operator", got)
	}
	if s := m.SendRequest(); s != "" {
		t.Fatalf("ctrl+j (Windows shape) SENT %q — this is the operator's exact report", s)
	}
}

// TestBothDeliveryShapesWork keeps the ANSI platforms working, so the fix cannot be a swap.
func TestBothDeliveryShapesWork(t *testing.T) {
	for name, key := range map[string]tea.KeyMsg{
		"ansi (KeyCtrlJ)":    {Type: tea.KeyCtrlJ},
		"windows (runes LF)": windowsCtrlJ(),
	} {
		t.Run(name, func(t *testing.T) {
			m := New()
			m.Focus()
			m.Newlines = NewlineCtrlJ
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
			m.Update(key)
			if m.Value() != "a\n" {
				t.Errorf("value = %q, want a newline inserted", m.Value())
			}
			if s := m.SendRequest(); s != "" {
				t.Errorf("the chord sent %q instead of newlineing", s)
			}
		})
	}
}

// TestTheRuneArmCannotSwallowASend is the safety property, and the reason the rune match is exactly one LF:
// a plain Enter must still send, in every newline mode, however it is spelled.
func TestTheRuneArmCannotSwallowASend(t *testing.T) {
	for _, mode := range []NewlineMode{NewlineCtrlJ, NewlineAltEnter, NewlineBoth, NewlineBackslashEnter} {
		m := New()
		m.Focus()
		m.Newlines = mode
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("send me")})
		// Enter as the console delivers it (KeyEnter), which must remain a SEND.
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if got := m.SendRequest(); got != "send me" {
			t.Errorf("mode %q: Enter did not send (got %q)", mode, got)
		}
	}
	// And a multi-rune "paste" is not a chord: it must not be mistaken for one.
	m := New()
	m.Focus()
	m.Newlines = NewlineCtrlJ
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a\nb")})
	if s := m.SendRequest(); s != "" {
		t.Errorf("a multi-rune message was treated as a chord and sent %q", s)
	}
}
