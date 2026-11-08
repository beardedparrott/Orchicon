package tui

// composer_bound_wiring_test.go — THE COUNTER HAS TO BE TURNED ON.
//
// The dock can count (dock/composer_charcounter_test.go), but only the shell knows it is a CHAT composer, and
// only the shell can install the bound. A counter no screen enables is exactly the "seam nothing calls" this
// change exists to avoid — so this asserts the outward view, not the setter.

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
