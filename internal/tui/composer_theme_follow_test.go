package tui

// composer_theme_follow_test.go — THE COMPOSER IS ALWAYS PAINTED FOR THE ACTIVE PALETTE.
//
// The operator: "I don't like that the composer in the TUI always seems to be black. It should match the
// color of the theme's background ... I believe is more of a bug than a feature because when I exited orch
// and went back into it, it was fine. I think when switching from a dark to light theme it may be keeping
// the composer black."
//
// THE COMPOSER IS THE ONE COMPONENT THAT CAPTURES ITS STYLES. Every other render path reads the theme
// package's styles at paint time, so a palette switch is automatic for them; the dock hands its styles to a
// bubbles textarea, which HOLDS them, so someone has to re-pin it. That is why this class of bug has
// appeared here repeatedly (it is what 13a2cd23 fixed for the STARTUP path, where the dock was built before
// the operator's theme was applied) — and it is why the switch deserves an end-to-end pin rather than an
// inspection.
//
// These assert the PAINTED COLOURS out of a real frame after a real switch, so a composer left holding the
// previous palette's fill fails here rather than in the operator's screenshot.

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
//
// This is the assertion the two switch tests above are too weak to make, and finding that out is why it
// exists. They checked for the PREVIOUS palette's surface — but the stale cells a missing re-pin actually
// leaves behind belong to the palette that was active when the DOCK WAS BUILT (the package-init dark
// default, or the theme NewApp applied at construction), not to the one being switched away from. Measured
// with the re-pin removed, the composer's own text cells carried `48;2;18;24;38` — the base dark palette's
// surface — after switching to a light one. That IS the operator's "it may be keeping the composer black",
// and the weaker check walked straight past it.
//
// The invariant stated generally is the one worth holding: whatever the composer paints, it paints in the
// palette that is active NOW.
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
		th := theme.Active()

		// Every fill the composer is ALLOWED to use, resolved through the same renderer the frame uses.
		// The ESCAPE is trimmed because bgSeqsIn reports sequences as they appear inside a row (starting at
		// "48;") — comparing one form against the other matches nothing, which is exactly how a first
		// version of this test reported every palette as broken.
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
		if len(allowed) < 3 {
			t.Fatalf("%s: fixture resolved only %d fills", name, len(allowed))
		}

		rows := composerLines(t, m)
		found := 0
		for _, r := range rows {
			for _, seq := range bgSeqsIn(r) {
				found++
				if !allowed[seq] {
					t.Errorf("%s: the composer paints %q, which is NOT one of this palette's fills — a cell is "+
						"left holding another theme's colour, which is the operator's \"keeping the composer "+
						"black\" after a dark-to-light switch", name, seq)
				}
			}
		}
		if found == 0 {
			t.Errorf("%s: the composer painted no fill at all, so this check proved nothing", name)
		}
	}
}

// bgSeqsIn returns every BACKGROUND sequence in a rendered row, in order.
//
// THE PARAMETER BOUNDARY CHECK IS LOAD-BEARING, and its absence is what a first version of this got wrong:
// searching for the bare text "48;" anywhere matches INSIDE a foreground value — `38;2;248;242` contains
// "48;242" — so the scanner reported foreground numbers as backgrounds, and the "which fill is this"
// comparison became nonsense. An SGR parameter only starts at a `[` or after a `;`.
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
