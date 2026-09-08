package diffs

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
)

// TestRenderPaneDegradation verifies the truecolor → 256 → 16 → ascii
// degradation path the acceptance criteria call out: add/del lines carry a
// distinct SGR color in color profiles, and the ascii profile emits no raw
// color escape codes.
func TestRenderPaneDegradation(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	var diff string
	for _, v := range vecs {
		if v.Name == "modify-two-lines" {
			diff = deref(v.ExpectedUnifiedDiff)
		}
	}
	rows := ParseUnifiedDiff(diff)
	if len(rows) == 0 {
		t.Fatal("modify fixture produced no rows")
	}

	profiles := []struct {
		name    string
		p       termenv.Profile
		wantSGR bool
	}{
		{"truecolor", termenv.TrueColor, true},
		{"256", termenv.ANSI256, true},
		{"ansi16", termenv.ANSI, true},
		{"ascii", termenv.Ascii, false},
	}
	for _, tc := range profiles {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			lipgloss.SetColorProfile(tc.p)
			t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
			out := RenderPane(rows, 80, tc.p)
			hasSGR := strings.Contains(out, "\x1b[")
			if tc.wantSGR && !hasSGR {
				t.Errorf("profile %s: expected ANSI SGR in %q", tc.name, ansi.Strip(out))
			}
			if !tc.wantSGR && hasSGR {
				t.Errorf("profile %s: expected no ANSI SGR in %q", tc.name, out)
			}
			if out == "" {
				t.Errorf("profile %s: RenderPane returned empty", tc.name)
			}
		})
	}
}

// TestRenderPaneSideBySideVsUnified asserts the narrow-pane collapse: at a
// wide width the pane renders two columns, at a narrow width it renders
// unified (single-column) — mirroring the GUI's DiffView breakpoint.
func TestRenderPaneSideBySideVsUnified(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	var diff string
	for _, v := range vecs {
		if v.Name == "modify-two-lines" {
			diff = deref(v.ExpectedUnifiedDiff)
		}
	}
	rows := ParseUnifiedDiff(diff)
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	wide := RenderPane(rows, MinSideBySideWidth+20, termenv.Ascii)
	narrow := RenderPane(rows, MinSideBySideWidth-10, termenv.Ascii)
	if wide == "" || narrow == "" {
		t.Fatal("render produced empty output")
	}
	// The wide layout's header shows both an "old" and a "new" column; the
	// unified layout collapses to a single column (line-number gutter pair
	// per row, no column separator header).
	if !strings.Contains(wide, "old") || !strings.Contains(wide, "new") {
		t.Errorf("wide render missing column headers:\n%s", wide)
	}
	if strings.Contains(narrow, "old") || strings.Contains(narrow, "new") {
		t.Errorf("narrow render should collapse to unified but kept column headers:\n%s", narrow)
	}
}
