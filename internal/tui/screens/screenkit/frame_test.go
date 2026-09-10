package screenkit

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestFrameFillsRegion pins the screenkit frame helper: a composed block
// that is shorter/narrower than the content region is padded to EXACTLY
// w×h (so panes fill the region instead of floating at the top-left), and
// an over-long block is truncated rather than overflowing.
func TestFrameFillsRegion(t *testing.T) {
	got := Frame("alpha\nbeta", 20, 6)
	lines := strings.Split(got, "\n")
	if len(lines) != 6 {
		t.Fatalf("rows = %d, want 6", len(lines))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w != 20 {
			t.Fatalf("row %d width = %d, want 20", i, w)
		}
	}
	if !strings.Contains(lines[0], "alpha") || !strings.Contains(lines[1], "beta") {
		t.Fatalf("content lost: %q", lines[0:2])
	}
	// Wider than the region: truncated, never overflowing.
	wide := Frame(strings.Repeat("x", 50)+"\n"+strings.Repeat("y", 50), 10, 3)
	for i, l := range strings.Split(wide, "\n") {
		if w := lipgloss.Width(l); w != 10 {
			t.Fatalf("wide row %d width = %d, want 10", i, w)
		}
	}
	// Taller than the region: clipped to h.
	tall := Frame(strings.Repeat("z\n", 9), 4, 3)
	if n := len(strings.Split(tall, "\n")); n != 3 {
		t.Fatalf("tall block rows = %d, want 3", n)
	}
	// An unsized region passes content through (never collapses to 1×1).
	if passthrough := Frame("alpha\nbeta", 0, 0); passthrough != "alpha\nbeta" {
		t.Fatalf("unsized frame must pass content through, got %q", passthrough)
	}
}

// TestBaseHasAuthRetry pins the dedupe signal the shell consults so the
// re-auth banner renders once.
func TestBaseHasAuthRetry(t *testing.T) {
	var b Base
	b.AddSource("things", "Things", nil)
	if b.HasAuthRetry() {
		t.Fatal("a healthy pane must not report an auth retry")
	}
	b.sources[0].list.Err = "session needs re-authentication — run /connect"
	if !b.HasAuthRetry() {
		t.Fatal("an unauthenticated pane must report the inline re-auth retry")
	}
}
