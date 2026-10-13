package input

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
)

// translate is the pure translation under test, expressed over a string so the
// cases read as input -> output.
func translate(in string) string {
	p := []byte(in)
	n := translateShiftEnter(p)
	return string(p[:n])
}

// translateAll is the COMPOSED translation under test (every rewrite the decorator
// applies), over a string so the cases read as input -> output.
func translateAll(in string) string {
	p := []byte(in)
	n := translateTerminalKeys(p)
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

	wrapped := TranslateTerminalKeys(f)

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

// ---------------------------------------------------------------- the library cross-check

// bubbleteaDecodes reports what the PINNED bubbletea makes of a raw byte sequence,
// driving a real tea.Program over an in-memory reader.
//
// WHY THIS IS NOT A TABLE OF OUR OWN BELIEFS. The whole fix rests on a claim about the
// library — that its SS3 support is half-implemented, decoding the arrow keys and F1-F4
// but not Home and End. A test that asserted that claim by consulting ss3KeyRewrites
// would be circular. This asks the library itself, so if bubbletea ever completes its
// table the rewrite stops being necessary and these tests say so out loud instead of
// silently rotting.
func bubbleteaDecodes(seq string) bool {
	return firstKeyOf(seq) != ""
}

// firstKeyOf returns the String() of the FIRST key the library decodes from seq, or ""
// if it decodes none. It returns as soon as one key arrives, so it never has to reason
// about how the library splits a sequence it does not recognise.
func firstKeyOf(seq string) string {
	kc := &keyCapture{got: make(chan string, 1)}
	prog := tea.NewProgram(
		kc,
		tea.WithInput(strings.NewReader(seq)),
		tea.WithoutRenderer(),
		tea.WithoutSignalHandler(),
	)
	go func() { _, _ = prog.Run() }()
	select {
	case k := <-kc.got:
		return k
	case <-time.After(5 * time.Second):
		// No key at all: the sequence produced nothing the library could decode.
		return ""
	}
}

// keyCapture records the first key it is handed and quits the program.
type keyCapture struct {
	got chan string
}

func (k *keyCapture) Init() tea.Cmd { return nil }

func (k *keyCapture) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		select {
		case k.got <- km.String():
		default:
		}
		return k, tea.Quit
	}
	return k, nil
}

func (k *keyCapture) View() string { return "" }

// TestBubbleteaDecodesTheCSIHomeEndSpellings is the premise check for the rewrite's
// TARGET: the sequences it emits must be sequences the library actually maps.
func TestBubbleteaDecodesTheCSIHomeEndSpellings(t *testing.T) {
	for seq, want := range map[string]string{"\x1b[H": "home", "\x1b[F": "end"} {
		if got := firstKeyOf(seq); got != want {
			t.Errorf("bubbletea decodes %q as %q, want %q — the SS3 rewrite targets a dead sequence",
				seq, got, want)
		}
	}
}

// TestBubbleteaDecodesTheSS3SpellingsItAlreadyHandles is the premise check for the
// rewrite's EXCLUSIONS.
func TestBubbleteaDecodesTheSS3SpellingsItAlreadyHandles(t *testing.T) {
	for seq, want := range map[string]string{
		"\x1bOA": "up", "\x1bOB": "down", "\x1bOC": "right", "\x1bOD": "left",
		"\x1bOP": "f1", "\x1bOQ": "f2", "\x1bOR": "f3", "\x1bOS": "f4",
	} {
		if got := firstKeyOf(seq); got != want {
			t.Errorf("bubbletea decodes %q as %q, want %q — this package must leave it alone",
				seq, got, want)
		}
	}
}

// TestBubbleteaSplitsTheSS3HomeEndSpellingIntoTwoKeys IS THE BUG, demonstrated against
// the real library rather than described: the SS3 Home/End spelling does not arrive as
// one unknown key, it arrives as alt+O and then an ordinary letter — which is why the
// symptom is the literal text "OH" / "OF" typed into the composer.
func TestBubbleteaSplitsTheSS3HomeEndSpellingIntoTwoKeys(t *testing.T) {
	// The library's own casing: "alt+O", with a capital O.
	for seq, want := range map[string]string{"\x1bOH": "alt+O", "\x1bOF": "alt+O"} {
		if got := firstKeyOf(seq); got != want {
			t.Errorf("bubbletea's first key for %q is %q, want %q — the reported litter is "+
				"alt+O followed by the letter, so these sequences must NOT be left untranslated",
				seq, got, want)
		}
	}
}
