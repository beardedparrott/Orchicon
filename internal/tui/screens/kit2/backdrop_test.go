package kit2

// backdrop_test.go — the modal BACKDROP, which is what makes a spliced box readable over a live frame.
//
// The operator, on /grants: "it puts it in the middle of screen and it's hard to see because it writes it
// overtop text." The box was already centered and opaque; what was missing is that everything BEHIND it stayed
// at full brightness, so the operator read two layers at once. These pin the two properties that fix it and the
// one that must not break while doing so (the frame contract).

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// trueColor forces a colour-capable profile for one test.
//
// IT IS REQUIRED, NOT DECORATION: lipgloss degrades to plain text on a non-TTY, so without it theme.HintText
// renders no styling at all and the recessing these tests are about is literally unobservable — the same reason
// internal/tui/theme's own opaque and contrast suites do this.
func trueColor(t *testing.T) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
}

// fullWidthRows builds a base whose rows are each exactly `width` cells, the way a real frame is.
func fullWidthRows(lines []string, width int) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if pad := width - len([]rune(l)); pad > 0 {
			l += strings.Repeat(" ", pad)
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestDimBackdropRecessesWithoutChangingTheFrame pins the two halves that must hold together: the CONTENT
// survives (so the operator can still see what is behind the modal) while its STYLING is replaced (so it
// recedes), and the frame keeps exactly its row count and width — the shell normalizes its render to w×h, and a
// backdrop that changed either would reflow the layout behind the modal.
func TestDimBackdropRecessesWithoutChangingTheFrame(t *testing.T) {
	trueColor(t)
	const w = 48
	base := fullWidthRows([]string{
		"BRIGHT TRANSCRIPT LINE ONE",
		"BRIGHT TRANSCRIPT LINE TWO",
		"BRIGHT TRANSCRIPT LINE THREE",
	}, w)

	dim := DimBackdrop(base, w)

	// GEOMETRY: same rows, every row still exactly w cells.
	if got, want := len(strings.Split(dim, "\n")), 3; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	for i, r := range strings.Split(dim, "\n") {
		if lw := visibleWidth(r); lw != w {
			t.Fatalf("row %d is %d cells, want %d — the frame contract is broken", i, lw, w)
		}
	}
	// CONTENT SURVIVES: the text is still readable, which is the point of dimming rather than hiding.
	for _, s := range []string{"BRIGHT TRANSCRIPT LINE ONE", "BRIGHT TRANSCRIPT LINE TWO", "BRIGHT TRANSCRIPT LINE THREE"} {
		if !strings.Contains(ansi.Strip(dim), s) {
			t.Fatalf("dimming removed %q from the backdrop:\n%s", s, ansi.Strip(dim))
		}
	}
	// BUT NOT AT ITS ORIGINAL BRIGHTNESS: the styling changed, which is what makes the modal the foreground.
	if dim == base {
		t.Fatal("the backdrop was not recessed at all — the modal would still compete with the transcript")
	}
}

// TestCenterOnDimmedCentersTheBoxAndKeepsTheFrame — centering plus the frame contract in one place, because the
// two are checked together by the shell: the composite must still be exactly width×height.
func TestCenterOnDimmedCentersTheBoxAndKeepsTheFrame(t *testing.T) {
	trueColor(t)
	const w, h = 60, 12
	lines := make([]string, 0, h)
	for i := 0; i < h; i++ {
		lines = append(lines, "BACKDROP ROW")
	}
	base := fullWidthRows(lines, w)
	box := "┌────┐\n│BOX │\n└────┘"

	got := CenterOnDimmed(base, box, w, h)

	rows := strings.Split(got, "\n")
	if len(rows) != h {
		t.Fatalf("rows = %d, want %d", len(rows), h)
	}
	for i, r := range rows {
		if lw := visibleWidth(r); lw != w {
			t.Fatalf("row %d is %d cells, want %d", i, lw, w)
		}
	}
	// Centered: the box's 3 rows start at (h-3)/2 = 4.
	top := (h - 3) / 2
	for i, r := range rows {
		inBox := strings.Contains(ansi.Strip(r), "BOX")
		if i == top+1 && !inBox {
			t.Fatalf("the box must be vertically centered: row %d should carry it, got %q", i, ansi.Strip(r))
		}
		if inBox && i != top+1 {
			t.Fatalf("the box landed on row %d, want %d", i, top+1)
		}
	}
	// And the backdrop around it is recessed while the box is not.
	if !strings.Contains(ansi.Strip(rows[0]), "BACKDROP ROW") {
		t.Fatal("the backdrop content was lost")
	}
	if rows[0] == fullWidthRows([]string{"BACKDROP ROW"}, w) {
		t.Fatal("the backdrop row was not recessed")
	}
}

// visibleWidth is the display width of a styled row (cells, not runes: these frames contain box-drawing glyphs).
func visibleWidth(s string) int {
	return len([]rune(ansi.Strip(s)))
}
