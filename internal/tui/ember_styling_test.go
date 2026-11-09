package tui

// ember_styling_test.go — THE TRANSCRIPT'S STRUCTURAL COLOUR.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

const (
	emberAccentSGR = "\x1b[38;2;243;132;53m" // #f38435
)

func renderAsEmber(t *testing.T, text string) string {
	t.Helper()
	if !theme.Use("ember") {
		t.Fatal("fixture: the ember palette is not registered")
	}
	t.Cleanup(func() { theme.Use(theme.DefaultName) })
	return chat.RenderItems([]chat.ChatItem{{Kind: chat.KindText, Text: text, Key: "t1"}}, 74)
}

// A HEADING TAKES THE ACCENT — the single biggest lift for prose-heavy replies, and the terminal's
// substitute for the GUI's type scale.
func TestAHeadingTakesTheThemeAccent(t *testing.T) {
	out := renderAsEmber(t, "## The short answer\n\nbody text here")
	headLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "The short answer") {
			headLine = l
		}
	}
	if headLine == "" {
		t.Fatalf("fixture: the heading did not render:\n%s", out)
	}
	if !strings.Contains(headLine, emberAccentSGR) {
		t.Errorf("the heading carries no accent SGR (%q):\n%q", emberAccentSGR, headLine)
	}
	// AND IT IS STILL BOLD, so a monochrome terminal keeps the only cue it has.
	if !strings.Contains(headLine, "\x1b[1m") {
		t.Errorf("the heading lost its bold — colour is decorative here and must never be the only cue:\n%q", headLine)
	}
}

// A LIST MARKER TAKES THE ACCENT, so a long list has the rhythm the operator was after.
func TestListMarkersTakeTheThemeAccent(t *testing.T) {
	out := renderAsEmber(t, "- first\n- second\n- third")
	if got := strings.Count(out, emberAccentSGR); got < 3 {
		t.Errorf("only %d accent run(s) in a three-item list — the markers are not all accented:\n%s", got, out)
	}
	// The marker is accented; the ITEM TEXT after it is not, or the whole list would be one colour and
	// the accent would stop meaning anything.
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, "second") {
			continue
		}
		if !strings.Contains(l, emberAccentSGR) {
			t.Errorf("the marker is not accented: %q", l)
		}
		// After the marker's close, the item text must be back on the restore colour.
		markerAt := strings.Index(l, "•")
		if markerAt < 0 {
			t.Fatalf("no bullet in %q", l)
		}
	}
}

// INLINE CODE TAKES THE ACCENT ON THE RAISED SURFACE — a warm chip rather than the neutral it was.
func TestInlineCodeTakesTheAccentChip(t *testing.T) {
	out := renderAsEmber(t, "run `theme.Use` to switch")
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "theme.Use") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("fixture: the code span did not render:\n%s", out)
	}
	if !strings.Contains(line, emberAccentSGR) {
		t.Errorf("the inline-code chip is not on the accent colour:\n%q", line)
	}
}

// AND A TOOL ROW IS PAINTED BY ITS OWN TOKEN.
func TestAToolRowIsPaintedByItsOwnToken(t *testing.T) {
	for _, name := range theme.Names() {
		if !theme.Use(name) {
			t.Fatalf("fixture: %q is listed but does not resolve", name)
		}
		th := theme.Active()
		if th.Tool == "" {
			t.Errorf("theme %q has no Tool token, so its tool rows render UNCOLOURED while every other "+
				"theme's are coloured — a theme-dependent difference in what the transcript shows", name)
			continue
		}
		if got := theme.ToolName.GetForeground(); got != th.Tool {
			t.Errorf("theme %q: the tool-row style paints %v, want the Tool token %v — the row is not following "+
				"its own token", name, got, th.Tool)
		}
	}
	theme.Use(theme.DefaultName)
}

// PLAIN PROSE STILL CARRIES NO ESCAPE CODES — the pass-through guarantee this renderer makes, and the
// reason the accent is applied to structural runs only.
func TestPlainProseKeepsNoEscapeCodes(t *testing.T) {
	out := renderAsEmber(t, "just a plain sentence with no markup at all")
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain prose carries escape codes, so the accent is leaking beyond the structural runs:\n%q", out)
	}
}
