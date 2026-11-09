package dock

// dock_geometry_test.go — the dock's two published measurements, pinned against its own render.

import (
	"strings"
	"testing"
)

func TestRenderedRowsMatchLines(t *testing.T) {
	for _, fullsend := range []bool{false, true} {
		for _, width := range []int{40, 60, 80, 120, 160, 200} {
			m := New()
			m.Width = width
			m.Focus()
			m.SetValue("draft text here")
			m.Stats = "orchicon/anthropic/claude-sonnet-4 · ctx 124K/200K · 1.2M tok · $1.2345"
			m.Mode = "brainstorm"
			m.Model = "orchicon/deepseek/deepseek-flash"
			if fullsend {
				m.Fullsend = "FULLSEND"
			}
			rendered := len(strings.Split(strings.TrimRight(m.View(), "\n"), "\n"))
			if rendered != m.Lines() {
				t.Errorf("fullsend=%v width=%d: Lines()=%d but View() rendered %d rows — the shell "+
					"reserves the former and paints the latter, so the dock's top would be clipped",
					fullsend, width, m.Lines(), rendered)
			}
		}
	}
}
