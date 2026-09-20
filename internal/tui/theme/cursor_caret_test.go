package theme

// cursor_caret_test.go — THE COMPOSER'S CARET IS VISIBLE ON EVERY THEME.
//
// The operator: "The cursor in the composer should be white and not black on ALL dark themes on every page."
//
// WHAT WAS ACTUALLY WRONG, and why it looked like a colour choice rather than a bug. bubbles renders the
// visible cursor as `m.Style.Inline(true).Reverse(true).Render(char)` (cursor/cursor.go:220), so the terminal
// SWAPS the style's foreground and background before anything reaches the screen. The effective BLOCK colour is
// therefore the style's FOREGROUND, and the effective CHARACTER colour is its BACKGROUND.
//
// The dock set `Background(AccentCyan).Foreground(Bg)` — which reads as "cyan block, dark character" and
// RENDERS as "Bg block, cyan character". On the dark palettes Bg is #0c0f18, so the caret was a near-black
// block: invisible against the composer it sits in. Measured SGR: `\x1b[7;38;2;12;15;24;48;2;7;182;213m` —
// reverse first, then fg=#0c0f18, which the terminal promotes to the background.
//
// SO THE ASSERTION IS VISIBILITY, NOT A COLOUR. A test that pinned "#f8fafc" would pass while a light-theme
// caret became a white block on a white surface. This measures the caret against the SURFACE it is drawn on,
// on every palette — which is the property the operator is describing, and which the old style fails.

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// cursorColors reads the caret's PRE-REVERSE foreground and background, which are the block and the character
// respectively once bubbles reverses them.
func cursorColors(t *testing.T) (block, char string) {
	t.Helper()
	fg, ok := ComposerCursor.GetForeground().(lipgloss.Color)
	if !ok {
		t.Fatalf("ComposerCursor has no foreground, so the caret has no block colour: %T", ComposerCursor.GetForeground())
	}
	bg, ok := ComposerCursor.GetBackground().(lipgloss.Color)
	if !ok {
		t.Fatalf("ComposerCursor has no background, so the caret has no character colour: %T", ComposerCursor.GetBackground())
	}
	return string(fg), string(bg)
}

// THE CARET IS LEGIBLE AGAINST THE COMPOSER on every theme.
//
// The composer's own background is theme.Surface (see dock.styledTa), so that is what the caret has to stand
// out from. 3.0 is the bar used elsewhere in this package for a NON-TEXT element; the caret carries a glyph, so
// it is held higher than that below.
func TestComposerCaretIsVisibleOnEveryTheme(t *testing.T) {
	restore := Active().Name
	defer Use(restore)

	for _, name := range Names() {
		if !Use(name) {
			t.Fatalf("theme %q could not be activated", name)
		}
		th := Active()
		block, char := cursorColors(t)

		// The caret block must stand out from the surface it is painted on. This is the assertion that fails
		// on the old style: block = Bg on a Surface background is a ratio near 1.0.
		if r := contrastRatio(block, string(th.Surface)); r < 3.0 {
			t.Errorf("%s: the caret block (%s) has only %.2f:1 against the composer surface (%s) — the caret "+
				"is invisible, which is the operator's black cursor", name, block, r, th.Surface)
		}
		// And the character inside the block must be readable on it, or typing shows a blank block.
		if r := contrastRatio(block, char); r < 4.5 {
			t.Errorf("%s: the caret's character (%s) has only %.2f:1 on its own block (%s)", name, char, r, block)
		}
		// The block must not be the character (a zero-contrast caret is not a caret).
		if block == char {
			t.Errorf("%s: the caret's block and character are the same colour (%s)", name, block)
		}
	}
}

// ON A DARK THEME THE CARET IS THE LIGHT COLOUR — the operator's ask, stated as a property rather than a hex
// value, and checked on every dark palette rather than the default one.
func TestComposerCaretIsLightOnDarkThemes(t *testing.T) {
	restore := Active().Name
	defer Use(restore)

	checked := 0
	for _, name := range Names() {
		if !Use(name) {
			continue
		}
		th := Active()
		// "Dark theme" is measured, not named: its surface is darker than its body text.
		if relLuminance(string(th.Bg)) >= relLuminance(string(th.Text)) {
			continue // a light palette
		}
		checked++
		block, _ := cursorColors(t)
		if block != string(th.Text) {
			t.Errorf("%s: on a DARK palette the caret block is %s, want the palette's light text colour %s — "+
				"this is the operator's \"should be white and not black\"", name, block, th.Text)
		}
	}
	if checked == 0 {
		t.Fatal("no dark palette was found in the registry, so this test asserted nothing — the luminance " +
			"test for \"dark\" needs revisiting")
	}
	t.Logf("checked %d dark palette(s)", checked)
}

// AND ON A LIGHT THEME IT IS THE DARK COLOUR. The fix must not turn a light palette's caret into a white block
// on a white surface, which a naive "always white" reading of the request would have done.
func TestComposerCaretIsDarkOnLightThemes(t *testing.T) {
	restore := Active().Name
	defer Use(restore)

	checked := 0
	for _, name := range Names() {
		if !Use(name) {
			continue
		}
		th := Active()
		if relLuminance(string(th.Bg)) < relLuminance(string(th.Text)) {
			continue // a dark palette
		}
		checked++
		block, _ := cursorColors(t)
		if block != string(th.Text) {
			t.Errorf("%s: on a LIGHT palette the caret block is %s, want the palette's dark text colour %s",
				name, block, th.Text)
		}
	}
	if checked == 0 {
		t.Fatal("no light palette was found in the registry")
	}
	t.Logf("checked %d light palette(s)", checked)
}
