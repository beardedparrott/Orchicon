package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func newCenterTestApp(w, h int) *App {
	app := NewApp(&client.Clients{}, &config.Profile{URL: "http://x", Token: "t"}, "v0.2.51")
	app.width, app.height = w, h
	return app
}

// TestTabBarCenteredPins operator finding 2 (Phase 2a): the tab bar is
// horizontally CENTERED across the viewport at 80 and 120 columns — the
// pre-Phase-2a render left-aligned it. The centered row is exactly w cells
// (theme-painted), and the blank gutter BEFORE the first tab equals the
// gutter AFTER the last tab (odd remainders go right).
func TestTabBarCentered(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		app := newCenterTestApp(w, h)
		app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "SRC"})
		app.SwitchTo(TabAsk)
		bar := app.tabBarView()
		// TabBar style carries Padding(0,1): the bar's own leading space is
		// part of the painted chrome, so the viewport gutter is measured
		// from the stripped bar's own head. barW stays the rendered width
		// (ANSI-aware); gutterTarget = (w - barW)/2 + padding.
		barW := lipgloss.Width(bar)
		if barW >= w {
			t.Fatalf("%d: tab bar (%d) overflows the viewport", w, barW)
		}
		// The full-width centered row the shell paints on row 0.
		row := ansi.Strip(app.centeredTabBarView())
		if got := lipgloss.Width(row); got != w {
			t.Fatalf("%d: centered tab row width %d, want %d", w, got, w)
		}
		if !strings.Contains(row, "Ask Orchicon") || !strings.Contains(row, "Control") {
			t.Fatalf("%d: centered row lost tab labels: %q", w, row)
		}
		// Assert on the plain (ANSI-stripped) row so the style bytes can
		// never shift the measurement. The bar render carries TWO painted
		// pad columns on each edge (the TabBar container pad + the first
		// tab's own pad), so the blank gutter before the first glyph is
		// (w-barW)/2 + 2 — symmetric with the trailing side.
		leading := len(row) - len(strings.TrimLeft(row, " "))
		trailing := len(row) - len(strings.TrimRight(row, " "))
		wantLeading := (w-barW)/2 + 2
		if leading != wantLeading {
			t.Errorf("%d: leading gutter %d, want %d (centered, bar %d)", w, leading, wantLeading, barW)
		}
		wantTrailing := w - barW - (w-barW)/2 + 2
		if trailing != wantTrailing {
			t.Errorf("%d: trailing gutter %d, want %d (centered, bar %d)", w, trailing, wantTrailing, barW)
		}
	}
}

// TestTabBarCenteredRegression is the regression pin: the tab bar must NOT
// hug the left edge (the pre-Phase-2a bug). At 120 the bar (84 cols) must
// start at column 18, not column 0.
func TestTabBarCenteredNotLeftAligned(t *testing.T) {
	app := newCenterTestApp(120, 40)
	row := ansi.Strip(app.centeredTabBarView())
	leading := len(row) - len(strings.TrimLeft(row, " "))
	if leading == 0 {
		t.Fatalf("tab bar is left-aligned at 120 (regression): %q", row)
	}
	if leading < 10 {
		t.Fatalf("tab bar barely indented at 120 (%d cols), want centered (~18)", leading)
	}
}
