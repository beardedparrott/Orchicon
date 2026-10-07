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
		DiffBadgeAdd.Render("+") + DiffBadgeDel.Render("-") +
		DiffScrollThumb.Render("│") + DiffScrollTrack.Render("│")
	if styled == "" {
		t.Fatal("styles rendered empty")
	}
}

// THE DIFF PANE'S SCROLLBAR CARRIES PALETTE COLOURS — on every palette.
//
// TestStylesConstruct above only proves the two styles RENDER (a bare
// lipgloss.NewStyle() renders its text unchanged and would pass), so it cannot
// catch the failure this gate exists for: a style that is declared and used by
// the renderer but never assigned in buildStyles. Such a style carries
// lipgloss.NoColor and draws in the TERMINAL's default foreground — exactly the
// defect the Tree/Timeline list rows were fixed for, and invisible in a test
// like TestStylesConstruct. It also renders nothing at all under the Ascii
// profile, so the operator on a monochrome terminal would see no bar.
//
// The track and thumb must also be DISTINGUISHABLE (by colour, on top of the
// distinct glyphs), so position is readable even when the glyphs look similar.
func TestDiffScrollbarStylesCarryPaletteColours(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev); Use(DefaultName) })

	const white = "#f8fafc"
	for _, name := range Names() {
		Use(name)
		th := Active()
		bg := string(th.Bg)

		for label, style := range map[string]lipgloss.Style{
			"DiffScrollTrack": DiffScrollTrack,
			"DiffScrollThumb": DiffScrollThumb,
		} {
			if _, ok := style.GetForeground().(lipgloss.NoColor); ok {
				t.Errorf("%s: %s has NO foreground — it renders in the terminal's default colour "+
					"(or nothing under Ascii), so the scrollbar is invisible or wrong-coloured", name, label)
				continue
			}
			if !strings.Contains(style.Render("│"), "\x1b[") {
				t.Errorf("%s: %s emitted no SGR — the bar has no colour under TrueColor", name, label)
			}
		}

		// The track must be visible against the pane background (a track nobody can see is not an
		// affordance), and the thumb must stand out from the track so the position reads.
		track := colorHex(DiffScrollTrack.GetForeground())
		thumb := colorHex(DiffScrollThumb.GetForeground())
		if track == "" || thumb == "" {
			t.Fatalf("%s: scrollbar colours are not concrete (%q / %q)", name, track, thumb)
		}
		if r := contrastRatio(track, bg); r < 1.3 {
			t.Errorf("%s: the scrollbar TRACK (%s) is indistinguishable from the background (%s): %.2f:1",
				name, track, bg, r)
		}
		if r := contrastRatio(track, thumb); r < 1.5 {
			t.Errorf("%s: the scrollbar THUMB (%s) is indistinguishable from its track (%s): %.2f:1 — "+
				"the operator cannot see where in the content they are", name, thumb, track, r)
		}
		_ = white
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
// The operator has moved this default more than once — ember, then forest, then slate, now teal — and
// each request came with the same shape of risk. Two ways a change can go wrong SILENTLY, neither of
// which reports an error at launch:
//
//  1. the name does not exist — Lookup returns nil and the client falls back to the base palette, so
//     the operator sees the OLD default and concludes the change did not ship;
//  2. the name resolves to a LIGHT palette — `teal-light` sits directly beside `teal`, and picking
//     the wrong one is a one-word mistake that would look like a deliberate choice.
//
// Both are asserted here rather than trusted, because the failure is invisible: nothing errors, the
// theme is simply not the one that was asked for.
func TestTheLaunchDefaultIsTealAndDark(t *testing.T) {
	th := Lookup(DefaultName)
	if th == nil {
		t.Fatalf("the launch default %q does not resolve — orch would silently fall back to the base "+
			"palette at startup, so the requested default would never appear", DefaultName)
	}
	if DefaultName != "teal" {
		t.Errorf("the launch default is %q, want \"teal\" (the operator's request: \"I would also like "+
			"to make teal the default theme in the TUI now\"). If this was changed deliberately, update "+
			"the comment on DefaultName too — and note that the GUI's default dark slot is meant to "+
			"track it (frontend/src/lib/theme-store.ts).", DefaultName)
	}
	// Dark means the background is darker than the text — the same test cursor_caret_test.go uses.
	if relLuminance(string(th.Bg)) >= relLuminance(string(th.Text)) {
		t.Errorf("the launch default %q is a LIGHT palette (bg %s, text %s) — the request was for the "+
			"DARK one; \"teal-light\" is the light sibling and is easy to select by mistake",
			DefaultName, th.Bg, th.Text)
	}
	// And it must be active on a fresh process, since `active` is what every render reads before any
	// preference is applied.
	if Active() == nil || Active().Name == "" {
		t.Fatal("no palette is active at init")
	}
}
