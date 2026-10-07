package tui

// mouse_leak.go — AN ORPHANED MOUSE REPORT MUST NOT BE TYPED INTO THE COMPOSER.
//
// The operator, resizing the terminal: "the resize seems a bit buggy and adds the following into the
// conversation box when resizing: [<32;27;30M[<32;96;29M[<32;117;29M[<32;113;30M".
//
// THAT STRING IS NOT TEXT. `ESC [ < b ; x ; y M` is an SGR mouse report — `b` encodes button and modifiers
// (32 = motion with the left button held, which is exactly what a window drag produces), and x/y are the
// cell the pointer is over. The leaked text is those reports WITH THE ESC MISSING.
//
// WHY THE ESC GOES MISSING. A terminal emits a mouse report as ONE escape sequence, but bubbletea's input
// reader decides whether a bare ESC begins a sequence using a short timeout, and when input is arriving in
// bursts — which is what a resize is, motion reports streaming faster than the reader flushes — the ESC is
// emitted on its own as a KeyEsc and the REST arrives as ordinary runes. bubbletea cannot put them back
// together, so the tail has to be recognized HERE or it becomes text in whatever field has the focus.
//
// WHY THIS IS A DROP AND NOT A FILTER OF "ODD-LOOKING" INPUT. The shape is unambiguous: a human does not
// type `[<32;27;30M`, and the pattern is anchored end-to-end. Anything that does not match exactly — a real
// `[`, a stray `<`, a half-typed bracket expression — is left alone, which is what keeps this from eating
// legitimate input in a textarea or a form field.
//
// NOT COVERED, deliberately: the legacy X10 encoding (`ESC [ M` followed by three RAW bytes, no
// separators). Those bytes can be any value, so a detector would have to guess at arbitrary input — and it
// is not the encoding in play here: bubbletea enables SGR (1006) alongside mouse tracking, and the report
// above is SGR.

import (
	"regexp"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// sgrMouseReport matches an SGR mouse report EXACTLY, without its ESC: the `<` marks the SGR form, the
// three numbers are button+modifiers, column and row, and the final byte is `M` (press/motion) or `m`
// (release). Anchored, so a fragment that merely contains one is not dropped.
var sgrMouseReport = regexp.MustCompile(`^\[<\d+;\d+;\d+[Mm]$`)

// orphanedMouseReport reports whether a key message is the ESC-less tail of a mouse report.
//
// A partial tail (the sequence split again mid-report) is matched too, as long as every character belongs to
// the report's alphabet and it starts with `[<` — a split that leaves `[<32;27` still must not be typed.
func orphanedMouseReport(k tea.KeyMsg) bool {
	if k.Type != tea.KeyRunes || len(k.Runes) < 2 {
		return false
	}
	s := string(k.Runes)
	if !strings.HasPrefix(s, "[<") {
		return false
	}
	if sgrMouseReport.MatchString(s) {
		return true
	}
	// A TRUNCATED report: only the characters a report can be made of, so a textarea's `[<` followed by a
	// letter or space is untouched.
	if len(s) < 3 {
		return false
	}
	for _, r := range s[2:] {
		if (r < '0' || r > '9') && r != ';' && r != 'M' && r != 'm' {
			return false
		}
	}
	return true
}

// dropOrphanedMouseReport consumes the tail of a split mouse report, reporting whether the message was one.
//
// It is called from dispatch, the ONE funnel every message passes through, so a report cannot reach the
// composer, a form field, or any screen's key handling.
func (m *App) dropOrphanedMouseReport(msg tea.Msg) bool {
	k, ok := msg.(tea.KeyMsg)
	return ok && orphanedMouseReport(k)
}
