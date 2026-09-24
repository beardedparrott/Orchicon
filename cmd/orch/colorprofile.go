package main

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

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
