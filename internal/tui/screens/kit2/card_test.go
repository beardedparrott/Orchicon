package kit2

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestCardLinesAreExactlyTheWidth pins the card's sizing contract: every row is
// EXACTLY `width` display cells. A card one cell wide or narrow would shift the
// whole transcript it is spliced into.
func TestCardLinesAreExactlyTheWidth(t *testing.T) {
	spec := CardSpec{
		Title:  "Permission",
		Body:   "write /home/ops/project/long/path/file.go",
		Lines:  []CardLine{{Text: "Allow once"}, {Text: "Allow for this session"}, {Text: "Deny"}},
		Footer: CardFooter(false),
	}
	for _, w := range []int{28, 40, 72} {
		for i, l := range CardLines(spec, w) {
			if got := lipgloss.Width(l); got != w {
				t.Fatalf("width %d: line %d is %d cells, want %d: %q", w, i, got, w, l)
			}
		}
	}
}

// TestCardHighlightsOnlyTheSelectedRow pins the selection rule: exactly one row
// carries the selection paint, and it is the row the caller marked.
func TestCardHighlightsOnlyTheSelectedRow(t *testing.T) {
	spec := CardSpec{
		Title: "Permission",
		Lines: []CardLine{{Text: "Allow once", Selected: true}, {Text: "Deny"}},
	}
	lines := CardLines(spec, 40)
	hits := 0
	for _, l := range lines {
		if strings.Contains(l, "▸ Allow once") {
			hits++
		}
		if strings.Contains(l, "▸ Deny") {
			t.Fatalf("an unselected row must not be highlighted: %q", l)
		}
	}
	if hits != 1 {
		t.Fatalf("the selected row must appear exactly once, got %d", hits)
	}
}

// TestCardDisabledRowCarriesItsReason pins decision 11: a row the policy denies
// is drawn with the pattern NAMED rather than offered.
func TestCardDisabledRowCarriesItsReason(t *testing.T) {
	spec := CardSpec{
		Title: "Permission",
		Lines: []CardLine{
			{Text: "Allow once"},
			{Text: "Allow for this session", Disabled: true, Detail: "denied by /etc/**"},
			{Text: "Deny", Selected: true},
		},
	}
	joined := strings.Join(CardLines(spec, 60), "\n")
	if !strings.Contains(joined, "denied by /etc/**") {
		t.Fatalf("a disabled row must name the reason it is disabled:\n%s", joined)
	}
}

// TestCardArrowsSkipDisabledRows pins the reachability rule: a disabled row
// cannot be moved onto, so Enter can never commit a grant the policy refuses.
func TestCardArrowsSkipDisabledRows(t *testing.T) {
	c := &Card{Lines: []CardLine{
		{Text: "Allow once"},
		{Text: "Allow for this session", Disabled: true},
		{Text: "Deny"},
	}, Sel: 0}
	if _, closed := c.HandleKey(tea.KeyMsg{Type: tea.KeyDown}); closed {
		t.Fatal("a move key must not close the card")
	}
	if c.Sel != 2 {
		t.Fatalf("down must skip the disabled row, landed on %d", c.Sel)
	}
	if _, closed := c.HandleKey(tea.KeyMsg{Type: tea.KeyUp}); closed {
		t.Fatal("a move key must not close the card")
	}
	if c.Sel != 0 {
		t.Fatalf("up must skip the disabled row, landed on %d", c.Sel)
	}
}

// TestCardEnterOnDisabledIsNotAChoice pins that Enter on a disabled row is a
// no-op rather than a decision.
func TestCardEnterOnDisabledIsNotAChoice(t *testing.T) {
	c := &Card{Lines: []CardLine{
		{Text: "Allow once"},
		{Text: "Allow for this session", Disabled: true},
	}, Sel: 1}
	if choice, closed := c.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}); closed || choice != -1 {
		t.Fatalf("enter on a disabled row must be a no-op, got choice=%d closed=%v", choice, closed)
	}
}

// TestCardEscReturnsClosed pins that esc is the caller's cue to resolve.
func TestCardEscReturnsClosed(t *testing.T) {
	c := &Card{Lines: []CardLine{{Text: "Deny"}}}
	if _, closed := c.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); !closed {
		t.Fatal("esc must report the card closed")
	}
}

// TestCardFooterNamesEscsOutcome pins the "a mode the operator cannot tell they
// are in is worse than no mode" rule: the footer says what esc DOES, because on
// this card esc is a decision rather than a dismissal.
func TestCardFooterNamesEscsOutcome(t *testing.T) {
	if !strings.Contains(CardFooter(false), "esc denies") {
		t.Fatalf("the permission card must say esc denies, got %q", CardFooter(false))
	}
	if !strings.Contains(CardFooter(true), "esc dismisses") {
		t.Fatalf("the question card must say esc dismisses, got %q", CardFooter(true))
	}
}
