package theme

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// transparent_adapt.go — how a TRANSPARENT theme stays legible while it gets out of the way.
//
// THE PROBLEM IT SOLVES, and it took three rounds with the operator to state properly. A transparent theme
// leaves the background UNPAINTED so the terminal shows through — and the moment it does, the palette's
// colours are being drawn on a surface the palette knows nothing about: the terminal's own background. A
// palette built for a near-white page is dark text, and on a dark terminal that is dark text on black. The
// operator, seeing exactly that: "the light transparent themes are almost impossible to see/read."
//
// THE FIX IS NOT TO RE-TINT THE PANELS — that is what breaks the transparency the theme exists for, and it is
// what the operator caught: "now the transparents are not transparent at all ... the terminal should bleed
// through everywhere." A panel that paints a fill is a panel that stops the bleed-through, so the answer has to
// be the FOREGROUNDS, not the fills.
//
// So a transparent theme is adapted at `Use()` time against the TERMINAL's own background:
//
//   - WHAT GROUND IS IT? lipgloss/termenv asks the terminal (OSC 11), falls back to $COLORFGBG, and assumes
//     DARK when neither answers — the same default every TUI uses, and the right one for the majority.
//     $ORCHICON_TERMINAL_BG overrides it, for the terminals that answer neither question truthfully (a
//     transparent terminal's idea of "its" background is exactly the thing the operator has configured away).
//   - WHAT DOES IT MEASURE AGAINST? The WORST CASE for that ground: pure black or pure white. A real terminal
//     background is almost never at the extreme, so anything that clears the floor against the extreme clears it
//     against the real thing — the adaptation is conservative by construction rather than by luck.
//   - WHAT MOVES? Every FOREGROUND token, each nudged toward the readable end until it clears the same floor the
//     theme gates already hold the solid palettes to. A token that already passes is left BYTE-IDENTICAL, so a
//     dark transparent theme on a dark terminal renders exactly as it did before this file existed.
//   - WHAT STAYS? The FILLED elements — the selection fill, the code chip / code block / diff fills. A selection
//     the operator cannot see is a selection they cannot trust, and a code span that is not on a chip is not a
//     code span: those are content, not surfaces, and they must keep a fill to function. They are self-contained
//     (their text is chosen against them, already gated), so the terminal under them does not matter.

// SetTerminalBgEnv names the override for the detected ground.
//
// IT EXISTS FOR THE TERMINALS THAT CANNOT ANSWER. A transparent terminal has, by definition, been configured so
// that its "background colour" is not what is behind it — so the OSC reply is either absent or a lie, and the
// operator is the only reliable source. "light" or "dark"; anything else is treated as unset.
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

// PrimeTerminalGround asks the terminal for its background ONCE, at a point where nothing else is reading stdin.
//
// The query writes an escape sequence and reads the reply from the tty, so it must not happen while bubbletea is
// draining that same tty — it would race for the bytes. `theme.Use` is the natural place to need the answer, and
// a session that STARTS on a solid palette and switches to a transparent one later would otherwise trigger the
// query mid-session. Priming it in NewApp (before tea.NewProgram) puts the one query in the safe window, and
// termenv caches the answer for the life of the process, so every later call is free.
func PrimeTerminalGround() { _ = terminalGroundHex() }

// readableOn returns c nudged AWAY from bg until it clears floor, or c unchanged when it already clears it.
//
// "Away" means toward white on a dark ground and toward black on a light one — the direction that raises
// contrast — and the blend preserves the palette's own hue for as long as the floor allows rather than
// substituting a flat white or black. That is the same trade the message bands make (see bubbleText), applied
// to the ground the terminal supplies.
func readableOn(c, bg string, floor float64) string {
	return readableOnAll(c, []string{bg}, floor)
}

// readableOnAll nudges c until it clears floor against EVERY target, and returns c untouched when it already
// does.
//
// IT TAKES SEVERAL TARGETS BECAUSE A TRANSPARENT THEME DRAWS ON MORE THAN THE TERMINAL. The ground is the
// terminal's background, but the code chip, the code block and the diff lines draw on the RAISED fill, which sits
// a little away from the ground — so a text colour chosen to clear the floor against the ground alone can sit at
// 3.9:1 on the chip above it. Measured exactly that on the light palettes, whose chip fill lifts toward their
// accent and swallows a near-white foreground.
//
// The floor is therefore met against the HARDEST of the targets, which is the same conservative direction the
// ground itself is chosen in (see terminalGroundHex).
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

// adaptTransparentForTerminal returns a copy of a transparent palette whose FOREGROUNDS are legible on the
// terminal's own background. Its fills are untouched.
//
// The copy's Bg and Surface are set to the GROUND, which is not a colour anything paints (both are left
// unpainted — see buildStyles) but IS what every contrast gate measures against. That is the point: the gates
// iterate the registry and read the ACTIVE palette, so reporting the real ground here means they measure the
// pairing the operator actually sees, with no special case in any gate.
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
	// THE RAISED FILL STAYS PAINTED, and it is a lift AWAY FROM THE GROUND rather than a blend toward the accent.
	// It has to be: a code span, a code block and a diff line all draw through it, and it must read as a raised
	// block on whatever the terminal is — while staying close enough to the ground that the text above it can
	// still clear its floor against BOTH. Blending the palette's accent in (which is what the first version did)
	// lifted a LIGHT palette's chip toward white on a dark terminal, and the adapted near-white body text landed
	// on it at 3.94:1. The measured failure is why this is a neutral lift, and why the text below is measured
	// against it as well as against the ground.
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

	// Select is deliberately NOT adapted: it is a fill that carries white text, it is gated against that
	// (TestSelectionFillCarriesWhiteText), and its whole job is to be unmistakably different from whatever is
	// behind it — which the operator wants to be able to see.
	return &cp
}
