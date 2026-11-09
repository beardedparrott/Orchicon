package tui

// composer_theme_follow_test.go — THE COMPOSER IS ALWAYS PAINTED FOR THE ACTIVE PALETTE.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// composerLines returns the frame rows that belong to the composer box.
func composerLines(t *testing.T, m *App) []string {
	t.Helper()
	m.dock.SetValue("hello")
	var out []string
	for _, r := range strings.Split(m.baseView(m.width, m.height), "\n") {
		if strings.Contains(r, "╭") || strings.Contains(r, "╰") || strings.Contains(r, "❯") {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		t.Fatal("fixture: no composer rows found in the frame")
	}
	return out
}

// THE FILL FOLLOWS A DARK → LIGHT SWITCH. This is the operator's exact report.
func TestTheComposerFillFollowsADarkToLightSwitch(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	dark := theme.Lookup("forest")
	light := theme.Lookup("catppuccin-latte")
	if dark == nil || light == nil {
		t.Fatal("fixture: palettes missing")
	}
	darkFill, lightFill := paintSeq(dark.Surface), paintSeq(light.Surface)
	if darkFill == "" || lightFill == "" || darkFill == lightFill {
		t.Fatalf("fixture: the two palettes' surfaces do not differ (%q / %q)", darkFill, lightFill)
	}

	m := phase3App(120, 40)
	if !m.SetTheme("forest") {
		t.Fatal("SetTheme(forest) failed")
	}
	rows := strings.Join(composerLines(t, m), "\n")
	if !strings.Contains(rows, darkFill) {
		t.Fatalf("fixture: the DARK composer is not painted with its palette's surface %q", darkFill)
	}

	if !m.SetTheme("catppuccin-latte") {
		t.Fatal("SetTheme(catppuccin-latte) failed")
	}
	rows = strings.Join(composerLines(t, m), "\n")
	if !strings.Contains(rows, lightFill) {
		t.Errorf("after switching to a LIGHT theme the composer is not painted with that palette's surface "+
			"%q — this is the operator's \"it may be keeping the composer black\"", lightFill)
	}
	if strings.Contains(rows, darkFill) {
		t.Errorf("the composer still carries the PREVIOUS (dark) palette's surface %q after switching to a "+
			"light theme", darkFill)
	}
}

// AND THE REVERSE DIRECTION, because a one-way test would pass on a re-pin that happened to coincide with
// the first switch.
func TestTheComposerFillFollowsALightToDarkSwitch(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	light := theme.Lookup("catppuccin-latte")
	dark := theme.Lookup("tokyo-night")
	lightFill, darkFill := paintSeq(light.Surface), paintSeq(dark.Surface)

	m := phase3App(120, 40)
	if !m.SetTheme("catppuccin-latte") {
		t.Fatal("SetTheme(catppuccin-latte) failed")
	}
	if rows := strings.Join(composerLines(t, m), "\n"); !strings.Contains(rows, lightFill) {
		t.Fatalf("fixture: the LIGHT composer is not painted with its palette's surface %q", lightFill)
	}

	if !m.SetTheme("tokyo-night") {
		t.Fatal("SetTheme(tokyo-night) failed")
	}
	rows := strings.Join(composerLines(t, m), "\n")
	if !strings.Contains(rows, darkFill) {
		t.Errorf("after switching to a DARK theme the composer is not painted with that palette's surface %q",
			darkFill)
	}
	if strings.Contains(rows, lightFill) {
		t.Errorf("the composer still carries the previous (light) palette's surface %q", lightFill)
	}
}

// THE COMPOSER'S OWN FILL IS THE THEME'S, NOT A HARDCODED ONE — asserted against the palette rather than
// against a literal, so it cannot pass on a composer that is simply painted something.
func TestTheComposerUsesTheActivePalettesFill(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	for _, name := range []string{"forest", "lumen", "dracula", "solarized-light"} {
		if !theme.Use(name) {
			t.Fatalf("Use(%q) failed", name)
		}
		// SetTheme, NOT theme.Use: NewApp applies the PROFILE's theme at construction, so a Use() before it
		// would be overwritten and this test would render every palette as forest while claiming to check it.
		m := phase3App(120, 40)
		if !m.SetTheme(name) {
			t.Fatalf("SetTheme(%q) failed", name)
		}
		th := theme.Active()
		rows := strings.Join(composerLines(t, m), "\n")
		if want := paintSeq(th.Surface); want != "" && !strings.Contains(rows, want) {
			t.Errorf("%s: the composer is not painted with its palette's surface %q", name, want)
		}
		// And the text on it is the palette's own foreground, so a fill/text pair cannot be mixed. The
		// FOREGROUND sequence, not the background one — that distinction is the whole point of the check.
		if want := fgSeq(th.Text); want != "" && !strings.Contains(rows, want) {
			t.Errorf("%s: the composer carries no text in the palette's foreground %q", name, want)
		}
	}
}

// fgSeq is paintSeq's foreground twin: the exact SGR a colour renders as when used for TEXT. It asks the
// renderer for the sequence rather than computing it (see paintSeq for why that matters).
func fgSeq(c lipgloss.TerminalColor) string {
	out := lipgloss.NewStyle().Foreground(c).Render("x")
	i := strings.IndexByte(out, 'x')
	if i < 0 {
		return ""
	}
	return out[:i]
}

// THE COMPOSER IS PAINTED ONLY IN THE ACTIVE PALETTE'S COLOURS.
func TestTheComposerIsPaintedOnlyInTheActivePalettesColours(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	for _, name := range []string{"catppuccin-latte", "tokyo-night", "solarized-light", "dracula", "github-light"} {
		m := phase3App(120, 40)
		if !m.SetTheme(name) {
			t.Fatalf("SetTheme(%q) failed", name)
		}
		assertComposerPaintsOnlyTheActivePalette(t, m, name)
	}
}

// bgSeqsIn returns every BACKGROUND sequence in a rendered row, in order.
func bgSeqsIn(s string) []string {
	var out []string
	for i := 0; i+3 <= len(s); i++ {
		if s[i:i+3] != "48;" {
			continue
		}
		if i > 0 && s[i-1] != ';' && s[i-1] != '[' {
			continue // part of a larger numeric parameter, not a sequence of its own
		}
		j := i
		for j < len(s) && s[j] != 'm' {
			j++
		}
		if j < len(s) {
			out = append(out, s[i:j+1])
			i = j
		}
	}
	return out
}

// assertComposerPaintsOnlyTheActivePalette is the shared form of the invariant above, for tests
// that reach a palette switch by a DIFFERENT route (the Themes pane.
func assertComposerPaintsOnlyTheActivePalette(t *testing.T, m *App, where string) {
	t.Helper()
	th := theme.Active()
	userFill, modelFill := theme.BubbleFills()
	allowed := map[string]bool{}
	for _, c := range []lipgloss.TerminalColor{
		th.Bg, th.Surface, th.SurfaceAlt, th.Select,
		lipgloss.Color(userFill), lipgloss.Color(modelFill),
	} {
		if seq := strings.TrimPrefix(paintSeq(c), "\x1b["); seq != "" {
			allowed[seq] = true
		}
	}
	found := 0
	for _, r := range composerLines(t, m) {
		for _, seq := range bgSeqsIn(r) {
			found++
			if !allowed[seq] {
				t.Errorf("%s: the composer paints %q, which is not one of the active palette's (%s) fills — a "+
					"cell is left holding another palette's colour", where, seq, th.Name)
			}
		}
	}
	if found == 0 {
		t.Errorf("%s: the composer painted no fill at all, so this check proved nothing", where)
	}
}
