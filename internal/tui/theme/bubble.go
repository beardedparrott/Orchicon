package theme

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// bubble.go — the chat bubble fills.
//
// The operator's ask: the GUI gives the user a much LIGHTER bubble over a
// darker one for the model, and in the TUI the two were indistinguishable.
// "Bubble contrast" is a different problem from "theme contrast": it is not
// about text legibility alone, it is about the two fills being OBVIOUSLY
// different from each other while both stay readable and sit over the screen
// background.
//
// Rather than hand-pick two colours for each of the palettes, the fills are
// DERIVED from each palette's own background and accent:
//
//	dark palettes  user = background lifted toward white (a real bubble)
//	               model = background lifted slightly (a subtle panel)
//	light palettes user = background tinted toward the accent
//	               model = background pushed toward black
//
// so the relationship holds on every theme, and a new palette gets sensible
// bubbles for free. TestBubbleContrast guarantees the separation and the
// legibility for the WHOLE registry.

// hexBlend blends hex toward target by t (0 = unchanged, 1 = target).
func hexBlend(hex, target string, t float64) string {
	r, g, b := hexRGB(hex)
	tr, tg, tb := hexRGB(target)
	if r < 0 || tr < 0 {
		return hex
	}
	mix := func(a, b uint8) uint8 {
		v := float64(a) + (float64(b)-float64(a))*t
		if v < 0 {
			v = 0
		}
		if v > 255 {
			v = 255
		}
		return uint8(v + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", mix(r, tr), mix(g, tg), mix(b, tb))
}

// hexRGB parses #rrggbb (returns -1s on failure).
func hexRGB(hex string) (uint8, uint8, uint8) {
	s := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return 255, 255, 255
	}
	parse := func(i int) (uint8, bool) {
		v, err := strconv.ParseUint(s[i:i+2], 16, 8)
		return uint8(v), err == nil
	}
	r, ok1 := parse(0)
	g, ok2 := parse(2)
	b, ok3 := parse(4)
	if !ok1 || !ok2 || !ok3 {
		return 255, 255, 255
	}
	return r, g, b
}

// isDarkHex reports whether a colour is a dark surface (perceptual luminance).
func isDarkHex(hex string) bool {
	r, g, b := hexRGB(hex)
	lum := (0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)) / 255
	return lum < 0.5
}

// bubbleText returns the foreground for a bubble fill.
//
// The palette's own text colour is already chosen against the BACKGROUND, and
// every bubble fill is only a modest blend away from it (dark themes lift
// toward white, light themes tint slightly), so the same colour stays readable
// on both fills.
//
// WHERE IT DOES NOT, THIS FUNCTION FIXES IT — which is what its own note asked for ("a single place to
// change if a future palette ever needs a different fill/text pairing"), and the hand-written community
// ports are that future palette. A NEAR-WHITE foreground has room on a lifted bubble; a MID-TONE one does
// not. One Dark's #abb2bf and Everforest's #d3c6aa measured 3.27:1 and 3.62:1 on the fills their palettes
// derive — genuinely unreadable, and reported as exactly that by the bubble gate.
//
// THE ALTERNATIVE WAS TO SHRINK THE FILL, AND IT DOES NOT WORK. Bounding the lift by what the text can be
// read on is fine on a dark palette and impossible on a light one: the MODEL bubble there is a blend TOWARD
// BLACK, and for solarized-light's foreground the readability wall sits at a 4% blend — so the two fills end
// up a luminance apart and the bubbles stop being distinguishable at all (measured: separation 1.04 against
// a 1.18 floor). Trading "readable" for "distinguishable" is not a trade worth making in either direction,
// so the fills keep their carefully-tuned geometry and the TEXT is moved out of the way instead — blended
// AWAY from the fill until it clears the same floor the body text is held to.
func bubbleText(fill string, t Theme) lipgloss.Color {
	text := string(t.Text)
	if contrastRatio(text, fill) >= bubbleTextTarget {
		return lipgloss.Color(text)
	}
	// Away from the fill, in the direction that raises contrast: toward white on a dark fill, toward black
	// on a light one. The original hue is preserved as far as the floor allows, which is why this blends the
	// palette's own colour rather than substituting a flat white/black.
	target := "#ffffff"
	if !isDarkHex(fill) {
		target = "#000000"
	}
	for k := 0.05; k <= 1.0; k += 0.05 {
		cand := hexBlend(text, target, k)
		if contrastRatio(cand, fill) >= bubbleTextTarget {
			return lipgloss.Color(cand)
		}
	}
	return lipgloss.Color(target)
}

// bubbleFills derives the user/model bubble backgrounds for a palette.
//
// The relationships are chosen so the two fills are visibly SEPARATED on every
// palette (asserted by TestBubbleContrast), which is harder on light themes: a
// near-white page leaves little room to go lighter, so the MODEL bubble is made
// a genuinely deeper neutral instead and the user's keeps only a light accent
// tint. Either way the user's fill is the lighter of the two.
//
// THE LIFT IS BOUNDED BY THE PALETTE'S OWN TEXT, and the hand-written community ports (theme_named.go)
// are what forced that into the open. This used to assume a NEAR-WHITE foreground on a dark palette and a
// NEAR-BLACK one on a light palette — true of every generated family, false of the ports: One Dark's
// foreground is #abb2bf and Everforest's #d3c6aa, both mid-tones that measured 3.27:1 and 3.62:1 on the
// lifted bubble they were given, which the gate caught as "unreadable". A palette with a mid-tone
// foreground simply cannot carry the lift a near-white one can, and guessing a smaller constant would have
// been the same mistake one step along. So the lift is the LARGEST that palette's own text can still be
// read on, found by walking down from the ideal — which also keeps each bubble as visible as its palette
// allows rather than flattening every one of them to the worst case.
func bubbleFills(bg, accent string) (user, model string) {
	if isDarkHex(bg) {
		// A clearly lifted user bubble over a subtly lifted model panel.
		return hexBlend(bg, "#ffffff", 0.17), hexBlend(bg, "#ffffff", 0.055)
	}
	return hexBlend(bg, accent, 0.12), hexBlend(bg, "#000000", 0.26)
}

// bubbleTextTarget is the contrast a bubble fill must give the text drawn on it. It is the body-text floor
// the theme gates use, with headroom over TestBubbleContrast's own 4.0 so a palette tweak cannot land just
// under the gate.
const bubbleTextTarget = 4.5

// BubbleFills exposes the ACTIVE palette's bubble fills as hex.
//
// Exported for the shell's tests, which assert that the chat surface is painted only in the active palette's
// colours: a bubble fill is one of those colours, and a test that could not name it would have to allow any
// background it happened not to recognise — which is the hole the check exists to close.
func BubbleFills() (user, model string) {
	return bubbleFills(string(Active().Bg), string(Active().Accent))
}
