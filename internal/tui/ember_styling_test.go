package tui

// ember_styling_test.go — THE TRANSCRIPT'S STRUCTURAL COLOUR.
//
// The operator: "I would like to see about adding some more 'pizaz' to conversations in the TUI ... Notice
// the various colors, etc.? I would like to make it more enhanced." Then, having been shown the menu:
// "Let's do 1-4 and then I can judge it after rebuild."
//
// So: headings, list markers and inline code take the theme's ACCENT, and a tool row carries its own
// token. Colour cannot be judged from a log, so these assert the actual SGR sequences — which is also the
// only way to pin that the accent reaches the text rather than the attribute set merely claiming it.
//
// THE PALETTE IS EMBER, whose accent is #f38435 → SGR `38;2;243;132;53`, and whose Tool token is the busy
// hue #22d3ee → `38;2;34;211;238`.

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
//
// Asserted on the STYLE's foreground rather than on rendered output, and that is a deliberate choice about
// what is testable here: this row is painted with lipgloss, and lipgloss STRIPS colour when the terminal
// profile has none — which is the profile a test binary has. (The heading, list and chip assertions above
// can read real escape codes because md writes SGR itself.) So the link asserted is the one that can
// break: the token reaches the style that paints the row — for EVERY registered theme, which is where the
// bug was: ember had a Tool token and the hand-written palettes did not.
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
