package tui

// composer_bound_wiring_test.go — THE COUNTER HAS TO BE TURNED ON.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/chatlimits"
)

// TestTheShellInstallsTheMessageBound renders the real frame and looks for the counter.
func TestTheShellInstallsTheMessageBound(t *testing.T) {
	m := newTestApp()
	m.dock.SetValue(strings.Repeat("a", chatlimits.WarnAt+1))

	view := m.View()
	if !strings.Contains(view, "80,001 / 100,000 characters") {
		t.Fatalf("the composer never shows the character counter, so the shell does not install the message "+
			"bound (dock.SetCharLimit) — the operator meets the limit mid-paste again:\n%s", view)
	}
}

// TestTheShellLeavesTheComposerQuietBelowTheBound is the other direction: the wiring must not turn the
// counter into permanent furniture.
func TestTheShellLeavesTheComposerQuietBelowTheBound(t *testing.T) {
	m := newTestApp()
	m.dock.SetValue(strings.Repeat("a", chatlimits.WarnAt-1))

	if view := m.View(); strings.Contains(view, "characters") {
		t.Fatalf("the composer shows a character count for ordinary input:\n%s", view)
	}
}
