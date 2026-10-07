package tui

// mouse_leak_test.go — A SPLIT MOUSE REPORT MUST NOT BE TYPED.
//
// The operator, resizing: "the resize seems a bit buggy and adds the following into the conversation box
// when resizing: [<32;27;30M[<32;96;29M[<32;117;29M[<32;113;30M".
//
// Those are SGR mouse reports with the ESC stripped (32 = motion with the left button held, which is what a
// window drag produces). bubbletea emits the lone ESC separately when its sequence timeout fires during a
// burst, so the tail arrives as runes — and the tail has to be dropped HERE, because reassembling it is not
// possible from this side.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestOrphanedMouseReportsAreDropped(t *testing.T) {
	// THE EXACT STRINGS FROM THE REPORT, including the run of four the operator saw concatenated — which is
	// how they arrive: one key message per report, in a burst.
	for _, s := range []string{
		"[<32;27;30M",
		"[<32;96;29M",
		"[<32;117;29M",
		"[<32;113;30M",
		"[<64;10;5M", // a wheel report split the same way
		"[<0;1;1m",   // a release
	} {
		if !orphanedMouseReport(runes(s)) {
			t.Errorf("%q was not recognized as an orphaned mouse report — it would be typed into the "+
				"composer, which is the report", s)
		}
	}
}

func TestATruncatedReportIsDroppedToo(t *testing.T) {
	// The sequence can split again mid-report, and a fragment is just as unfit to type as a whole one.
	//
	// THE BOUNDARY IS TWO CHARACTERS: a report always carries at least the button digit after `[<`, so a real
	// tail is three or more — which is why the bare `[<` (below) is left alone rather than guessed at.
	for _, s := range []string{"[<32;27", "[<4;", "[<32;", "[<3", "[<32;27;"} {
		if !orphanedMouseReport(runes(s)) {
			t.Errorf("the truncated report %q would be typed into the composer", s)
		}
	}
}

func TestOrdinaryTypingIsNeverDropped(t *testing.T) {
	// THE POINT OF THE ANCHOR. A textarea is full of characters this must leave alone: brackets, `<`, and
	// anything that merely CONTAINS a report's letters.
	for _, s := range []string{
		"a", "[", "<", "[<", "[<x", "[x<32;1;1M", "see [<32;27;30M]", "(a<b)", "[link]", "less-than <",
		"32;27;30", "array[i]", "[[shell]]", "M", "m", "[<32;27;30Z", // Z is not a report terminator
	} {
		if orphanedMouseReport(runes(s)) {
			t.Errorf("ordinary input %q was dropped as a mouse report", s)
		}
	}
}

func TestNonRuneKeysAreNeverDropped(t *testing.T) {
	// The predicate is about RUNES. A chord, an arrow, an enter — none of them is a report, and a guard that
	// swallowed one would break navigation to serve a leak.
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyEnter},
		{Type: tea.KeyUp},
		{Type: tea.KeyCtrlV},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: nil},
	} {
		if orphanedMouseReport(k) {
			t.Errorf("%v was dropped as a mouse report", k)
		}
	}
}

// THE DROP IS WIRED INTO THE FUNNEL, not merely available: a split report must not reach the composer, so
// the shell has to consume it before any key routing happens.
func TestASplitReportNeverReachesTheComposer(t *testing.T) {
	m, _, _ := newScopeApp(t)
	m.dock.SetValue("")
	before := m.dock.Value()

	if !m.dropOrphanedMouseReport(runes("[<32;27;30M")) {
		t.Fatal("the shell did not consume a split mouse report")
	}
	// And through the real entry point, so the guard's PLACEMENT is pinned too: nothing downstream may see
	// it, which is what the operator's "adds ... into the conversation box" was.
	next, _ := m.Update(runes("[<32;96;29M"))
	m2 := next.(*App)
	if got := m2.dock.Value(); got != before {
		t.Errorf("the composer holds %q, want it unchanged (%q) — the report was typed in", got, before)
	}
}
