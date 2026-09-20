package theme

// transparent_test.go — A TRANSPARENT THEME LEAVES THE BACKGROUND UNPAINTED, AND KEEPS THE TINTS.
//
// The operator: "I would like a couple of semi-transparent background themes for dark and light" ... "Yes
// the tints should definitely be there" ... "Composer should also be transparent as well on transparent
// themes."
//
// "Transparent" in a terminal cannot mean alpha — a cell has a foreground and a background and nothing in
// between, so there is no opacity to set. What it CAN mean is "this cell is not painted at all", which is
// what these tests assert: NO background sequence is emitted for the app background, while every tinted
// surface still emits its own. That distinction is the whole feature, so it is asserted in both directions
// (absent where it must be, present where it must be) rather than only the absence.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// bgSeq reports whether a rendered style emitted ANY background sequence.
func bgSeq(s string) bool { return strings.Contains(s, "48;2;") || strings.Contains(s, "48;5;") }

// rgbOf returns the `48;2;r;g;b` sequence a #rrggbb colour renders as, for an exact-match assertion.
func rgbOf(t *testing.T, hex string) string {
	t.Helper()
	r, g, b := hexRGB(hex)
	if r < 0 {
		t.Fatalf("bad hex %q", hex)
	}
	return "48;2;" + itoa(int(r)) + ";" + itoa(int(g)) + ";" + itoa(int(b))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// THE COMPOSER IS UNPAINTED ON A TRANSPARENT THEME AND FILLED ON A SOLID ONE, and the two are checked together
// because the solid half is what stops "unpainted" from passing on a theme that has simply stopped painting.
func TestTheComposerIsTransparentOnATransparentTheme(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		Use(DefaultName)
	})

	Use("forest-transparent")
	if out := ComposerBox.Render("x"); bgSeq(out) {
		t.Errorf("the composer box painted a background on a transparent theme (%q)", out)
	}
	if out := ComposerBg.Render(""); bgSeq(out) {
		t.Errorf("the composer's repair style painted a background (%q) — its repair would re-paint the "+
			"box the theme exists to leave alone", out)
	}

	// AND THE SOLID THEME IS UNCHANGED — the composer is a filled panel there, as before.
	Use("forest")
	if out := ComposerBox.Render("x"); !bgSeq(out) {
		t.Errorf("the composer stopped painting its fill on a SOLID theme (%q)", out)
	}
	if out := ComposerBg.Render(""); !bgSeq(out) {
		t.Error("the composer's repair style stopped carrying a fill on a solid theme")
	}
}

// SWITCHING BACK RESTORES THE BACKGROUND. The unpainted value must not leak into the next theme — a
// statefulness bug here would leave the client transparent until restart, which is exactly the class of
// "it kept the old theme" complaint this area has produced before.
func TestLeavingATransparentThemeRestoresTheBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		Use(DefaultName)
	})

	Use("forest")
	solid := ScreenBg.Render("x")
	if !bgSeq(solid) {
		t.Fatal("fixture: the solid theme painted no background")
	}

	Use("forest-transparent")
	if bgSeq(ScreenBg.Render("x")) {
		t.Fatal("the transparent theme painted a background")
	}

	Use("forest")
	if got := ScreenBg.Render("x"); !bgSeq(got) {
		t.Errorf("returning to a solid theme left the background unpainted (%q)", got)
	}
}

// A TRANSPARENT THEME IS ITS BASE PALETTE, UNCHANGED, WITH THE BACKGROUND LEFT OUT. Asserted so the pair
// cannot drift: the tints on a transparent theme are the tints the operator chose.
func TestATransparentVariantIsItsBasePalette(t *testing.T) {
	for _, base := range transparentListed {
		variant := Lookup(base + transparentSuffix)
		if variant == nil {
			t.Fatalf("no transparent variant for %q", base)
		}
		src := Lookup(base)
		if src == nil {
			t.Fatalf("no base palette %q", base)
		}
		if !variant.Transparent {
			t.Errorf("%s: Transparent is false", variant.Name)
		}
		if src.Transparent {
			t.Errorf("%s: the BASE palette is marked transparent", base)
		}
		// Every colour token identical — only the flag and the name differ.
		if variant.Bg != src.Bg || variant.Surface != src.Surface || variant.SurfaceAlt != src.SurfaceAlt ||
			variant.Border != src.Border || variant.Text != src.Text || variant.Accent != src.Accent ||
			variant.Select != src.Select {
			t.Errorf("%s: the transparent variant's palette differs from its base — the tints must be the "+
				"palette the operator chose", variant.Name)
		}
	}
}

// ANY PALETTE CAN BE MADE TRANSPARENT BY NAME, which is what keeps the short picker list from being a
// limitation: the listed set is a couple per mode, and the suffix rule is the escape hatch.
func TestTheTransparentSuffixResolvesForAnyPalette(t *testing.T) {
	for _, base := range Names() {
		if strings.HasSuffix(base, transparentSuffix) {
			continue
		}
		th := Lookup(base + transparentSuffix)
		if t == nil {
			t.Errorf("%s-transparent does not resolve", base)
			continue
		}
		if th.Name != base+transparentSuffix {
			t.Errorf("%s: resolved to name %q", base, th.Name)
		}
		if !th.Transparent {
			t.Errorf("%s-transparent resolved to a non-transparent theme", base)
		}
	}
	// And an unknown base is still an error rather than a mystery theme.
	if Lookup("no-such-palette-transparent") != nil {
		t.Error("an unknown transparent name resolved to a theme")
	}
}

// EVERY TRANSPARENT THEME SAYS SO IN ITS NAME, which is what the operator asked for: "each theme that has
// it should mention it in the title". The picker prints the name, so the name is the title.
func TestEveryTransparentThemeNamesItself(t *testing.T) {
	for _, name := range Names() {
		th := Lookup(name)
		if th == nil {
			continue
		}
		if th.Transparent && !strings.Contains(name, "transparent") {
			t.Errorf("theme %q is transparent but its name does not say so", name)
		}
		if !th.Transparent && strings.Contains(name, "transparent") {
			t.Errorf("theme %q is named transparent but is not", name)
		}
	}
}

// A TRANSPARENT THEME LETS THE TERMINAL THROUGH EVERYWHERE — THAT IS THE WHOLE FEATURE.
//
// The operator, after a version that kept the panels painted so a light palette stayed readable: "now the
// transparents are not transparent at all ... only transparent in the top left corner. The terminal should bleed
// through everywhere but with the light tint of the color scheme coming through."
//
// So this test asserts the SURFACES the app would otherwise own are all unpainted: the frame background, the
// panel fill, the composer, the tab strip and both chat bands. The palette's character then comes through in
// its FOREGROUNDS — text, borders, accents — which is the "light tint of the colour scheme" that IS expressible.
//
// THE TWO FILLS THAT REMAIN ARE CONTENT, NOT SURFACES, and they are asserted as such here so the exception is
// pinned rather than accidental: the selection fill (a selection the operator cannot see is a selection they
// cannot trust) and the raised fill behind a code span / code block / diff line (a code span with no chip is not
// a code span). Both carry their own text and are gated as self-contained pairs.
func TestATransparentThemePaintsNoSurfacesAtAll(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		Use(DefaultName)
	})

	for _, name := range []string{"forest-transparent", "light-transparent", "tokyo-night-transparent"} {
		if !Use(name) {
			t.Fatalf("Use(%q) failed", name)
		}
		th := Active()
		if !th.Transparent {
			t.Fatalf("%s: Transparent flag is false", name)
		}

		// THE SURFACES: not one sequence between them.
		for label, out := range map[string]string{
			"the frame background": ScreenBg.Render("x"),
			"the panel fill":       PanelBgStyle.Render("x"),
			"the panel rows":       OpaquePanel("x", 1),
			"the composer box":     ComposerBox.Render("x"),
			"the composer repair":  ComposerBg.Render(""),
			"the tab strip":        TabBar.Render("x"),
			"the model band":       BubbleModel.Render("x"),
			"the operator band":    BubbleUser.Render("x"),
		} {
			if bgSeq(out) {
				t.Errorf("%s: %s is painted (%q) on a transparent theme — the terminal cannot bleed through",
					name, label, out)
			}
		}

		// AND THE CONTENT FILLS REMAIN, because they are the two things that stop working without a fill.
		if out := ListItemSelected.Render("x"); !bgSeq(out) {
			t.Errorf("%s: the selection fill painted nothing — a selected row would be invisible", name)
		}
		// The RAISED fill is SurfaceAlt (the code chip / code block / diff line goes through it). It is NOT
		// SurfaceBg, which is the panel fill and is unpainted with the rest of the surfaces — keeping those two
		// straight is the whole distinction this test exists to hold.
		raised := lipgloss.NewStyle().Background(SurfaceAlt).Render("x")
		if !bgSeq(raised) {
			t.Errorf("%s: the raised fill %s painted nothing — a code span would stop reading as a code span",
				name, SurfaceAlt)
		}
	}
}
