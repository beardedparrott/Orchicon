package main

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The true-colour advertisement, and the one place it goes missing.
//
// WHY THIS IS HERE AND NOT IN A WINDOWS LAUNCHER. There is no native Windows orch
// binary to set it in — release.yml ships linux/darwin only, deliberately, because
// the runtime layer is POSIX-only — so `orch` on Windows IS the Linux binary,
// running inside the WSL2 distro that scripts/install.ps1 installs into. And a
// Windows-side environment variable would not reliably cross that boundary anyway:
// WSL relays a specific set of variables and requires WSLENV to relay more.
//
// SO THE FIX IS IN-PROCESS, and it SUPPLIES AN ADVERTISEMENT rather than forcing a
// profile. That distinction is the whole reason this is safe where the removed
// hand-rolled override was not (see terminalColorProfile below): we set COLORTERM
// and then let termenv's own detection decide, so NO_COLOR still wins (EnvColorProfile
// checks EnvNoColor first), the GNU screen degradation still applies, and the value
// is lowercased the same way termenv lowercases it.
//
// WHAT ACTUALLY GOES WRONG, measured on this repo's own palettes. The xterm-256
// cube's first non-zero level is 95/255 (~L37%), so its only dark entries are
// GREYS: every dark ground in every theme — forest's #0e1511 (L7), teal's #152322
// (L11) — quantises to grey 233/234, which is black. So below true colour a dark
// ground cannot be tinted AT ANY LIGHTNESS and the palette is not the thing to
// change. Windows Terminal supports 24-bit and does not reliably advertise it to a
// WSL shell, so the profile lands on ANSI256 and the theme looks broken.
const colorTermEnv = "COLORTERM"

// wslEnvMarkers are the variables WSL sets in the distro's own environment. Any
// one of them means we are inside WSL.
var wslEnvMarkers = []string{"WSL_DISTRO_NAME", "WSL_INTEROP"}

// TrueColorOptOut disables the WSL true-colour default. It exists because this
// code decides something on the operator's behalf, and a decision with no way to
// contradict it is the kind of help that becomes an obstacle. Any other value
// (or unset) leaves the default in place.
const TrueColorOptOut = "ORCHICON_TRUECOLOR"

// needsWSLTrueColor reports whether the app should supply the true-colour
// advertisement its terminal failed to relay.
//
// EVERY GUARD IS LOAD-BEARING:
//
//   - tty — no terminal means no colour to get right, and this matches
//     disableAutoWrap, which already leaves non-TTY stdout untouched. termenv would
//     return Ascii first regardless, so this only keeps us from mutating the
//     environment of a piped run.
//   - COLORTERM already set — the terminal advertised after all, and its word is
//     better than our inference. This is also the escape hatch: COLORTERM=yes pins
//     ANSI256, because termenv maps yes/true to ANSI256.
//   - TERM=dumb — a terminal that has told us it can do nothing.
//   - the opt-out — see TrueColorOptOut.
//   - a WSL marker — the actual condition. Without it we are on a platform whose
//     advertisement we have no reason to distrust, and termenv is simply correct.
func needsWSLTrueColor(tty bool, getenv func(string) string) bool {
	if !tty {
		return false
	}
	if strings.TrimSpace(getenv(colorTermEnv)) != "" {
		return false
	}
	if strings.TrimSpace(getenv("TERM")) == "dumb" {
		return false
	}
	if strings.TrimSpace(getenv(TrueColorOptOut)) == "0" {
		return false
	}
	for _, marker := range wslEnvMarkers {
		if strings.TrimSpace(getenv(marker)) != "" {
			return true
		}
	}
	return false
}

// applyWSLTrueColorDefault supplies the advertisement when WSL is running us in a
// terminal that can take 24-bit colour but did not say so. Called from main BEFORE
// applyTerminalColorProfile, because termenv reads the environment at
// NewOutput/EnvColorProfile time (its environ is a live view of os.Getenv, and the
// resolution in applyTerminalColorProfile runs immediately after).
func applyWSLTrueColorDefault() {
	if v := wslTrueColorDefault(stdoutIsTTY(), os.Getenv); v != "" {
		_ = os.Setenv(colorTermEnv, v)
	}
}

// wslTrueColorDefault returns the COLORTERM value to supply, or "" to leave the
// environment alone. Splitting the DECISION out of the applier is what makes the
// whole chain assertable: the tests drive this with a described terminal and then
// feed the result through the REAL termenv resolution, so what is pinned is the
// outcome — 'WSL terminal that did not advertise' reaches TrueColor — rather than
// the shape of an if-statement.
func wslTrueColorDefault(tty bool, getenv func(string) string) string {
	if needsWSLTrueColor(tty, getenv) {
		return "truecolor"
	}
	return ""
}

// stdoutIsTTY reports whether stdout is a terminal, which is the same fact
// termenv's own isTTY() check consults first.
func stdoutIsTTY() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// terminalColorProfile is how much colour the app paints with: whatever the TERMINAL advertises it can
// take.
//
// IT DELEGATES TO termenv, AND THAT IS THE WHOLE FIX. This used to be a hand-rolled one-liner at the top of
// main —
//
//	if os.Getenv("COLORTERM") == "truecolor" || os.Getenv("COLORTERM") == "24bit" {
//		lipgloss.SetColorProfile(termenv.TrueColor)
//	}
//
// — which is a subset of a detection termenv already performs and lipgloss already consults by default. A
// subset is not merely redundant here: it was WRONG in three ways, and each one made the screen worse than
// leaving it alone.
//
//   - CASE. termenv lowercases COLORTERM before matching, so `COLORTERM=TrueColor` resolves. The exact
//     equality above did not match it, so a terminal advertising 24-bit in mixed case got no override.
//
//   - screen. termenv deliberately degrades truecolor to ANSI256 under GNU `screen` (distinguishing it from
//     tmux via TERM_PROGRAM) because screen cannot render 24-bit. Forcing TrueColor there MANGLES every
//     colour — the override could only ever produce a worse picture than the detection it was shadowing.
//
//   - NO_COLOR / CLICOLOR=0. Setting an explicit profile bypasses EnvColorProfile's no-colour handling
//     entirely, so an operator who had asked for no colour got it back. (termenv's default path is
//     EnvColorProfile precisely so those variables are honoured.)
//
// And it could not IMPROVE anything in exchange: in every case where it forced TrueColor, termenv's
// detection already returns TrueColor. So the decision is now termenv's, taken once here rather than
// re-derived by hand, and cmd/orch/colorprofile_test.go pins the cases above.
//
// WHAT termenv SEES, for the record: COLORTERM (24bit/truecolor/yes/true), TERM (the kitty, alacritty,
// wezterm, ghostty, contour and rio names, plus xterm/linux/256color), TERM_PROGRAM (tmux vs screen),
// NO_COLOR and CLICOLOR/CLICOLOR_FORCE — and whether stdout is a terminal at all. That last one is worth
// stating because it is a deliberate consequence: with no terminal on stdout the profile is Ascii, so
// `orch` piped into a file writes no escape sequences. That matches disableAutoWrap, which already leaves
// non-TTY stdout untouched for the same reason.
func terminalColorProfile(out *termenv.Output) termenv.Profile {
	return out.EnvColorProfile()
}

// applyTerminalColorProfile resolves the profile from the real stdout and pins it on lipgloss, which every
// style in the TUI renders through. Called once, from main, before anything is drawn.
func applyTerminalColorProfile() {
	lipgloss.SetColorProfile(terminalColorProfile(termenv.NewOutput(os.Stdout)))
}
