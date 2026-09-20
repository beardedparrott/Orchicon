package theme

// theme_named_test.go — the hand-written community ports stay in the registry, and stay themselves.
//
// The palette gates in contrast_test.go iterate Names(), so they already cover these palettes' contrast.
// What they CANNOT catch is a palette quietly disappearing from the list, or being edited into something
// that is no longer the palette it claims to be — which is the specific risk of a set defined by its
// upstream hexes rather than generated from a spec.

import (
	"github.com/charmbracelet/lipgloss"

	"strings"
	"testing"
)

// EVERY PORT IS LISTED. A palette that is defined but unreachable is a palette nobody can choose.
func TestNamedPortsAreAllListed(t *testing.T) {
	have := map[string]bool{}
	for _, n := range Names() {
		have[n] = true
	}
	if len(namedThemes) < 8 {
		t.Fatalf("only %d community ports are defined — the operator asked for several more themes of "+
			"various colour spectrums", len(namedThemes))
	}
	for _, th := range namedThemes {
		if !have[th.Name] {
			t.Errorf("palette %q is defined but missing from the registry", th.Name)
		}
		if Lookup(th.Name) == nil {
			t.Errorf("Lookup(%q) = nil", th.Name)
		}
	}
}

// THE SET COVERS BOTH MODES, and each palette's declared character matches its background — the picker
// groups by IsDark, so a mislabelled palette lands in the wrong section and reads as a bug.
func TestNamedPortsCoverBothModes(t *testing.T) {
	var dark, light int
	for _, th := range namedThemes {
		switch {
		case isDarkHex(string(th.Bg)):
			dark++
		default:
			light++
		}
		if got, want := IsDark(th.Name), isDarkHex(string(th.Bg)); got != want {
			t.Errorf("%s: IsDark = %v, want %v (from its background %s)", th.Name, got, want, th.Bg)
		}
	}
	if dark == 0 || light == 0 {
		t.Errorf("the community ports are %d dark and %d light — the operator asked for both", dark, light)
	}
}

// EVERY TOKEN IS SET, and the text/background pair is legible. A zero token would render as no colour at
// all (an empty ANSI slot) rather than as an error, so it is asserted here rather than left to look odd.
func TestNamedPortsAreComplete(t *testing.T) {
	for _, th := range namedThemes {
		for label, c := range map[string]lipglossColor{
			"Bg": th.Bg, "Surface": th.Surface, "SurfaceAlt": th.SurfaceAlt,
			"Border": th.Border, "BorderFaint": th.BorderFaint,
			"Text": th.Text, "TextDim": th.TextDim, "TextFaint": th.TextFaint,
			"Accent": th.Accent, "AccentCyan": th.AccentCyan, "AccentIndigo": th.AccentIndigo,
			"Select": th.Select, "OK": th.OK, "Warn": th.Warn, "Err": th.Err, "Busy": th.Busy, "Tool": th.Tool,
		} {
			if !strings.HasPrefix(string(c), "#") || len(string(c)) != 7 {
				t.Errorf("%s: %s = %q is not a concrete #rrggbb colour", th.Name, label, c)
			}
		}
	}
}

// lipglossColor is a local alias so the completeness loop stays readable.
type lipglossColor = lipgloss.Color
