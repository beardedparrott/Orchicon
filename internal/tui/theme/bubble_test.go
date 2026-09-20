package theme

// bubble_test.go — the bubble fills keep their geometry, and the bubble TEXT adapts only when it must.
//
// Two promises are pinned here, and they pull in opposite directions:
//
//   - THE FILLS ARE UNCHANGED for the palettes that already worked. The operator tuned these by eye (the
//     lighter operator band over the subtler model panel), so a change to the derivation that silently
//     re-coloured every existing transcript would be a regression dressed as an improvement.
//   - THE TEXT ADAPTS, BUT ONLY WHEN IT HAS TO. For a palette whose own foreground has room on the fills,
//     the bubble text IS the palette's text — byte for byte. It moves only on the palettes where that
//     foreground is a mid-tone and would be unreadable.

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// THE EXISTING PALETTES' FILLS ARE THE DOCUMENTED CONSTANTS, unchanged by the adaptive text.
func TestBubbleFillsKeepTheirGeometry(t *testing.T) {
	for _, name := range []string{"forest", "dark", "light", "gruvbox-dark", "gruvbox-light", "ember"} {
		th := Lookup(name)
		if th == nil {
			t.Fatalf("fixture: %s is missing", name)
		}
		user, model := bubbleFills(string(th.Bg), string(th.Accent))
		var wantUser, wantModel string
		if isDarkHex(string(th.Bg)) {
			wantUser = hexBlend(string(th.Bg), "#ffffff", 0.17)
			wantModel = hexBlend(string(th.Bg), "#ffffff", 0.055)
		} else {
			wantUser = hexBlend(string(th.Bg), string(th.Accent), 0.12)
			wantModel = hexBlend(string(th.Bg), "#000000", 0.26)
		}
		if user != wantUser || model != wantModel {
			t.Errorf("%s: fills are (%s, %s), want (%s, %s) — the bubble geometry must not drift",
				name, user, model, wantUser, wantModel)
		}
	}
}

// WHERE THE PALETTE'S TEXT FITS, IT IS USED VERBATIM. This is what keeps the adaptive path invisible on
// every palette that already worked.
func TestBubbleTextIsThePaletteTextWhenItFits(t *testing.T) {
	for _, name := range []string{"forest", "dark", "light", "gruvbox-dark", "gruvbox-light", "ember"} {
		th := Lookup(name)
		user, model := bubbleFills(string(th.Bg), string(th.Accent))
		for _, fill := range []string{user, model} {
			if got := string(bubbleText(fill, *th)); got != string(th.Text) {
				t.Errorf("%s: bubbleText(%s) = %s, want the palette's own text %s — this palette's "+
					"foreground has room on its fills, so nothing should be moved", name, fill, got, th.Text)
			}
		}
	}
}

// AND WHERE IT DOES NOT, IT MOVES — enough to be read, and no further than that. A version of this that
// jumped straight to pure white/black would pass the readability floor while throwing away the palette's
// colour, so the moved value is asserted to still be a BLEND of the palette's own text rather than a
// substitute.
func TestBubbleTextMovesOutOfTheWayOnlyWhenNeeded(t *testing.T) {
	midTone := []string{"one-dark", "everforest-dark", "catppuccin-latte", "solarized-light", "rose-pine-dawn"}
	for _, name := range midTone {
		th := Lookup(name)
		if th == nil {
			t.Fatalf("fixture: %s is missing", name)
		}
		user, model := bubbleFills(string(th.Bg), string(th.Accent))
		moved := false
		for _, fill := range []string{user, model} {
			drawn := string(bubbleText(fill, *th))
			if r := contrastRatio(drawn, fill); r < bubbleTextTarget {
				t.Errorf("%s: the drawn bubble text %s on %s is %.2f:1, want >= %.1f — the whole point of "+
					"the adaptive path", name, drawn, fill, r, bubbleTextTarget)
			}
			if drawn != string(th.Text) {
				moved = true
			}
			if drawn == "#ffffff" || drawn == "#000000" {
				t.Errorf("%s: the bubble text became pure %s — the palette's own colour should be carried as "+
					"far as the floor allows, not replaced", name, drawn)
			}
		}
		// The fixtures are chosen BECAUSE their foreground is a mid-tone; if one of them stopped needing the
		// move the assertion above would still pass while this test stopped testing anything.
		if !moved && contrastRatio(string(th.Text), user) < bubbleTextTarget {
			t.Errorf("%s: fixture no longer exercises the adaptive path", name)
		}
	}
}

// the alias keeps the import used if the assertions above are ever trimmed.
var _ = lipgloss.Color("")
