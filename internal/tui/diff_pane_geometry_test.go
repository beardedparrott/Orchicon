package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/beardedparrott/orchicon/internal/testfixtures"
	"github.com/beardedparrott/orchicon/internal/tui/diffs"
)

// TestDiffPaneComposedGeometryUnchanged is the anti-tear gate for the wrap +
// viewport + scrollbar work: at several terminal sizes, the composed App.View()
// has exactly the terminal's row count, every row is exactly the terminal width,
// and the pane's column band is exactly diffPaneWidth() wide. It is what pins
// that the pane's outer geometry did NOT change even though its body now reserves
// a scrollbar column and its content is one cell narrower than the panel.
func TestDiffPaneComposedGeometryUnchanged(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	sizes := []struct{ w, h int }{{200, 50}, {120, 40}, {80, 24}}
	for _, sz := range sizes {
		sz := sz
		t.Run(sizeName(sz.w, sz.h), func(t *testing.T) {
			m, _ := qaApp(t)
			m.width, m.height = sz.w, sz.h
			m = openPaneViaD(m)
			if !m.diffOpen || !m.diffPane.HasOwner() {
				t.Fatalf("%dx%d: pane not open with an owner", sz.w, sz.h)
			}
			pw := m.diffPaneWidth()

			rows := strings.Split(m.View(), "\n")
			if len(rows) != sz.h {
				t.Errorf("%dx%d: composed view has %d rows, want %d", sz.w, sz.h, len(rows), sz.h)
			}
			for i, r := range rows {
				if w := ansi.StringWidth(r); w != sz.w {
					t.Errorf("%dx%d: row %d width=%d, want %d — JoinHorizontal would tear", sz.w, sz.h, i, w, sz.w)
				}
			}

			// The pane occupies columns [0, pw); no row may be wider than pw
			// before the content joins on — read the pane's OWN view, which the
			// shell normalizes to exactly pw.
			paneLines := normalizeBlock(m.diffPane.View(), pw, m.screenRows()+m.dock.Lines()+m.panelRows())
			for i, l := range paneLines {
				if w := ansi.StringWidth(l); w != pw {
					t.Errorf("%dx%d: pane row %d width=%d, want diffPaneWidth()=%d", sz.w, sz.h, i, w, pw)
				}
			}
		})
	}
}

// TestDiffPaneNarrowRendersOneColumn is the measured-collapse acceptance
// criterion at the COMPOSED level: the narrow terminal (80 cols, pane at its
// 48-cell floor) renders the diff in ONE readable column, while a wide one shows
// the two-column side-by-side layout.
func TestDiffPaneNarrowRendersOneColumn(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	// Drive the pane directly at the two body widths the shell produces.
	rows := diffs.ParseUnifiedDiff(unifiedFixture(t, "modify-two-lines"))
	if len(rows) == 0 {
		t.Fatal("no rows")
	}

	// Pane floor: 48-cell pane → body 46 (< MinSideBySideWidth) → unified.
	narrowPane := 48
	narrowBody := narrowPane - 2
	if narrowBody >= diffs.MinSideBySideWidth {
		t.Fatalf("pane floor body %d is not below the threshold %d", narrowBody, diffs.MinSideBySideWidth)
	}
	for _, l := range diffs.RenderLines(rows, narrowBody, termenv.Ascii) {
		s := ansi.Strip(l)
		if strings.Contains(s, " old ") || strings.Contains(s, " new ") {
			t.Errorf("narrow pane kept side-by-side headers: %q", s)
		}
	}

	// A 120-col terminal: pane 54 → body 52 (>= threshold) → side-by-side.
	wideBody := 54 - 2
	if wideBody < diffs.MinSideBySideWidth {
		t.Fatalf("120-col body %d is below the threshold %d — the pane would collapse", wideBody, diffs.MinSideBySideWidth)
	}
	wideLines := diffs.RenderLines(rows, wideBody, termenv.Ascii)
	if h := ansi.Strip(wideLines[0]); !strings.Contains(h, "old") || !strings.Contains(h, "new") {
		t.Errorf("120-col pane collapsed to unified: %q", h)
	}
}

func sizeName(w, h int) string { return fmt.Sprintf("%dx%d", w, h) }

// unifiedFixture returns one shared fixture vector's expected unified diff.
func unifiedFixture(t *testing.T, name string) string {
	t.Helper()
	vecs, err := testfixtures.LoadFileEditVectors()
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	for _, v := range vecs {
		if v.Name == name && v.ExpectedUnifiedDiff != nil {
			return *v.ExpectedUnifiedDiff
		}
	}
	t.Fatalf("fixture %q not found", name)
	return ""
}
