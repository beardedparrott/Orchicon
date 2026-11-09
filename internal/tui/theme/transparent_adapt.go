package theme

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// transparent_adapt.go — how a TRANSPARENT theme stays legible while it gets out of the way.

// SetTerminalBgEnv names the override for the detected ground. IT EXISTS FOR THE TERMINALS THAT
// CANNOT ANSWER.
const SetTerminalBgEnv = "ORCHICON_TERMINAL_BG"

// terminalGroundHex is the background a transparent theme's foregrounds are measured against, as the WORST CASE
// for the terminal's own lightness: pure black or pure white.
func terminalGroundHex() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SetTerminalBgEnv))) {
	case "light":
		return "#ffffff"
	case "dark":
		return "#000000"
	}
	if lipgloss.HasDarkBackground() {
		return "#000000"
	}
	return "#ffffff"
}

// PrimeTerminalGround asks the terminal for its background ONCE, at a point where nothing else is
// reading stdin.
func PrimeTerminalGround() { _ = terminalGroundHex() }

// readableOn returns c nudged AWAY from bg until it clears floor, or c unchanged when it already
// clears it.
func readableOn(c, bg string, floor float64) string {
	return readableOnAll(c, []string{bg}, floor)
}

// readableOnAll nudges c until it clears floor against EVERY target, and returns c untouched when
// it already does.
func readableOnAll(c string, targets []string, floor float64) string {
	clears := func(v string) bool {
		for _, bg := range targets {
			if contrastRatio(v, bg) < floor {
				return false
			}
		}
		return true
	}
	if clears(c) {
		return c
	}
	// The direction is set by the FIRST target — the ground — since it is the one the eye reads the text
	// against, and every raised fill is on the same side of it.
	target := "#ffffff"
	if len(targets) > 0 && !isDarkHex(targets[0]) {
		target = "#000000"
	}
	for k := 0.05; k <= 1.0; k += 0.05 {
		cand := hexBlend(c, target, k)
		if clears(cand) {
			return cand
		}
	}
	return target
}

// adaptTransparentForTerminal returns a copy of a transparent palette whose FOREGROUNDS are legible
// on the terminal's own background.
func adaptTransparentForTerminal(src *Theme) *Theme {
	ground := terminalGroundHex()
	dark := isDarkHex(ground)

	// The floors are the ones the existing gates hold every palette to, and they move with the ground exactly as
	// they do for the solid palettes: a LIGHT ground needs 4.5:1 for status text, a dark one 3.0:1.
	statusFloor := 3.0
	if !dark {
		statusFloor = 4.5
	}

	cp := *src
	cp.Bg = lipgloss.Color(ground)
	cp.Surface = lipgloss.Color(ground)
	// THE RAISED FILL STAYS PAINTED, and it is a lift AWAY FROM THE GROUND rather than a blend toward
	// the accent.
	lift := "#ffffff"
	if !dark {
		lift = "#000000"
	}
	cp.SurfaceAlt = lipgloss.Color(hexBlend(ground, lift, 0.10))

	// Body text clears the floor against the ground AND the raised fill it is also drawn on.
	cp.Text = lipgloss.Color(readableOnAll(string(src.Text), []string{ground, string(cp.SurfaceAlt)}, 4.5))
	cp.TextDim = lipgloss.Color(readableOn(string(src.TextDim), ground, 3.0))
	cp.TextFaint = lipgloss.Color(readableOn(string(src.TextFaint), ground, 3.0))
	cp.Border = lipgloss.Color(readableOn(string(src.Border), ground, 1.8))
	cp.BorderFaint = lipgloss.Color(readableOn(string(src.BorderFaint), ground, 1.4))

	// The accent family draws text (headings, menu titles, reasoning labels), so it is held to the status floor
	// rather than to the structural one.
	cp.Accent = lipgloss.Color(readableOn(string(src.Accent), ground, statusFloor))
	cp.AccentCyan = lipgloss.Color(readableOn(string(src.AccentCyan), ground, statusFloor))
	cp.AccentIndigo = lipgloss.Color(readableOn(string(src.AccentIndigo), ground, statusFloor))

	cp.OK = lipgloss.Color(readableOn(string(src.OK), ground, statusFloor))
	cp.Warn = lipgloss.Color(readableOn(string(src.Warn), ground, statusFloor))
	cp.Err = lipgloss.Color(readableOn(string(src.Err), ground, statusFloor))
	cp.Busy = lipgloss.Color(readableOn(string(src.Busy), ground, statusFloor))
	cp.Tool = lipgloss.Color(readableOn(string(src.Tool), ground, statusFloor))

	// Select is deliberately NOT adapted: it is a fill that carries white text, it is gated against
	// that (TestSelectionFillCarriesWhiteText).
	return &cp
}
