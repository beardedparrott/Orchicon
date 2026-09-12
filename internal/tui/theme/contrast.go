package theme

// contrast.go — runtime contrast helpers.
//
// These live in the package (not only in tests) because palette derivation needs
// them at BUILD time: an accent is chosen for borders and fills, where roughly
// 3:1 suffices, but chat PROSE needs body-text contrast. On several light
// palettes the raw accent sat near 3.1:1 against the background, so the accent
// is shifted toward the palette's text until it clears the prose floor
// (ensureContrast) rather than being hard-coded per theme.

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

// ensureContrast shifts want toward the palette's text colour until it reaches
// target contrast on bg. The text colour is already chosen against the
// background, so the blend always converges.
func ensureContrast(want, bg string, target float64) string {
	cur := want
	for i := 0; i < 8; i++ {
		if contrastRatio(cur, bg) >= target {
			return cur
		}
		// Toward white on a dark page, toward black on a light one — i.e. away
		// from the background, which is what raises contrast in either mode.
		dir := "#ffffff"
		if relLuminance(bg) >= 0.5 {
			dir = "#000000"
		}
		cur = hexBlend(cur, dir, 0.25)
	}
	return cur
}
