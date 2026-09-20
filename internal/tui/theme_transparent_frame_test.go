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
	tea "github.com/charmbracelet/bubbletea"
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

// A TRANSPARENT SESSION PAINTS NO SURFACES — THE TERMINAL BLEEDS THROUGH EVERYWHERE.
//
// This is the operator's report, verbatim: "now the transparents are not transparent at all ... only transparent
// in the top left corner. The terminal should bleed through everywhere but with the light tint of the color
// scheme coming through."
//
// ASSERTED PER-CELL RATHER THAN PER-THEME, because the frame is where a transparency can be undone: the shell
// wraps every row in a base style and then runs two repairs (padScreenLine's bgOpaque and the composer's
// RepairAfterResets) whose whole job is to RE-PAINT the background after any inner reset. A transparency the
// styles honoured and the repairs undid would pass every test in the theme package and still paint solid cells
// here.
//
// THE ALLOWED FILLS ARE THE CONTENT ONES. A screen may still paint the raised fill (a code chip, a code block, a
// diff line) and the selection fill: those are content that stops working without a fill, not surfaces — see
// theme.buildStyles. Everything else must be UNPAINTED, and "everything else" includes the panel fill, which is
// the one that was covering the screen.
func TestATransparentSessionPaintsNoSurfaces(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.Ascii)
		theme.Use(theme.DefaultName)
	})

	// A screen with REAL panes on both sides, which is where the operator sees this.
	m := phase3App(120, 40)
	m.SwitchTo(TabWork)
	m = runApp(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.SetTheme("tokyo-night-transparent") {
		t.Fatal("SetTheme(tokyo-night-transparent) failed")
	}
	th := theme.Active()
	panelFill := paintSeq(th.Bg)
	if panelFill == "" {
		t.Fatal("fixture: no panel fill resolved")
	}

	// Everything the frame is ALLOWED to paint.
	allowed := map[string]bool{}
	for _, c := range []lipgloss.TerminalColor{th.SurfaceAlt, th.Select} {
		if seq := strings.TrimPrefix(paintSeq(c), "\x1b["); seq != "" {
			allowed[seq] = true
		}
	}
	if len(allowed) != 2 {
		t.Fatalf("fixture: resolved %d allowed fills, want 2", len(allowed))
	}

	rows := strings.Split(m.baseView(120, 40), "\n")
	if len(rows) < 4 {
		t.Fatalf("fixture: frame has only %d rows", len(rows))
	}
	// THE REGION SCANNED IS ABOVE THE COMPOSER, and it is scoped deliberately rather than for convenience. The
	// composer's CARET is a solid block by design (a see-through cursor is not a thing), and its style paints the
	// GROUND as its background — so on a dark terminal its sequence is byte-identical to a painted panel's and
	// cannot be told apart by inspection. The composer's own transparency is asserted where it belongs, in the
	// dock and theme packages; what this test covers is the region the operator was complaining about, where the
	// panes, the rails and the tab strip were covering the screen.
	composerTop := m.composerTopRow()
	if composerTop > len(rows) {
		composerTop = len(rows)
	}
	painted := 0
	for i := 0; i < composerTop; i++ {
		for _, seq := range bgSeqsIn(rows[i]) {
			painted++
			if !allowed[seq] {
				t.Errorf("frame row %d paints %q, which is not a content fill — a surface is stopping the "+
					"terminal from bleeding through (the panel fill is %q)", i, seq,
					strings.TrimPrefix(panelFill, "\x1b["))
			}
		}
	}
	if painted == 0 {
		t.Error("the frame painted no fills at all above the composer, so this check proved nothing")
	}

	// The shell's blank separator row is pure padding and must be completely unpainted — the "top left corner"
	// the operator could see through is the only part of a surface-painted frame that is not covered.
	if got := bgSeqsIn(rows[tabBarRows]); len(got) != 0 {
		t.Errorf("the shell's blank separator row is painted %v on a transparent theme", got)
	}

	// AND A SOLID THEME DOES PAINT SURFACES, so none of the above can pass on a frame that stopped painting.
	if !m.SetTheme("tokyo-night") {
		t.Fatal("SetTheme(tokyo-night) failed")
	}
	solidPanel := paintSeq(theme.Active().Bg)
	found := false
	for _, r := range strings.Split(m.baseView(120, 40), "\n") {
		if strings.Contains(r, solidPanel) {
			found = true
		}
	}
	if !found {
		t.Errorf("a SOLID session does not paint its panel fill %s anywhere", solidPanel)
	}
}
