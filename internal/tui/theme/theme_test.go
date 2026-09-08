package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// The theme must construct every named style without panicking and render
// sensibly under each termenv color profile (truecolor / 256 / 16), the
// degradation path acceptance criteria call out.

func TestStylesConstruct(t *testing.T) {
	// Touch every exported style/color so any bad declaration panics here,
	// at construction, not in a screen far from the cause.
	styled := TabActive.Render("Work") + TabInactive.Render("Execution") +
		Footer.Render("footer") + FooterVersionDrift.Render("drift") +
		ListTitle.Render("t") + ListItem.Render("i") + ListItemSelected.Render("s") +
		ListMeta.Render("m") + DetailKey.Render("k") + DetailValue.Render("v") +
		PaneBorder.Render("pane") + StatusOK.Render("ok") + StatusWarn.Render("w") +
		StatusErr.Render("e") + StatusBusy.Render("b") + HelpOverlay.Render("help") +
		ErrorText.Render("err") + HintText.Render("hint") + SpinnerStyle.Render("*") +
		DiffAdd.Render("+a") + DiffDel.Render("-d") + DiffCtx.Render(" c") +
		DiffEmphasis.Render("!") + DiffHeader.Render("h") +
		DiffLineNoOld.Render("1") + DiffLineNoNew.Render("1") +
		DiffGutter.Render("│") + DiffPanel.Render("P") +
		DiffTabActive.Render("t") + DiffTabInactive.Render("t") +
		DiffFileSel.Render("f") + DiffBadgeAdd.Render("+") + DiffBadgeDel.Render("-")
	if styled == "" {
		t.Fatal("styles rendered empty")
	}
}

// go test cannot allocate a real terminal, so assert the ANSI output under
// each profile explicitly instead of relying on termenv detection.
func TestProfileDegradation(t *testing.T) {
	profiles := []struct {
		name    string
		p       termenv.Profile
		wantSGR bool // color profiles should emit SGR; Ascii may not
	}{
		{"truecolor", termenv.TrueColor, true},
		{"256", termenv.ANSI256, true},
		{"ansi16", termenv.ANSI, true},
		{"ascii", termenv.Ascii, false},
	}
	for _, tc := range profiles {
		t.Run(tc.name, func(t *testing.T) {
			lipgloss.SetColorProfile(tc.p)
			t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
			out := TabActive.Render("Work")
			hasSGR := strings.Contains(out, "\x1b[")
			if tc.wantSGR && !hasSGR {
				t.Errorf("profile %s: expected ANSI SGR in %q", tc.name, ansi.Strip(out))
			}
			if !tc.wantSGR && hasSGR {
				t.Errorf("profile %s: expected no ANSI in %q", tc.name, out)
			}
		})
	}
}
