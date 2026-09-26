// Package input adapts the bytes a terminal sends into the key vocabulary the
// TUI's composer understands.
package input

import (
	"bytes"
	"os"

	"github.com/charmbracelet/x/term"
)

// ShiftEnterToNewline rewrites the byte sequence a terminal sends for Shift+Enter
// into the chord the composer already maps to "insert a newline".
//
// WHY THIS IS NEEDED. bubbletea v1.3.10 has neither a Shift+Enter key nor Kitty
// keyboard-protocol support, and it does not know the legacy keypad-Enter
// sequence either. Given the three bytes Konsole sends, it decodes TWO keys —
// alt+O and then M — and the composer dutifully inserts the literal text "OM".
// Verified against the pinned library:
//
//	"\x1bOM" -> runes "alt+O" | runes "M"      (the bug: "OM" appears)
//	"\x1b\r" -> enter "alt+enter"              (the newline chord)
//
// WHY TRANSLATE RATHER THAN SWALLOW. Dropping the sequence would stop the "OM"
// litter but leave Shift+Enter a dead key. Rewriting it to the newline chord
// makes the key DO what every other editor makes it do, using a mapping the
// composer already has (dock.Model.insertNewline) so there is no second code
// path to keep in step.
//
// The target is the alt+enter chord, which the composer's own hint documents as
// the newline key ("enter send · alt+enter newline") and which is the default
// NewlineMode. A profile configured for backslash-enter keeps that chord too;
// this only ADDS Shift+Enter.
//
// WHY IT TAKES AND RETURNS A *os.File. This wraps the program's INPUT, and
// bubbletea decides whether it may put the terminal into raw mode by asserting
// that input satisfies term.File (io.ReadWriteCloser + Fd) — term/tty_unix.go's
// initInput:
//
//	if f, ok := p.input.(term.File); ok && term.IsTerminal(f.Fd()) {
//		p.previousTtyInputState, err = term.MakeRaw(p.ttyInput.Fd())
//
// A plain io.Reader wrapper fails that assertion, so the terminal would never
// enter raw mode: every keystroke would echo and the TUI would be unusable. The
// decorator therefore EMBEDS the file, so Write/Close/Fd are still the real
// ones and only Read is ours. The compile-time assertion below pins that.
//
// LIMITS, stated rather than hidden:
//   - the rewrite is per Read, so a sequence split across two Reads is not
//     translated. A keypress is written by the terminal in one go, so this does
//     not arise in practice, and the alternative — holding a trailing ESC back
//     for the next Read — would delay or strand a genuine Escape keypress,
//     which is a worse failure than a missed rewrite.
//   - a genuine alt+shift+O IMMEDIATELY followed by M within one Read is
//     byte-for-byte identical to Shift+Enter and is therefore rewritten too.
//     There is no way to tell them apart at this layer; the pairing is not a
//     thing a person types.
func ShiftEnterToNewline(f *os.File) *shiftEnterFile { return &shiftEnterFile{File: f} }

var (
	// shiftEnterSequence is what Konsole 26.08 sends for Shift+Enter: the
	// legacy keypad-Enter encoding (ESC O M), which is also what other
	// terminals using the DEC application-keypad encoding send.
	shiftEnterSequence = []byte{0x1b, 'O', 'M'}
	// newlineChordBytes is alt+enter (ESC CR).
	newlineChordBytes = []byte{0x1b, '\r'}
)

// shiftEnterFile decorates a terminal input file, translating Shift+Enter as it
// is read. Embedding *os.File is what keeps the raw-mode contract intact (see
// ShiftEnterToNewline); only Read is overridden.
type shiftEnterFile struct {
	*os.File
}

// The raw-mode contract, asserted rather than assumed: if this stops holding,
// bubbletea silently leaves the terminal in cooked mode and the TUI breaks in a
// way that is hard to trace back here.
var _ term.File = (*shiftEnterFile)(nil)

func (f *shiftEnterFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		n = translateShiftEnter(p[:n])
	}
	return n, err
}

// translateShiftEnter rewrites the Shift+Enter sequence to the newline chord IN
// PLACE and returns the new length. The replacement is shorter than the match,
// so the result always fits and the length only ever shrinks — which is what the
// caller must report (io.Reader's one hard contract).
//
// Pure and byte-slice-only, so the translation is testable without a TTY.
func translateShiftEnter(p []byte) int {
	if !bytes.Contains(p, shiftEnterSequence) {
		return len(p)
	}
	return copy(p, bytes.ReplaceAll(p, shiftEnterSequence, newlineChordBytes))
}
