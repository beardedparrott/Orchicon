package md

import (
	"strings"
	"testing"
)

// search_highlight_test.go — the in-transcript search MATCH BAND.
//
// The operator: "it doesn't highlight the findings so it makes it rather difficult in a large stream of text to
// find where the found item is on the page. We should add a highlight to the conversation text for each item
// the search finds."
//
// The band is drawn by the markdown renderer because that is where the text becomes styled output, and it
// mirrors the CHIP and the ACCENT exactly: a fail-closed colour set, and a close that RESTORES the surface
// rather than resetting to the terminal's defaults (a reset would drop the rest of the row to the host's
// colours — the band hole the chip section documents).

// withHighlight installs a usable match band for the duration of a test.
func withHighlight(t *testing.T) {
	t.Helper()
	SetSearchHighlight("#101010", "#fbbf24", "#e5e5e5", "#181818")
	SetCodeChip("#e5e5e5", "#181818")
	t.Cleanup(func() {
		SetSearchHighlight("", "", "", "")
		SetCodeChip("", "")
	})
}

// stripSGR removes escape sequences so the test can look at the TEXT, which must be untouched by marking.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip the 'm'
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestSearchMarksEveryOccurrence(t *testing.T) {
	withHighlight(t)
	lines := RenderOnSearch("alpha needle beta needle gamma", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "needle")
	joined := strings.Join(lines, "\n")

	if got := strings.Count(joined, highlightOpen()); got != 2 {
		t.Fatalf("match bands = %d, want 2 (one per occurrence)\n%q", got, joined)
	}
	// THE TEXT IS UNCHANGED: marking is presentation, and losing a character to it would corrupt the reply.
	if got := stripSGR(joined); !strings.Contains(got, "alpha needle beta needle gamma") {
		t.Errorf("marking altered the text: %q", got)
	}
}

func TestSearchPreservesOriginalCase(t *testing.T) {
	withHighlight(t)
	lines := RenderOnSearch("a NEEDLE here", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "needle")
	if got := stripSGR(strings.Join(lines, "\n")); !strings.Contains(got, "NEEDLE") {
		t.Errorf("the match was re-cased, which is not what was written: %q", got)
	}
}

func TestNoQueryRendersExactlyAsBefore(t *testing.T) {
	withHighlight(t)
	// An armed colour set with an EMPTY query must be a no-op — the state before a search starts.
	with := RenderOnSearch("plain text with needle", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "")
	without := RenderOn("plain text with needle", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"})
	if strings.Join(with, "\n") != strings.Join(without, "\n") {
		t.Error("an empty query changed the render; the unsearched path must be byte-for-byte identical")
	}
}

func TestNoMatchRendersExactlyAsBefore(t *testing.T) {
	withHighlight(t)
	a := RenderOnSearch("nothing to see", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "zzz")
	b := RenderOn("nothing to see", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"})
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Error("a non-matching query changed the render")
	}
}

// FAIL CLOSED: with no band configured the query must not be drawn at all. A band whose close could not
// restore the surface would leak its background across the rest of the row.
func TestHighlightIsOffWithoutAColourSet(t *testing.T) {
	SetSearchHighlight("", "", "", "")
	lines := RenderOnSearch("a needle here", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "needle")
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "\x1b[48;") {
		t.Errorf("a background was emitted with no band configured: %q", joined)
	}
}

// THE QUERY IS RENDER-SCOPED, so a render that passed one cannot bleed into the next.
func TestSearchQueryDoesNotLeakBetweenRenders(t *testing.T) {
	withHighlight(t)
	_ = RenderOnSearch("a needle here", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "needle")
	after := RenderOn("a needle here", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"})
	if strings.Contains(strings.Join(after, "\n"), highlightOpen()) {
		t.Error("the search query leaked into a later render")
	}
}

// A match spanning styling — the case that makes marking at the SPAN level rather than the source level the
// right choice: the hit is found in the text as drawn, not in a source form that lost its markup.
func TestSearchMarksInsideStyledRuns(t *testing.T) {
	withHighlight(t)
	lines := RenderOnSearch("plain **bold needle** plain", 80, Surface{Fg: "#e5e5e5", Bg: "#181818"}, "needle")
	if got := strings.Count(strings.Join(lines, "\n"), highlightOpen()); got != 1 {
		t.Fatalf("match bands = %d, want 1 inside the emphasis run", got)
	}
}
