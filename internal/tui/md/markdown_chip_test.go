package md

// markdown_chip_test.go — the inline-code chip: real theme colours instead of reverse video, drawn
// so it can be closed inside a host background band without leaving a hole.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// forceColor makes lipgloss emit real escape sequences.
func forceColor(t *testing.T) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

// TestCodeChipUsesThemeColoursAndRestoresTheSurface is the core contract: the span is drawn in the
// chip's colours, and its close returns to the SURFACE rather than the terminal default.
func TestCodeChipUsesThemeColoursAndRestoresTheSurface(t *testing.T) {
	forceColor(t)
	// Distinctive values so each assertion names exactly what it found.
	const (
		chipFg = "#010203"
		chipBg = "#040506"
		sfFg   = "#070809"
		sfBg   = "#0a0b0c"
	)
	SetCodeChip(chipFg, chipBg)
	t.Cleanup(func() { SetCodeChip("", "") })

	out := RenderOnString("Run `go test` now.", 60, Surface{Fg: sfFg, Bg: sfBg})

	// 1. The chip's own colours are applied.
	if !strings.Contains(out, "38;2;1;2;3") {
		t.Fatalf("the chip foreground is missing from %q", out)
	}
	if !strings.Contains(out, "48;2;4;5;6") {
		t.Fatalf("the chip background is missing from %q", out)
	}

	// 2. The close RESTORES the surface. The chip emits its fg then bg on the way out, so the
	//    assertion takes everything after the code text rather than just the last sequence.
	idx := strings.Index(out, " now.")
	if idx < 0 {
		t.Fatalf("the text after the chip is missing from %q", out)
	}
	afterCode := out[strings.Index(out, "go test"):idx]
	if !strings.Contains(afterCode, "48;2;10;11;12") {
		t.Errorf("the chip closes with %q, which does not restore the surface background (48;2;10;11;12) "+
			"— the rest of the line would render on the wrong background", afterCode)
	}
	if !strings.Contains(afterCode, "38;2;7;8;9") {
		t.Errorf("the chip closes with %q, which does not restore the surface foreground", afterCode)
	}

	// 3. AND IT NEVER RESETS TO THE TERMINAL DEFAULT. `\x1b[49m` is the measured band hole:
	//    band → chip → 49 → the remainder of the line loses the band's background.
	if strings.Contains(out, "\x1b[49m") || strings.Contains(out, "\x1b[39m") {
		t.Errorf("the chip resets to the terminal defaults instead of restoring the surface: %q", out)
	}
	// 4. Styling was actually emitted, so none of the above is vacuous.
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("no escape sequences at all — the colour profile was not forced and every assertion " +
			"above is vacuous")
	}
}

// WITHOUT A DECLARED SURFACE THE CHIP IS OFF, and a code span keeps rendering as reverse video.
func TestCodeChipIsOffWithoutASurface(t *testing.T) {
	forceColor(t)
	SetCodeChip("#010203", "#040506")
	t.Cleanup(func() { SetCodeChip("", "") })

	out := RenderString("Run `go test` now.", 60)

	if strings.Contains(out, "48;2;4;5;6") {
		t.Errorf("a chip was drawn with no declared surface: %q", out)
	}
	if !strings.Contains(out, "\x1b[7m") {
		t.Errorf("a code span with no surface should fall back to reverse video (SGR 7): %q", out)
	}
	if !strings.Contains(out, "\x1b[27m") {
		t.Errorf("reverse video must be closed with its targeted off-code (27): %q", out)
	}
}

// BOLD INSIDE A CHIP SURVIVES. The chip is stripped out of the attribute set before rendering, so the
// rest of the set still applies — a code span inside emphasis must be both.
func TestCodeChipComposesWithOtherAttributes(t *testing.T) {
	forceColor(t)
	SetCodeChip("#010203", "#040506")
	t.Cleanup(func() { SetCodeChip("", "") })

	out := RenderOnString("**bold `code` here**", 60, Surface{Fg: "#070809", Bg: "#0a0b0c"})

	if !strings.Contains(out, "48;2;4;5;6") {
		t.Errorf("the chip was not drawn inside bold text: %q", out)
	}
	if !strings.Contains(out, "\x1b[1m") {
		t.Errorf("bold was lost when it contained a code span: %q", out)
	}
	// The chip must not carry the reverse-video codes as well.
	if strings.Contains(out, "\x1b[7m") {
		t.Errorf("a chipped span also emitted reverse video, so the two fight: %q", out)
	}
}

// A MALFORMED CHIP COLOUR FALLS BACK TO REVERSE VIDEO rather than drawing half a chip.
func TestCodeChipFallsBackOnAMalformedColour(t *testing.T) {
	forceColor(t)
	SetCodeChip("not-a-colour", "#040506")
	t.Cleanup(func() { SetCodeChip("", "") })

	out := RenderOnString("Run `go test` now.", 60, Surface{Fg: "#070809", Bg: "#0a0b0c"})

	if strings.Contains(out, "48;2;4;5;6") {
		t.Errorf("a chip background was drawn without a usable foreground, leaving text on an "+
			"unchecked fill: %q", out)
	}
	if !strings.Contains(out, "\x1b[7m") {
		t.Errorf("a malformed chip colour should fall back to reverse video: %q", out)
	}
	if !strings.Contains(out, "go test") {
		t.Errorf("the code text went missing: %q", out)
	}
}
