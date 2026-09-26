package input

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
)

// translate is the pure translation under test, expressed over a string so the
// cases read as input -> output.
func translate(in string) string {
	p := []byte(in)
	n := translateShiftEnter(p)
	return string(p[:n])
}

// TestShiftEnterBecomesTheNewlineChord is the whole point: the three bytes a
// terminal sends for Shift+Enter must come out as the alt+enter chord, because
// that is what the composer maps to insertNewline. Before this, bubbletea split
// the same three bytes into alt+O and M and the composer typed "OM".
func TestShiftEnterBecomesTheNewlineChord(t *testing.T) {
	if got, want := translate("\x1bOM"), "\x1b\r"; got != want {
		t.Fatalf("Shift+Enter = %q, want the newline chord %q", got, want)
	}
}

// TestOrdinaryInputIsUntouched guards the rewrite against being a blunt
// instrument: text, plain Enter, alt+enter, and a lone Escape must all pass
// through verbatim.
func TestOrdinaryInputIsUntouched(t *testing.T) {
	for _, in := range []string{
		"hello world",
		"\r",            // plain Enter
		"\x1b\r",        // alt+enter (already the chord)
		"\x1b",          // a lone Escape press — must NOT be withheld
		"\x1bO",         // ESC O with nothing after it: not the sequence
		"OM",            // the literal letters, no ESC
		"multi\nline",   //
		"\x1b[27;2;13~", // modifyOtherKeys form: not ours to rewrite
		"\x1b[13;2u",    // CSI-u form: not ours to rewrite
	} {
		if got := translate(in); got != in {
			t.Errorf("input %q was rewritten to %q; it must pass through unchanged", in, got)
		}
	}
}

// TestRepeatedAndEmbeddedSequences covers the realistic cases: several
// Shift+Enter presses in one read, and the sequence surrounded by typed text.
func TestRepeatedAndEmbeddedSequences(t *testing.T) {
	if got, want := translate("a\x1bOMb\x1bOM\x1bOMc"), "a\x1b\rb\x1b\r\x1b\rc"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestLengthShrinksByTheNumberOfMatches pins the buffer arithmetic: the
// replacement is shorter than the match, so the returned length must be the
// SHRUNKEN one. Reporting the original length would hand the caller stale
// trailing bytes — io.Reader's one hard contract.
func TestLengthShrinksByTheNumberOfMatches(t *testing.T) {
	p := []byte("\x1bOM\x1bOM\x1bOM")
	if n := translateShiftEnter(p); n != 6 {
		t.Fatalf("three sequences (9 bytes) should report 6 bytes, got %d", n)
	}
	if got := string(p[:6]); got != "\x1b\r\x1b\r\x1b\r" {
		t.Fatalf("got %q", got)
	}
}

// TestReadTranslatesThroughARealFile drives the decorator the way the program
// does: a real file, read in chunks, with the sequence straddling nothing. It
// also proves the decorated value is still a *os.File underneath, which is the
// raw-mode contract.
func TestReadTranslatesThroughARealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte("hi\x1bOMthere"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	wrapped := ShiftEnterToNewline(f)

	// The contract bubbletea checks before it will put the terminal in raw
	// mode. If this ever stops holding, keys echo and the TUI is unusable.
	if _, ok := interface{}(wrapped).(term.File); !ok {
		t.Fatal("the decorated input no longer satisfies term.File — bubbletea would not enter raw mode")
	}
	if wrapped.Fd() != f.Fd() {
		t.Fatal("Fd() must be the real terminal fd, not a copy")
	}

	out, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "hi\x1b\rthere"; got != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

// TestSequenceSplitAcrossReadsIsNotTranslated documents the stated limit rather
// than pretending it does not exist. A trailing ESC is NOT held back (that would
// delay or strand a real Escape keypress, which is worse), so a split sequence
// passes through as the two keys bubbletea would have made of it anyway.
func TestSequenceSplitAcrossReadsIsNotTranslated(t *testing.T) {
	// Two independent calls stand in for two Reads: the translator is
	// stateless by design.
	if got := translate("\x1b"); got != "\x1b" {
		t.Fatalf("first chunk must pass through, got %q", got)
	}
	if got := translate("OM"); !strings.HasPrefix(got, "OM") {
		t.Fatalf("second chunk must pass through, got %q", got)
	}
}
