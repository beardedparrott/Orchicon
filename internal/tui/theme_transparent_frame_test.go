package tui

// theme_transparent_frame_test.go — THE WHOLE FRAME RESPECTS A TRANSPARENT THEME.
//
// The theme package's own tests prove the STYLES are right. This proves the FRAME agrees, which is a
// different question: the shell wraps every row in a base style and then runs two repairs (padScreenLine's
// bgOpaque and the composer's RepairAfterResets) whose entire job is to RE-PAINT the background after any
// inner reset. A transparency that the styles honoured and the repairs undid would look correct in the
// theme tests and paint solid cells on the real screen — so the assertion has to be made against a
// rendered frame, not against a style.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// paintSeq returns the exact SGR a colour renders as, ASKED OF THE RENDERER rather than computed.
//
// Hand-deriving it from the hex was wrong and worth recording: termenv converts a #rrggbb through a
// float round-trip and truncates (`int(v*255)`), so #1f7a5c renders as `48;2;31;121;92` — green 121, not
// the 122 the hex says. A helper that computed the sequence itself therefore searched for a string that is
// never emitted, and reported a present tint as missing. Deriving it from the same renderer the frame uses
// removes the whole class of disagreement.
func paintSeq(c lipgloss.TerminalColor) string {
	out := lipgloss.NewStyle().Background(c).Render("x")
	i := strings.IndexByte(out, 'x')
	if i < 0 {
		return ""
	}
	return out[:i]
}

// A TRANSPARENT SESSION PAINTS NO APP BACKGROUND, WHILE ITS TINTS SURVIVE.
//
// BOTH DIRECTIONS ARE ASSERTED. Absence alone would pass on a frame that had simply stopped painting —
// indistinguishable from a broken theme, and shippable as "transparency works" while every panel,
// selection fill and bubble had quietly become bare text on the operator's wallpaper.
func TestATransparentSessionPaintsNoAppBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	m := phase3App(120, 40)
	if !m.SetTheme("forest-transparent") {
		t.Fatal("SetTheme(forest-transparent) failed")
	}
	m.dock.SetValue("hello")
	m.refreshLayout()

	base := theme.Lookup("forest")
	if base == nil {
		t.Fatal("fixture: the forest palette is missing")
	}
	frame := m.baseView(120, 40)

	// The app background's sequence is what WOULD be painted for this palette — the active theme leaves it
	// unpainted, so asking the renderer for it is the only way to search for "the colour that must not
	// appear".
	if appBg := paintSeq(base.Bg); appBg != "" && strings.Contains(frame, appBg) {
		t.Errorf("the app background %s (%q) appears in a TRANSPARENT session's frame — cells are being "+
			"painted over the terminal, which is what the theme exists to prevent", base.Bg, appBg)
	}
	// And the tints the operator confirmed they want are still there: the panel surface, which every pane
	// on screen carries.
	if panel := paintSeq(base.Surface); panel != "" && !strings.Contains(frame, panel) {
		t.Errorf("the panel surface tint %s (%q) is MISSING from a transparent session's frame — panels "+
			"would have no fill at all", base.Surface, panel)
	}
}

// A SOLID SESSION IS UNCHANGED — the mechanism must not have made every theme transparent.
func TestASolidSessionStillPaintsItsBackground(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	m := phase3App(120, 40)
	if !m.SetTheme("forest") {
		t.Fatal("SetTheme(forest) failed")
	}
	base := theme.Lookup("forest")
	if appBg := paintSeq(base.Bg); appBg != "" && !strings.Contains(m.baseView(120, 40), appBg) {
		t.Errorf("a SOLID session no longer paints its background %s", base.Bg)
	}
}

// SWITCHING FROM A TRANSPARENT THEME BACK TO A SOLID ONE RE-PAINTS THE FRAME IMMEDIATELY.
//
// This is the stateful half, and it is the shape of the operator's earlier report ("when switching from a
// dark to light theme it may be keeping the composer black"): a style captured or cached at one palette
// and read at another. The frame is rendered after each switch rather than only inspected in the model.
func TestSwitchingOutOfATransparentThemeRepaintsTheFrame(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	m := phase3App(120, 40)
	if !m.SetTheme("forest-transparent") {
		t.Fatal("SetTheme(forest-transparent) failed")
	}
	m.dock.SetValue("hello")
	appBg := paintSeq(theme.Lookup("forest").Bg)
	if appBg == "" {
		t.Fatal("fixture: could not resolve the app background's sequence")
	}
	if strings.Contains(m.baseView(120, 40), appBg) {
		t.Fatal("fixture: the transparent theme painted the app background")
	}

	if !m.SetTheme("forest") {
		t.Fatal("SetTheme(forest) failed")
	}
	if frame := m.baseView(120, 40); !strings.Contains(frame, appBg) {
		t.Error("after switching back to a SOLID theme the frame still has no app background — the " +
			"transparency leaked into the next theme")
	}
}
