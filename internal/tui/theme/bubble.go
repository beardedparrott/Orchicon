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
// on both fills. This is a single place to change if a future palette ever
// needs a different fill/text pairing.
func bubbleText(fill string, t Theme) lipgloss.Color {
	_ = fill
	return lipgloss.Color(t.Text)
}

// bubbleFills derives the user/model bubble backgrounds for a palette.
//
// The relationships are chosen so the two fills are visibly SEPARATED on every
// palette (asserted by TestBubbleContrast), which is harder on light themes: a
// near-white page leaves little room to go lighter, so the MODEL bubble is made
// a genuinely deeper neutral instead and the user's keeps only a light accent
// tint. Either way the user's fill is the lighter of the two.
func bubbleFills(bg, accent string) (user, model string) {
	if isDarkHex(bg) {
		// A clearly lifted user bubble over a subtly lifted model panel.
		return hexBlend(bg, "#ffffff", 0.17), hexBlend(bg, "#ffffff", 0.055)
	}
	return hexBlend(bg, accent, 0.12), hexBlend(bg, "#000000", 0.26)
}
