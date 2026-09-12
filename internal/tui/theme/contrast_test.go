package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// relLuminance is the WCAG relative luminance of a #rrggbb colour.
func relLuminance(hex string) float64 {
	if len(hex) != 7 || hex[0] != '#' {
		return -1
	}
	parse := func(s string) float64 {
		var v float64
		for _, c := range s {
			v *= 16
			switch {
			case c >= '0' && c <= '9':
				v += float64(c - '0')
			case c >= 'a' && c <= 'f':
				v += float64(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v += float64(c-'A') + 10
			}
		}
		return v
	}
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return ((c + 0.055) / 1.055) * ((c + 0.055) / 1.055) * 1.055
	}
	return 0.2126*lin(parse(hex[1:3])) + 0.7152*lin(parse(hex[3:5])) + 0.0722*lin(parse(hex[5:7]))
}

// contrastRatio is the WCAG contrast ratio between two #rrggbb colours.
func contrastRatio(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Regression for the operator's "the light theme was abysmal — I couldn't see
// anything". The GUI's border tokens are tasteful hairlines in a browser, but in
// a TUI the border is the ONLY thing separating panes AND carries each panel's
// title, so a ~1.1:1 border against the background makes the whole layout read
// as blank. Structural colours must stay legible in BOTH themes.
func TestStructuralContrast(t *testing.T) {
	for _, name := range Names() {
		Use(name)
		th := Active()
		bg := string(th.Bg)

		// Body text must be comfortably readable.
		if r := contrastRatio(string(th.Text), bg); r < 4.5 {
			t.Errorf("%s: Text contrast %.2f vs background %s, want >= 4.5", name, r, bg)
		}
		// Dimmed text (hints, meta) still needs to be readable.
		if r := contrastRatio(string(th.TextDim), bg); r < 3.0 {
			t.Errorf("%s: TextDim contrast %.2f vs background, want >= 3.0", name, r)
		}
		// Structural lines: the pane borders that carry the titles.
		if r := contrastRatio(string(th.Border), bg); r < 1.8 {
			t.Errorf("%s: Border contrast %.2f vs background — pane borders and their titles will be invisible", name, r)
		}
		// Status colours must be distinguishable from the background.
		for label, c := range map[string]lipgloss.Color{"OK": th.OK, "Warn": th.Warn, "Err": th.Err} {
			if r := contrastRatio(string(c), bg); r < 2.5 {
				t.Errorf("%s: %s contrast %.2f vs background, want >= 2.5", name, label, r)
			}
		}
	}
	Use(DefaultName)
}

// The two themes must be genuinely different palettes (a switch that changes
// nothing is a silent failure).
func TestThemesDiffer(t *testing.T) {
	if string(Dark.Bg) == string(Light.Bg) {
		t.Fatal("dark and light share a background — the switch would be a no-op")
	}
	if contrastRatio(string(Dark.Text), string(Dark.Bg)) < 4.5 {
		t.Error("dark theme body text is too low-contrast")
	}
}
