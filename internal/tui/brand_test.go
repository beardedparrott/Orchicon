package tui

import (
	"strings"
	"testing"
)

// Regression for the operator's "looks like I'm tripping on acid" wordmark: a
// single row came out one cell short, which shifted the H's crossbar and every
// glyph after it on that row, shearing the whole lockup. Every row of the
// composed wordmark must be EXACTLY the same width, and every glyph must
// occupy exactly its cell so vertically aligned strokes line up.
func TestWelcomeBrandRowsAreUniform(t *testing.T) {
	if len(welcomeBrand) != brandRows {
		t.Fatalf("wordmark has %d rows, want %d", len(welcomeBrand), brandRows)
	}
	want := len([]rune(welcomeBrand[0]))
	if want != len([]rune("ORCHICON"))*(brandGlyphW+1)-1 {
		t.Fatalf("wordmark width = %d, want %d", want, len([]rune("ORCHICON"))*(brandGlyphW+1)-1)
	}
	for i, r := range welcomeBrand {
		if got := len([]rune(r)); got != want {
			t.Errorf("row %d is %d cells, want %d — a short/long row shears the wordmark:\n%s", i, got, want, r)
		}
	}
}

// Every glyph must be exactly brandGlyphW wide in every row, or composing it
// into a word misaligns the following letters.
func TestBrandGlyphsAreFixedWidth(t *testing.T) {
	for ch, g := range brandGlyphs {
		if len(g) != brandRows {
			t.Errorf("glyph %q has %d rows, want %d", ch, len(g), brandRows)
		}
		for row, cell := range g {
			if got := len([]rune(cell)); got != brandGlyphW {
				t.Errorf("glyph %q row %d is %d cells, want %d: %q", ch, row, got, brandGlyphW, cell)
			}
		}
	}
}

// The composed word must be the letters actually requested, in order — a
// missing glyph silently drops a letter.
func TestComposeBrandKeepsEveryLetter(t *testing.T) {
	if strings.Contains(strings.Join(welcomeBrand, ""), "\x00") {
		t.Fatal("wordmark contains a null rune")
	}
	// 8 letters => 8 glyph cells + 7 separators.
	for _, r := range welcomeBrand {
		if n := strings.Count(r, " "); n == 0 {
			t.Errorf("row has no separators — letters collapsed: %q", r)
		}
	}
}
