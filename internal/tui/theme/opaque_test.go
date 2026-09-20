package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Regression for the operator's "I can see the Konsole matrix through the
// frame" report: a composed row carries inner styles whose \x1b[0m resets turn
// the background OFF for every cell after them. Opaque must re-assert the
// background so no cell is left on the terminal's own background.
func TestOpaqueLeavesNoUnpaintedCells(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	// A row the way a panel builds it: a border cell and a styled title, both
	// of which emit their own trailing reset.
	border := lipgloss.NewStyle().Foreground(lipgloss.Color("#445566"))
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Bold(true)
	row := border.Render("┌─ ") + title.Render("Workers") + border.Render(" ─┐")

	out := Opaque(row, 40)

	if w := lipgloss.Width(out); w != 40 {
		t.Fatalf("Opaque width = %d, want exactly 40", w)
	}

	// Walk the row: after every reset, the next state must be a background
	// SGR (or another escape) — never printable content on the terminal's bg.
	rest := out
	resets := 0
	for {
		i := strings.Index(rest, "\x1b[0m")
		if i < 0 {
			break
		}
		resets++
		rest = rest[i+len("\x1b[0m"):]
		if rest == "" {
			break
		}
		if strings.HasPrefix(rest, "\x1b[") {
			continue // another escape sets its own state
		}
		// The next thing printed must not be an unpainted space/content run.
		t.Fatalf("cell(s) after reset %d render unpainted: %q", resets, firstN(rest, 20))
	}
	if resets == 0 {
		t.Fatal("test row produced no resets — the fixture is not exercising the repair")
	}
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Opaque must still honour an empty profile (no background to assert) and a
// width of zero without inventing output.
func TestOpaqueDegradesSafely(t *testing.T) {
	if got := Opaque("x", 0); got != "x" {
		t.Fatalf("width 0 must pass through, got %q", got)
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	if got := Opaque("abc", 6); lipgloss.Width(got) != 6 {
		t.Fatalf("ascii profile width = %d, want 6", lipgloss.Width(got))
	}
}
