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
		PaneBorder.Render("pane") + ComposerBox.Render("box") +
		StatusOK.Render("ok") + StatusWarn.Render("w") +
		StatusErr.Render("e") + StatusBusy.Render("b") + HelpOverlay.Render("help") +
		ErrorText.Render("err") + HintText.Render("hint") + SpinnerStyle.Render("*") +
		DiffAdd.Render("+a") + DiffDel.Render("-d") + DiffCtx.Render(" c") +
		DiffEmphasis.Render("!") + DiffHeader.Render("h") +
		DiffLineNoOld.Render("1") + DiffLineNoNew.Render("1") +
		DiffGutter.Render("│") + DiffPanel.Render("P") +
		DiffTabActive.Render("t") + DiffTabInactive.Render("t") +
		DiffFileSel.Render("f") + DiffClose.Render("✕") +
		DiffBadgeAdd.Render("+") + DiffBadgeDel.Render("-")
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

// THE LAUNCH DEFAULT RESOLVES, AND IS A DARK PALETTE.
//
// The operator: "I want the default theme for orch to be the Ember dark theme." Two ways that can go
// wrong silently, and neither reports an error at launch:
//
//  1. the name does not exist — Lookup returns nil and the client falls back to the base palette, so
//     the operator sees the OLD default and concludes the change did not ship;
//  2. the name resolves to a LIGHT palette — `ember-light` sits directly beside `ember`, and picking
//     the wrong one is a one-word mistake that would look like a deliberate choice.
//
// Both are asserted here rather than trusted, because the failure is invisible: nothing errors, the
// theme is simply not the one that was asked for.
func TestTheLaunchDefaultIsEmberAndDark(t *testing.T) {
	th := Lookup(DefaultName)
	if th == nil {
		t.Fatalf("the launch default %q does not resolve — orch would silently fall back to the base "+
			"palette at startup, so the requested default would never appear", DefaultName)
	}
	if DefaultName != "ember" {
		t.Errorf("the launch default is %q, want \"ember\" (the operator's request). If this was changed "+
			"deliberately, update the comment on DefaultName too.", DefaultName)
	}
	// Dark means the background is darker than the text — the same test cursor_caret_test.go uses.
	if relLuminance(string(th.Bg)) >= relLuminance(string(th.Text)) {
		t.Errorf("the launch default %q is a LIGHT palette (bg %s, text %s) — the request was for the "+
			"DARK one; \"ember-light\" is the light sibling and is easy to select by mistake",
			DefaultName, th.Bg, th.Text)
	}
	// And it must be active on a fresh process, since `active` is what every render reads before any
	// preference is applied.
	if Active() == nil || Active().Name == "" {
		t.Fatal("no palette is active at init")
	}
}
