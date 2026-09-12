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

// Every registered theme must be selectable, uniquely named, non-empty, and
// must actually re-derive the styles (a theme that renders identically to the
// previous one is a silent failure).
func TestRegistryIsUsable(t *testing.T) {
	names := Names()
	if len(names) < 2 {
		t.Fatalf("registry has %d themes, want at least 2", len(names))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" {
			t.Fatal("a theme has an empty name")
		}
		if seen[n] {
			t.Fatalf("duplicate theme name %q in the registry", n)
		}
		seen[n] = true
		if Lookup(n) == nil {
			t.Fatalf("Lookup(%q) = nil but Names() lists it", n)
		}
		if !Use(n) {
			t.Fatalf("Use(%q) failed", n)
		}
		if Active().Name != n {
			t.Fatalf("Use(%q) left active = %q", n, Active().Name)
		}
		// The derived styles must carry the palette's colours.
		if got := string(Dark.Bg); n == Dark.Name && string(Active().Bg) != got {
			t.Fatalf("%s: active Bg = %q, want %q", n, Active().Bg, got)
		}
		if ScreenBg.Render("x") == "" {
			t.Fatalf("%s: ScreenBg renders nothing — the background is unset", n)
		}
	}
	if Lookup("definitely-not-a-theme") != nil {
		t.Fatal("Lookup must return nil for an unknown name")
	}
	if Use("definitely-not-a-theme") {
		t.Fatal("Use must reject an unknown name")
	}
	Use(DefaultName)
}

// The SELECTION FILL carries WHITE text (selected rows, the active tab pill,
// the file-selection chip). A fill that is too light renders the selection as a
// blank block — and this is exactly what the extended palette set risked, since
// each family's fill lightness comes from its accent.
func TestSelectionFillCarriesWhiteText(t *testing.T) {
	const white = "#f8fafc"
	for _, name := range Names() {
		Use(name)
		th := Active()
		if r := contrastRatio(white, string(th.Select)); r < 3.0 {
			t.Errorf("%s: white on the selection fill (%s) is %.2f:1 — a selected row reads as a blank block", name, th.Select, r)
		}
		// The fill must also be visible against the pane background.
		if r := contrastRatio(string(th.Select), string(th.Bg)); r < 1.5 {
			t.Errorf("%s: the selection fill (%s) is indistinguishable from the background", name, th.Select)
		}
	}
	Use(DefaultName)
}

// The palette set must cover the requested families in both modes, so the
// operator can actually choose greens, blues, purples, greys, etc.
func TestPaletteFamiliesPresent(t *testing.T) {
	have := map[string]bool{}
	for _, n := range Names() {
		have[n] = true
	}
	want := []string{
		// dark families (greens, blues, purples, blacks, greys…)
		"obsidian", "forest", "ocean", "violet", "ember", "amber", "rose", "teal", "crimson", "slate",
		// light families (light green, light purple, light grey…)
		"lumen", "forest-light", "ocean-light", "violet-light", "ember-light", "amber-light", "rose-light", "teal-light", "slate-light",
		// the ported pair that existing configs reference
		"dark", "light", "gruvbox-dark", "gruvbox-light",
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("palette %q is missing from the registry", n)
		}
	}
	if len(Names()) < 20 {
		t.Errorf("registry has %d palettes, want the full family set (>= 20)", len(Names()))
	}
}

// The operator's ask: the user's bubble must be OBVIOUSLY lighter than the
// model's (the GUI's relationship), on every palette — not merely a different
// token. Both fills must also keep the text readable and stay distinguishable
// from the screen background.
func TestBubbleContrast(t *testing.T) {
	for _, name := range Names() {
		Use(name)
		th := Active()
		bg := string(th.Bg)
		user := BubbleUser.GetBackground()
		model := BubbleModel.GetBackground()
		if user == nil || model == nil {
			t.Fatalf("%s: bubble fills are unset", name)
		}
		us, ms := colorHex(user), colorHex(model)
		if us == "" || ms == "" {
			t.Fatalf("%s: bubble fills are not concrete colours (%v / %v)", name, user, model)
		}
		if us == ms {
			t.Fatalf("%s: user and model bubbles are the SAME colour (%s)", name, us)
		}
		// The two fills must be visibly separated from each other.
		if r := contrastRatio(us, ms); r < 1.18 {
			t.Errorf("%s: user(%s) vs model(%s) bubble contrast is %.2f — they read as the same bubble", name, us, ms, r)
		}
		// Each fill must be distinct from the screen background it sits on.
		for label, c := range map[string]string{"user": us, "model": ms} {
			if r := contrastRatio(c, bg); r < 1.04 {
				t.Errorf("%s: the %s bubble (%s) is indistinguishable from the background (%s)", name, label, c, bg)
			}
		}
		// Text must stay readable on BOTH fills.
		for label, c := range map[string]string{"user": us, "model": ms} {
			if r := contrastRatio(string(th.Text), c); r < 4.0 {
				t.Errorf("%s: text on the %s bubble (%s) is %.2f:1 — unreadable", name, label, c, r)
			}
		}
	}
	Use(DefaultName)
}

// colorHex extracts the #rrggbb from a lipgloss colour.
func colorHex(c lipgloss.TerminalColor) string {
	if c == nil {
		return ""
	}
	type stringer interface{ String() string }
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	if s, ok := c.(lipgloss.Color); ok {
		return string(s)
	}
	return ""
}
