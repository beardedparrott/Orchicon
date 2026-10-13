// Package input adapts the bytes a terminal sends into the key vocabulary the
// TUI's composer understands.
//
// THIS PACKAGE EXISTS BECAUSE THE TERMINAL'S VOCABULARY IS WIDER THAN THE LIBRARY'S,
// and the failure mode is silent rather than loud: a sequence bubbletea cannot decode
// is not rejected, it is read as ORDINARY CHARACTERS, so the symptom is litter typed
// into whatever field has focus. Two spellings have needed adapting so far, both
// legacy encodings that are entirely standard — Shift+Enter, and the SS3 form of
// Home/End.
package input

import (
	"bytes"
	"os"

	"github.com/charmbracelet/x/term"
)

// TranslateTerminalKeys decorates the program's INPUT file, translating every byte
// sequence this package knows about as it is read. It is what cmd/orch hands to
// tea.WithInput.
//
// IT IS ONE DECORATOR RATHER THAN ONE PER REWRITE, because chaining them would nest a
// Read inside a Read per rewrite and each would have to restate the raw-mode contract
// below — and that contract guards a failure that is silent, so it belongs in exactly
// one place.
//
// WHY TRANSLATE RATHER THAN SWALLOW, for every rewrite here: a dropped sequence stops
// the litter but leaves the key DEAD, whereas rewriting it to a spelling the library
// already understands makes the key DO what every other editor makes it do. None of
// them invents key vocabulary — each targets a mapping that already exists (the
// composer's insertNewline; bubbletea's own Home/End).
//
// WHY IT TAKES AND RETURNS A *os.File. bubbletea decides whether it may put the
// terminal into raw mode by asserting that input satisfies term.File
// (io.ReadWriteCloser + Fd) — term/tty_unix.go's initInput:
//
//	if f, ok := p.input.(term.File); ok && term.IsTerminal(f.Fd()) {
//		p.previousTtyInputState, err = term.MakeRaw(p.ttyInput.Fd())
//
// A plain io.Reader wrapper fails that assertion, so the terminal would never enter
// raw mode: every keystroke would echo and the TUI would be unusable. The decorator
// therefore EMBEDS the file, so Write/Close/Fd are still the real ones and only Read
// is ours. The compile-time assertion below pins that.
//
// LIMITS, stated rather than hidden:
//   - the rewrite is per Read, so a sequence split across two Reads is not translated.
//     A keypress is written by the terminal in one go, so this does not arise in
//     practice, and the alternative — holding a trailing ESC back for the next Read —
//     would delay or strand a genuine Escape keypress, which is a worse failure than a
//     missed rewrite.
//   - a genuine alt+O IMMEDIATELY followed by M, H or F within one Read is
//     byte-for-byte identical to keypad-Enter, SS3 Home and SS3 End respectively, and
//     is therefore rewritten too. There is no way to tell them apart at this layer;
//     alt+O followed by one of those three letters is not a thing a person types at a
//     shell prompt.
func TranslateTerminalKeys(f *os.File) *keyFile { return &keyFile{File: f} }

// keyFile decorates a terminal input file, translating the sequences below as it is
// read. Embedding *os.File is what keeps the raw-mode contract intact (see
// TranslateTerminalKeys); only Read is overridden.
type keyFile struct {
	*os.File
}

// The raw-mode contract, asserted rather than assumed: if this stops holding,
// bubbletea silently leaves the terminal in cooked mode and the TUI breaks in a way
// that is hard to trace back here.
var _ term.File = (*keyFile)(nil)

func (f *keyFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		n = translateTerminalKeys(p[:n])
	}
	return n, err
}

// translateTerminalKeys applies every rewrite, IN PLACE in the caller's read buffer,
// and returns the new length.
//
// ORDER: the Shift+Enter rewrite runs FIRST because it is the only one that CHANGES
// the length (it shortens), so every later rewrite indexes into a buffer whose extent
// is already settled. The rewrites cannot feed each other: one emits ESC CR and the
// other ESC [ <letter>, and neither is a prefix of any sequence either one matches.
//
// Pure and byte-slice-only, so the translation is testable without a TTY.
func translateTerminalKeys(p []byte) int {
	n := translateShiftEnter(p)
	return translateSS3HomeEnd(p[:n])
}

// translateShiftEnter rewrites the byte sequence a terminal sends for Shift+Enter into
// the chord the composer already maps to "insert a newline" (ESC CR).
//
// WHY THIS IS NEEDED. bubbletea v1.3.10 has neither a Shift+Enter key nor Kitty
// keyboard-protocol support, and it does not know the legacy keypad-Enter sequence
// either. Given the three bytes Konsole sends, it decodes TWO keys — alt+O and then M —
// and the composer dutifully inserts the literal text "OM". Verified against the
// pinned library:
//
//	"\x1bOM" -> runes "alt+O" | runes "M"      (the bug: "OM" appears)
//	"\x1b\r" -> enter "alt+enter"              (the newline chord)
//
// The target is the alt+enter chord, which the composer's own hint documents as the
// newline key ("enter send · alt+enter newline") and which is the default NewlineMode.
// A profile configured for backslash-enter keeps that chord too; this only ADDS
// Shift+Enter.
//
// The replacement is shorter than the match, so the result always fits and the length
// only ever shrinks — which is what the caller must report (io.Reader's one hard
// contract).
func translateShiftEnter(p []byte) int {
	if !bytes.Contains(p, shiftEnterSequence) {
		return len(p)
	}
	return copy(p, bytes.ReplaceAll(p, shiftEnterSequence, newlineChordBytes))
}

var (
	// shiftEnterSequence is what Konsole 26.08 sends for Shift+Enter: the legacy
	// keypad-Enter encoding (ESC O M), which is also what other terminals using the
	// DEC application-keypad encoding send.
	shiftEnterSequence = []byte{0x1b, 'O', 'M'}
	// newlineChordBytes is alt+enter (ESC CR).
	newlineChordBytes = []byte{0x1b, '\r'}
)

// ss3KeyRewrites are the SS3 (ESC O <char>) sequences bubbletea v1.3.10 does NOT
// decode, each paired with the CSI spelling of the SAME KEY that it does.
//
// WHY THIS IS NEEDED, and why the bug is so specific. The SS3 family is
// HALF-implemented upstream: bubbletea decodes the application-cursor arrows
// (ESC O A-D -> Up/Down/Right/Left) and F1-F4 (ESC O P-S), and it decodes the CSI
// spellings of Home and End (ESC [ H, ESC [ F, ESC [ 1~ , ESC [ 4~) — but not the SS3
// spellings of Home and End. So a keyboard whose Home/End key emits SS3 (a laptop's
// fn+Left/Right, or any terminal left in application-cursor mode) hits the one hole in
// an otherwise complete table.
//
// MEASURED, not inferred. Driving the real binary in a real pty and writing the raw
// bytes: "\x1bOH" left the composer holding the literal text "OH", and "\x1bOF" left it
// holding "OF". bubbletea decodes ESC+O as alt+O and then the letter as an ordinary
// rune, and the composer inserts both, ignoring the Alt. That is the operator's report
// exactly: "when I do it inside the composer I get the following OH and OF."
//
// EVERY PAIR IS THE SAME LENGTH (3 bytes -> 3 bytes), which is what lets the rewrite
// happen in the caller's read buffer. A pair that GREW would need a buffering reader
// and is deliberately not expressed here.
//
// THESE ARE DELIBERATELY ABSENT — bubbletea already decodes them, and rewriting them
// would BREAK the keys:
//
//	ESC O A .. ESC O D   the application-cursor arrows
//	ESC O P .. ESC O S   F1 .. F4
//	ESC O M              keypad Enter — handled above, as the newline chord
//
// A GENERAL "ESC O ?" -> "ESC [ ?" RULE IS THEREFORE WRONG, and the F-keys are why:
// ESC O P is F1, while ESC [ P is not a key at all. The table is explicit for the same
// reason it is small.
var ss3KeyRewrites = [][2]string{
	{"\x1bOH", "\x1b[H"}, // Home
	{"\x1bOF", "\x1b[F"}, // End
}

// translateSS3HomeEnd rewrites the SS3 Home/End spellings to their CSI equivalents IN
// PLACE and returns the new length, which is always unchanged — every pair in
// ss3KeyRewrites is the same length. That invariant is pinned by
// TestSS3RewritesNeverChangeLength, because if it stopped holding, the copy below would
// silently TRUNCATE the read buffer and eat whatever followed the keypress.
func translateSS3HomeEnd(p []byte) int {
	n := len(p)
	for _, rw := range ss3KeyRewrites {
		from, to := []byte(rw[0]), []byte(rw[1])
		if !bytes.Contains(p[:n], from) {
			continue
		}
		copy(p, bytes.ReplaceAll(p[:n], from, to))
	}
	return n
}
