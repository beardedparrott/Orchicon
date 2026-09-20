package theme

// ember_gui_match_test.go — THE GUI'S EMBER NIGHT IS THE TUI'S EMBER PALETTE.
//
// The operator, after the first attempt: "You didn't really change the color scheme of the gui theme Ember
// Night. I wanted the colors of that gui theme to match the theme of Ember in the TUI. It's close but not
// quite the same."
//
// THEY WERE RIGHT AND "CLOSE" WAS THE PROBLEM. That attempt changed only the accent and asserted in a
// comment that the rest "already matched exactly" — the background was 12 30% 8% against 14 32% 8%, the
// card 12 25% 12% against 13 31% 12%, and the border 12 15% 17% against 12 29% 30%. Nobody would notice a
// two-point hue shift; everybody notices a border at half the lightness.
//
// So the claim is tested instead of asserted. This reads frontend/src/index.css, finds the
// `[data-theme="orchicon-dark-ember"]` block, and compares each token to the value the TUI derives for the
// SAME palette — which makes the two clients' versions of "Ember" the same shape of thing to keep in step,
// and turns a future divergence into a failing test rather than a screenshot.

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// guiCSSPath is the stylesheet, relative to this package.
const guiCSSPath = "../../../frontend/src/index.css"

// hslOf converts a #rrggbb to the `H S% L%` triple index.css writes. Rounded to whole values, which is the
// precision the stylesheet stores — so the comparison cannot fail on a sub-point of arithmetic.
func hslOf(t *testing.T, hex string) string {
	t.Helper()
	if len(hex) != 7 || hex[0] != '#' {
		t.Fatalf("not a hex colour: %q", hex)
	}
	n := func(s string) float64 {
		v, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return float64(v) / 255
	}
	r, g, b := n(hex[1:3]), n(hex[3:5]), n(hex[5:7])
	max := math.Max(r, math.Max(g, b))
	min := math.Min(r, math.Min(g, b))
	l := (max + min) / 2
	var h, sat float64
	if max != min {
		d := max - min
		if l > 0.5 {
			sat = d / (2 - max - min)
		} else {
			sat = d / (max + min)
		}
		switch max {
		case r:
			h = (g - b) / d
			if g < b {
				h += 6
			}
		case g:
			h = (b-r)/d + 2
		default:
			h = (r-g)/d + 4
		}
		h *= 60
	}
	return strconv.Itoa(int(math.Round(h))%360) + " " +
		strconv.Itoa(int(math.Round(sat*100))) + "% " +
		strconv.Itoa(int(math.Round(l*100))) + "%"
}

// emberGUITokens reads the ember-night block and returns token → value, minus any comments.
func emberGUITokens(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(guiCSSPath)
	if err != nil {
		t.Fatalf("read the stylesheet: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, `[data-theme="orchicon-dark-ember"].dark,`)
	if start < 0 {
		t.Fatalf("%s has no orchicon-dark-ember block — the GUI default theme is missing", guiCSSPath)
	}
	block := src[start:]
	if end := strings.Index(block, "\n  }"); end > 0 {
		block = block[:end]
	}
	// Strip /* … */ so a token mentioned in a comment is not read as a declaration.
	block = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(block, "")
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "--") {
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(name)] = strings.TrimSuffix(strings.TrimSpace(val), ";")
	}
	return out
}

// THE WHOLE PALETTE, TOKEN BY TOKEN.
func TestTheGUIEmberThemeMatchesTheTUIPalette(t *testing.T) {
	ember := Lookup("ember")
	if ember == nil {
		t.Fatal("the TUI has no ember palette to match against")
	}
	tokens := emberGUITokens(t)

	cases := []struct {
		token string
		hex   string
		role  string
	}{
		{"--background", string(ember.Bg), "the window background"},
		{"--foreground", string(ember.Text), "body text"},
		{"--card", string(ember.Surface), "panels"},
		{"--card-foreground", string(ember.Text), "text on panels"},
		{"--popover", string(ember.Surface), "popovers"},
		{"--popover-foreground", string(ember.Text), "text in popovers"},
		{"--primary", string(ember.Accent), "the accent"},
		{"--primary-foreground", string(ember.Bg), "text on the accent"},
		{"--secondary", string(ember.SurfaceAlt), "raised surfaces"},
		{"--secondary-foreground", string(ember.Text), "text on raised surfaces"},
		{"--muted", string(ember.SurfaceAlt), "muted surfaces"},
		{"--muted-foreground", string(ember.TextDim), "muted text"},
		{"--accent", string(ember.SurfaceAlt), "the neutral accent surface"},
		{"--accent-foreground", string(ember.Text), "text on it"},
		{"--border", string(ember.Border), "borders"},
		{"--ring", string(ember.Accent), "focus rings"},
		{"--glass-input-focus", string(ember.Accent), "input focus"},
		{"--scroll-thumb-hover", string(ember.Accent), "the scrollbar on hover"},
		{"--nav-active-from", string(ember.Accent), "the active tab pill's gradient start"},
		{"--nav-active-to", string(ember.AccentIndigo), "the active tab pill's gradient end"},
		{"--nav-active-border", string(ember.Accent), "the active tab pill's border"},
		{"--nav-active-glow", string(ember.Accent), "the active tab pill's glow"},
		{"--mesh-glow-1", string(ember.Accent), "the ambient trio's first pass"},
		{"--mesh-glow-2", string(ember.AccentIndigo), "the ambient trio's second pass"},
		{"--mesh-glow-3", string(ember.AccentCyan), "the ambient trio's third pass"},
	}
	// THE USER CONVERSATION BUBBLE IS THE TUI'S OWN USER BAND, and it is asserted HERE rather than left to
	// inspection because that is exactly how it went wrong. The bubble was painted with `bg-primary`, so it
	// inherited the accent — and when this theme's accent became ember's orange, every message the operator wrote
	// turned into a wall of orange ("I am not a fan of the orange conversation bubble for users").
	//
	// The TUI derives its user band from bubbleFills(), not from the accent, so recomputing it here is what keeps
	// the two clients showing the same thing — and it is a different value from --primary, which is the point.
	// The palette's TEXT is passed because the lift is now bounded by it (see bubbleFills); this test would
	// otherwise assert against a fill the runtime never produces.
	userFill, _ := bubbleFills(string(ember.Bg), string(ember.Accent))
	if hslOf(t, userFill) == hslOf(t, string(ember.Accent)) {
		t.Fatalf("fixture: the TUI's user band equals its accent (%s), so this assertion could not tell the two "+
			"apart", userFill)
	}
	cases = append(cases, struct {
		token string
		hex   string
		role  string
	}{"--user-bubble", userFill, "the operator's conversation bubble (the TUI's user band, NOT the accent)"})
	for _, c := range cases {
		got, ok := tokens[c.token]
		if !ok {
			t.Errorf("%s is not declared in the GUI's ember-night block — %s would fall back to "+
				"whatever the base theme sets", c.token, c.role)
			continue
		}
		want := hslOf(t, c.hex)
		if got != want {
			t.Errorf("GUI %s = %q, but the TUI's ember %s is %q (from %s). The two clients advertise the "+
				"same theme; a token that is only CLOSE is what the operator reported as \"it's close but "+
				"not quite the same\"", c.token, got, c.role, want, c.hex)
		}
	}
}

// AND THE LITERAL COLOURS TOO — the three box-shadows cannot take an hsl token, so they carry the accent's
// RGB by hand and are the easiest place for the match to rot silently.
func TestTheGUIEmberLiteralsCarryTheAccentRGB(t *testing.T) {
	tokens := emberGUITokens(t)
	for _, token := range []string{"--glass-menu-shadow", "--glass-input-glow", "--glass-input-focus-glow"} {
		val, ok := tokens[token]
		if !ok {
			t.Errorf("%s is not declared in the ember block", token)
			continue
		}
		// hsl(25 89% 58%) = #f38435 = rgb(243, 132, 53).
		if !strings.Contains(val, "rgba(243, 132, 53") {
			t.Errorf("%s = %q, which does not carry the accent's RGB (243, 132, 53) — a literal is the "+
				"easiest place for the accent to go stale, because nothing recomputes it", token, val)
		}
	}
}
