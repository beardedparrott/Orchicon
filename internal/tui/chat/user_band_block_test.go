package chat

// user_band_block_test.go — THE OPERATOR'S BAND IS A BLOCK, NOT ONE RIGHT-ALIGNED ROW PER LINE.
//
// The operator, with a screenshot of a wrapped message: "See how when it drops a line it starts from the right and
// not the left? It leaves a large whitespace in front of the subsequent lines."
//
// Right-aligning each row INDIVIDUALLY is what produced that: every line was pushed flush right on its own, so
// each line began at a different column and the shorter ones started far across the pane — the second line of a
// two-line message could begin half-way in.
//
// The fix is a right-aligned BLOCK: one left edge shared by every row, which is how the GUI's bubble works (its
// text is left-aligned inside a box that sits on the right). These assertions are chosen to DISCRIMINATE the two
// shapes rather than merely to pass: per-row right alignment makes the leading columns DIFFER, and makes the
// short lines start deep in the pane.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// leadingCells is how many blank cells sit before a row's first glyph.
func leadingCells(row string) int {
	s := ansi.Strip(row)
	return len(s) - len(strings.TrimLeft(s, " "))
}

// bandRows is a rendered band's non-blank rows, in order. The blank rows are the inter-message gap (chatBandGap),
// not content.
func bandRows(out string) []string {
	var rows []string
	for _, r := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(ansi.Strip(r)) == "" {
			continue
		}
		rows = append(rows, r)
	}
	return rows
}

// EVERY ROW OF A WRAPPED MESSAGE STARTS AT THE SAME COLUMN — the operator's report, inverted into a rule.
func TestAWrappedUserMessageSharesOneLeftEdge(t *testing.T) {
	const pane = 60
	out := RenderItems([]ChatItem{{Kind: KindUser, Text: strings.Repeat("word ", 45)}}, pane)
	rows := bandRows(out)
	if len(rows) < 3 {
		t.Fatalf("fixture: the message did not wrap into several rows: %q", out)
	}

	// The label rides row 0, so the rows to compare are the continuations.
	first := leadingCells(rows[0])
	if first != 1 {
		t.Errorf("row 0 starts at column %d, want the 1-cell band padding: %q", first, ansi.Strip(rows[0]))
	}
	var cont []int
	for _, r := range rows[1:] {
		cont = append(cont, leadingCells(r))
	}
	for i, c := range cont {
		if c != cont[0] {
			t.Errorf("continuation row %d starts at column %d but the first starts at %d — each line beginning at "+
				"its own column is exactly the operator's \"it starts from the right and not the left\":\n%q",
				i+1, c, cont[0], out)
		}
	}

	// AND THE FIRST LINE'S TEXT SITS AT THAT SAME COLUMN, which is what makes it a BLOCK rather than a label
	// with text that happens to be wrapped. "word" cannot match the label, so Index finds the message.
	if at := strings.Index(ansi.Strip(rows[0]), "word"); at != cont[0] {
		t.Errorf("row 0's text starts at column %d but its continuations start at %d — the block has no single "+
			"left edge:\n%q", at, cont[0], out)
	}

	// AND THE BLOCK IS NOT PUSHED ACROSS THE PANE. Without this, a per-row right alignment could still satisfy
	// the shared-column check whenever every line happened to be the same width.
	if cont[0] > pane/2 {
		t.Errorf("the block starts at column %d of %d — more than half the pane is whitespace in front of it: %q",
			cont[0], pane, out)
	}
}

// AND A SHORT MESSAGE IS STILL FLUSH RIGHT, which is the shape the operator asked for and the sibling test
// (TestUserBubbleRightAlignedModelLeftAligned) pins. The block rule must not have quietly turned the operator's
// band into a left-aligned one.
func TestAShortUserMessageIsStillFlushRight(t *testing.T) {
	const pane = 60
	out := RenderItems([]ChatItem{{Kind: KindUser, Text: "short"}}, pane)
	rows := bandRows(out)
	if len(rows) != 1 {
		t.Fatalf("fixture: a five-letter message wrapped: %q", out)
	}
	at := strings.Index(ansi.Strip(rows[0]), "short")
	if at < pane/2 {
		t.Errorf("a short message starts at column %d of %d — the operator's band is no longer on the right:%q",
			at, pane, ansi.Strip(rows[0]))
	}
}

// AND NOTHING IS CUT OFF THE FIRST LINE. The label used to be paid for by TRUNCATING row 0 after the wrap, which
// silently dropped up to a label's worth of characters from the first line of a long message. The body is now
// wrapped to a width that already excludes the label, so every word survives.
func TestNoWordIsLostFromTheFirstLineOfALongMessage(t *testing.T) {
	// Distinct, countable words: every one must appear in the render. The tokens are ZERO-PADDED so no one of
	// them is a substring of another — otherwise a dropped "tok1" would be found inside "tok10" and the test
	// would pass on the very bug it exists to catch.
	var words []string
	for i := 0; i < 40; i++ {
		words = append(words, fmt.Sprintf("tok%02d", i))
	}
	out := ansi.Strip(RenderItems([]ChatItem{{Kind: KindUser, Text: strings.Join(words, " ")}}, 60))
	for _, w := range words {
		if !strings.Contains(out, w) {
			t.Errorf("word %q was dropped from the render — the label must be reserved BEFORE the wrap, not cut out "+
				"of the first line afterwards", w)
		}
	}
}
