package kit2

import (
	"strings"
	"testing"
)

// The operator: "The tool tip word wrap is wrapping the tool title and shortcut
// on separate lines, it should keep those together."
//
// wrapHint breaks at the hint's " · " separators, so a key and its meaning stay
// on one line; only a segment that cannot fit at all is wrapped further.
func TestWrapHintKeepsShortcutWithItsMeaning(t *testing.T) {
	hint := "↑/↓ or tab: field · ←/→: move · ctrl+u: clear · enter: next · ctrl+s: save · esc: cancel"
	for _, width := range []int{80, 60, 40, 30} {
		lines := WrapHint(hint, width)
		for _, l := range lines {
			if l == "" {
				t.Fatalf("width %d: empty line in %q", width, lines)
			}
			// No line may END on a bare key with its meaning pushed away.
			trimmed := strings.TrimRight(l, " ")
			for _, bare := range []string{"ctrl+s:", "esc:", "ctrl+u:", "enter:", "←/→:", "↑/↓"} {
				if strings.HasSuffix(trimmed, bare) {
					t.Fatalf("width %d: line %q ends on a bare key", width, l)
				}
			}
		}
	}
}

// A hint that fits is returned unchanged (no gratuitous re-flow).
func TestWrapHintPassesThroughWhenItFits(t *testing.T) {
	hint := "ctrl+g text box · enter send"
	if got := WrapHint(hint, 80); len(got) != 1 || got[0] != hint {
		t.Fatalf("a fitting hint must pass through, got %q", got)
	}
}

// A single segment longer than the line still gets wrapped rather than
// overflowing (the panel would truncate it).
func TestWrapHintHardWrapsAnOversizedSegment(t *testing.T) {
	long := "ctrl+x: " + strings.Repeat("word ", 20)
	lines := WrapHint(long, 24)
	if len(lines) < 2 {
		t.Fatalf("an oversized segment must wrap, got %q", lines)
	}
	for _, l := range lines {
		if len([]rune(l)) > 24 {
			t.Fatalf("line %q exceeds the width", l)
		}
	}
}
