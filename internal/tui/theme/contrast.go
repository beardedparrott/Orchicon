package theme

// contrast.go — the WCAG contrast primitives the theme gates measure with.
//
// They live in the package (not only in the tests) so every palette gate —
// TestBubbleContrast, TestSelectionFillCarriesWhiteText, the pane-structure and
// border gates — measures relative luminance and contrast ratio with ONE
// implementation rather than each re-deriving it.
//
// (The chat transcript no longer needs an `ensureContrast` shift: the two
// speakers are separated by full-width background BANDS whose fills are
// contrast-gated by TestBubbleContrast, so there is no per-glyph colour to
// nudge into range.)

// relLuminance is the WCAG relative luminance of a #rrggbb colour.
func relLuminance(hex string) float64 {
	if len(hex) != 7 || hex[0] != '#' {
		return -1
	}
	lin := func(c uint8) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return ((v + 0.055) / 1.055) * ((v + 0.055) / 1.055) * 1.055
	}
	r, g, b := hexRGB(hex)
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// contrastRatio is the WCAG contrast ratio between two #rrggbb colours.
func contrastRatio(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
