package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Regression for the operator's "blue box riding off the pane": the composer's
// composed rows ended with an OPEN surface-background SGR (the repair
// re-asserted the box background before a line break, and at the block's end).
// The shell pads every row out to the terminal width, so that open background
// leaked onto the padding — a band of the box's colour extending right, past
// its border.
//
// The contract: a composed row must never END with an unterminated SGR, so
// whatever the caller appends inherits the caller's own background, not the
// block's.
func TestComposedRowsDoNotLeakTheirBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	m := phase3App(120, 40)
	d := m.dock
	d.Width = 76

	for i, line := range strings.Split(d.View(), "\n") {
		if strings.HasSuffix(line, "\x1b[0m") {
			continue // properly terminated
		}
		t.Fatalf("dock row %d does not end with a reset — its background leaks onto the caller's padding:\n%q", i, line)
	}

	// And the composed launch block must be clean too.
	for i, line := range m.centeredWelcomeView(120, 30) {
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, "\x1b[0m") && strings.Contains(line, "\x1b[") {
			t.Fatalf("launch row %d leaves an open SGR:\n%q", i, line)
		}
	}
}
