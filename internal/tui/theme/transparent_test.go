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

// THE APP BACKGROUND IS NOT PAINTED, AND THE TINTS ARE. The two halves in one test, because a version of
// this that dropped the background AND the surfaces would pass a one-sided check while destroying the
// feature the operator confirmed they wanted.
func TestATransparentThemePaintsNoBackgroundButKeepsTints(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		Use(DefaultName)
	})

	for _, name := range []string{"forest-transparent", "light-transparent"} {
		if !Use(name) {
			t.Fatalf("Use(%q) failed", name)
		}
		th := Active()
		if !th.Transparent {
			t.Fatalf("%s: Transparent flag is false", name)
		}

		// (1) THE APP BACKGROUND: no sequence at all, so the terminal shows through.
		if out := ScreenBg.Render("x"); bgSeq(out) {
			t.Errorf("%s: ScreenBg emitted a background (%q) — the frame would paint over the terminal "+
				"instead of showing it", name, out)
		}
		// Nor the app-background COLOUR by any other route (the panel borders and the tab bar paint
		// through theme.Bg, so this catches a site that bypassed ScreenBg).
		if want := rgbOf(t, string(th.Bg)); strings.Contains(ScreenBg.Render("x"), want) {
			t.Errorf("%s: the app background %s is still being painted somewhere", name, th.Bg)
		}
		// (2) THE TINTS REMAIN — this is the half that makes it a theme rather than bare text.
		if out := SurfaceBg.Render("x"); !bgSeq(out) {
			t.Errorf("%s: SurfaceBg painted nothing — panels would lose their tint", name)
		}
		if out := ListItemSelected.Render("x"); !bgSeq(out) {
			t.Errorf("%s: the selection fill painted nothing", name)
		}
		if out := BubbleModel.Render("x"); !bgSeq(out) {
			t.Errorf("%s: the model bubble painted nothing — a transparent theme must still separate "+
				"the two speakers", name)
		}
	}
}

// THE COMPOSER FOLLOWS THE BACKGROUND, NOT THE PANELS. It is the one fill the operator asked to be
// transparent too, which is why it has a token of its own.
func TestTheComposerIsTransparentWhilePanelsAreNot(t *testing.T) {
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
	if out := SurfaceBg.Render("x"); !bgSeq(out) {
		t.Error("the panel surface lost its tint on a transparent theme")
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
