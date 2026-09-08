package diffs

import (
	"strings"
	"testing"
	"unicode/utf8"

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

// TestRenderPaneTruncatesUtf8Cleanly guards the truncation fix: a narrow pane
// must clip a wide multi-byte line (CJK / emoji from the unicode fixture) to
// the column budget WITHOUT splitting a rune in half. The previous byte-slicing
// truncate emitted invalid UTF-8 (a truncation artifact the acceptance
// criteria forbid). Every rendered line must remain valid UTF-8 and never
// exceed the pane width.
func TestRenderPaneTruncatesUtf8Cleanly(t *testing.T) {
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	var diff string
	for _, v := range vecs {
		if v.Name == "unicode-line-level" {
			diff = deref(v.ExpectedUnifiedDiff)
		}
	}
	if diff == "" {
		t.Fatal("unicode-line-level fixture not found")
	}
	rows := ParseUnifiedDiff(diff)
	if len(rows) == 0 {
		t.Fatal("unicode fixture produced no rows")
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	// Probe a range of narrow widths that force truncation in each cell.
	for _, width := range []int{48, 34, 24, 20, 16, 12} {
		width := width
		out := RenderPane(rows, width, termenv.Ascii)
		if out == "" {
			t.Fatalf("width %d: empty render", width)
		}
		for _, line := range strings.Split(out, "\n") {
			if line == "" {
				continue
			}
			if !utf8.ValidString(line) {
				t.Fatalf("width %d: render emitted invalid UTF-8 in line %q (bytes %v)",
					width, line, []byte(line))
			}
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d: line %q exceeds pane width (ansi width %d)",
					width, line, ansi.StringWidth(line))
			}
		}
	}
}
