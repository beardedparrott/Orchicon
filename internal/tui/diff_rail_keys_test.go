package tui

// diff_rail_keys_test.go — the KEYBOARD FLOOR for the diff rail's width.
//
// The drag is the primary gesture, but a terminal that does not report motion (or an operator without a
// mouse) must still be able to size the rail. These chords are the floor: ctrl+right widens, ctrl+left
// narrows, ctrl+down resets to auto. They must be non-text (this repo's composerBypassKeys rule), bound by
// no bubbles textarea key, registered in the route list so the help overlay cannot drift, and gated on the
// pane being OPEN.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func ctrlArrow(t *testing.T, key tea.KeyType) tea.KeyMsg {
	t.Helper()
	return tea.KeyMsg{Type: key}
}

// THE CHORD GROWS AND SHRINKS the rail by one step, and the composed content follows.
func TestDiffRailCtrlArrowsResize(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	start := m.diffPaneWidth() // the auto seed
	startContent := m.contentWidth()

	nm, _ := m.Update(ctrlArrow(t, tea.KeyCtrlRight))
	m = nm.(*App)
	if got, want := m.diffPaneWidth(), start+diffResizeStep; got != want {
		t.Fatalf("ctrl+right: rail = %d, want the seed %d + step %d", got, start, diffResizeStep)
	}
	if delta := startContent - m.contentWidth(); delta != diffResizeStep {
		t.Errorf("ctrl+right moved contentWidth by %d, want %d", delta, diffResizeStep)
	}
	if got, want := contentStartColumn(t, m), start+diffResizeStep; got != want {
		t.Errorf("the composed content starts at %d after ctrl+right, want %d", got, want)
	}

	nm, _ = m.Update(ctrlArrow(t, tea.KeyCtrlLeft))
	m = nm.(*App)
	if got, want := m.diffPaneWidth(), start; got != want {
		t.Errorf("ctrl+left brought the rail to %d, want back to the seed %d", got, want)
	}
}

// CTRL+DOWN RESETS TO AUTO — and the reset must clear the OVERRIDE, not just move the width, or a restart
// would resurrect the abandoned value.
func TestDiffRailCtrlDownResetsToAuto(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	auto := m.diffPaneAutoWidth()

	nm, _ := m.Update(ctrlArrow(t, tea.KeyCtrlRight))
	m = nm.(*App)
	nm, _ = m.Update(ctrlArrow(t, tea.KeyCtrlRight))
	m = nm.(*App)
	if m.diffPaneW == 0 {
		t.Fatal("the chords did not set an override — nothing to reset")
	}

	nm, _ = m.Update(ctrlArrow(t, tea.KeyCtrlDown))
	m = nm.(*App)

	if m.diffPaneW != 0 {
		t.Errorf("ctrl+down left the override at %d, want 0 (auto)", m.diffPaneW)
	}
	if got := m.diffPaneWidth(); got != auto {
		t.Errorf("after ctrl+down the rail is %d, want the auto width %d", got, auto)
	}
	if got := contentStartColumn(t, m); got != auto {
		t.Errorf("the composed content starts at %d after reset, want the auto width %d", got, auto)
	}
}

// THE CHORDS ARE A NO-OP WITH THE PANE CLOSED: they must fall through, not resize an invisible rail.
func TestDiffRailChordsAreInertWithThePaneClosed(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	m.diffOpen = false

	for _, key := range []tea.KeyType{tea.KeyCtrlRight, tea.KeyCtrlLeft, tea.KeyCtrlDown} {
		nm, _ := m.Update(ctrlArrow(t, key))
		m = nm.(*App)
		if m.diffPaneW != 0 {
			t.Errorf("key %v set an override (%d) with the pane closed", key, m.diffPaneW)
		}
	}
}

// THE CHORD'S KEYS ARE NON-TEXT AND REGISTERED. The route list is the source the help overlay renders from
// (TestHelpOverlayMatchesKeymap asserts the parity); what is asserted HERE is that each chord has a route
// whose Match fires, so the behaviour and the registry cannot drift.
func TestDiffRailResizeChordsAreRegisteredNonText(t *testing.T) {
	routes := GlobalKeyRoutes(Tabs)
	want := map[string]bool{"ctrl+right": false, "ctrl+left": false, "ctrl+down": false}
	for _, r := range routes {
		if _, ok := want[r.Keys]; !ok {
			continue
		}
		want[r.Keys] = true
		if r.Scope != "global" {
			t.Errorf("route %q is scope %q, want global (the chord must work from any focus)", r.Keys, r.Scope)
		}
		if r.Match == nil {
			t.Fatalf("route %q has no Match", r.Keys)
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("no route registered for the diff-rail chord %s", key)
		}
	}
	// NON-TEXT: every one of them must be a composer bypass, or the textarea would eat it while composing.
	for key := range want {
		if !composerBypassKeys[key] {
			t.Errorf("%s is not in composerBypassKeys — the composer would consume the chord", key)
		}
	}
}

// AND THE CHORD IS REACHABLE WHILE THE COMPOSER HOLDS THE FOCUS (the launch default) because the bypass
// map carries it past the textarea.
func TestDiffRailChordsAreReachableFromTheComposer(t *testing.T) {
	m := newDiffRailApp(t, 120, 40)
	m.setFocus(focusComposer)
	start := m.diffPaneWidth()

	nm, _ := m.Update(ctrlArrow(t, tea.KeyCtrlRight))
	m = nm.(*App)
	if got, want := m.diffPaneWidth(), start+diffResizeStep; got != want {
		t.Fatalf("ctrl+right from the composer gave %d, want %d — the chord did not bypass the textarea", got, want)
	}
}
