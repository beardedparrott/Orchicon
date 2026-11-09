package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Regression for the operator's "I can see the Konsole matrix through the frame" report: a composed
// row carries inner styles whose \x1b[0m resets turn the background OFF for every cell after them.
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

	// Walk the row: after every reset, the next state must be a background SGR (or another escape) —
	// never printable content on the terminal's bg.
	rest := out
	resetCount := 0
	for {
		i := strings.Index(rest, "\x1b[0m")
		if i < 0 {
			break
		}
		resetCount++
		rest = rest[i+len("\x1b[0m"):]
		if rest == "" {
			break
		}
		if strings.HasPrefix(rest, "\x1b[") {
			continue // another escape sets its own state
		}
		// The next thing printed must not be an unpainted space/content run.
		t.Fatalf("cell(s) after reset %d render unpainted: %q", resetCount, firstN(rest, 20))
	}
	if resetCount == 0 {
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

// BOTH RESET SPELLINGS ARE REPAIRED, which they were not — and the gap had a visible symptom.
func TestRepairAfterResetsCatchesEveryResetSpelling(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	// Every reset spelling a producer here is known to emit: lipgloss/termenv's own terminator, and the
	// omitted-parameter form the bubbles viewport writes when it pads a line to the pane width.
	known := []string{"\x1b[0m", "\x1b[m"}

	// The sequence a repair has to produce after a reset: the pane's own background, derived from the same
	// style the panel repairs with (never hand-written — see TestOpaqueLeavesNoUnpaintedCells for why).
	const reset = "\x1b[0m"
	paint := PanelBgStyle.Render("")
	if !strings.HasSuffix(paint, reset) {
		t.Fatalf("fixture: PanelBgStyle does not render a trailing reset (%q)", paint)
	}
	bgOpen := strings.TrimSuffix(paint, reset)
	if bgOpen == "" {
		t.Fatal("fixture: PanelBgStyle renders no background — this test would prove nothing")
	}

	for _, spelling := range known {
		// A styled run, the reset, then the padding a viewport writes — the shape of the reported hole.
		row := "\x1b[38;2;0;0;0mhi" + spelling + "     "
		got := RepairAfterResets(row, PanelBgStyle)

		if !strings.Contains(got, spelling+bgOpen) {
			t.Errorf("the reset spelling %q was NOT repaired — the cells after it stay on the terminal's "+
				"background, which is the operator's coloured whitespace.\n  in : %q\n  out: %q",
				spelling, row, got)
		}
	}

	// AND THE PRODUCTION LIST COVERS THEM, so a spelling cannot be dropped from `resets` without this failing
	// (the behaviour above is asserted from the contract; this asserts the repair actually consults it).
	for _, spelling := range known {
		found := false
		for _, r := range resets {
			if r == spelling {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("`resets` does not carry %q, so RepairAfterResets never repairs it — the list and the "+
				"producers have drifted (have: %q)", spelling, resets)
		}
	}

	// AND THE SCAN PICKS THE EARLIEST OF EITHER, which is what makes one pass enough when both appear in a
	// single row (the shell's own \x1b[0m rows and a viewport's \x1b[m padding can meet in one line).
	mixed := "\x1b[38;2;0;0;0ma\x1b[0m  b\x1b[m  "
	i, n := nextReset(mixed)
	if i != strings.Index(mixed, "\x1b[0m") {
		t.Errorf("nextReset found a reset at %d, want the EARLIEST (%d)", i, strings.Index(mixed, "\x1b[0m"))
	}
	if n != len("\x1b[0m") {
		t.Errorf("nextReset reported length %d, want %d", n, len("\x1b[0m"))
	}

	// A row with no reset at all is untouched, and reports no reset — otherwise the walk in the repair loops.
	if i, n := nextReset("plain text"); i != -1 || n != 0 {
		t.Errorf("nextReset on a reset-free string = (%d, %d), want (-1, 0)", i, n)
	}
}
