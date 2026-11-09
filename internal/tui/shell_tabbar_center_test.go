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

// TestTabBarCenteredPins operator finding 2 (Phase 2a): the tab bar is horizontally CENTERED across
// the viewport at 80 and 120 columns — the pre-Phase-2a render left-aligned it.
func TestTabBarCentered(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, h := size[0], size[1]
		app := newCenterTestApp(w, h)
		app.RegisterScreen(TabAsk, &tabBarScreenStub{body: "SRC"})
		app.SwitchTo(TabAsk)
		bar := app.tabBarView()
		// TabBar style carries Padding(0,1): the bar's own leading space is part of the painted chrome, so
		// the viewport gutter is measured from the stripped bar's own head.
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
		// CENTERING, ASSERTED EXACTLY: the row is the pad, then the bar, then the remainder.
		pad := (w - barW) / 2
		wantRow := strings.Repeat(" ", pad) + ansi.Strip(bar) + strings.Repeat(" ", w-barW-pad)
		if row != wantRow {
			t.Errorf("%d: centered row is not pad+bar+remainder\n got %q\nwant %q", w, row, wantRow)
		}
	}
}

// TestTabBarCenteredRegression is the regression pin: the tab bar must NOT hug the left edge (the
// pre-Phase-2a bug).
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
