package screenkit

// list_row_test.go — THE STATUS IS ALWAYS VISIBLE ON A LIST ROW.
//
// The operator, mid-live-test:
//
//	"Both Workflow Runs and Executions titles are too long and you can't see the status. I think we
//	 should have workflow name - title - status, but maybe cut off text to ensure they all show up on
//	 the screen visibly."
//
// The row used to be built as `"  " + Title + pad + Meta` and then truncated from the RIGHT, so a long
// title ate the padding and then the status. The status is both the field the operator scans a run
// list for AND the only field that changes while a run is live, so losing it made a perfectly
// refreshing list look frozen.
//
// listRow IS the paint decision — List.View calls it and then applies nothing but an SGR style — so the
// assertions are made against listRow's exact output, plus one end-to-end read of the painted frame to
// prove View really routes through it.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// execTitle and execMeta are a REAL executions row, taken from the operator's screen: the workflow name
// and work-item title are long, the status short.
const (
	execTitle = "Execution parity — Workers (+versions), Workflows (+runs), Executions, Schedules"
	execMeta  = "running · Quick Software Engineer"
)

// THE LONG TITLE CASE — the operator's exact report. The status must survive a title that overflows.
func TestListRowKeepsTheStatusWhenTheTitleOverflows(t *testing.T) {
	row := listRow(Item{Title: execTitle, Meta: execMeta}, 80)

	if !strings.Contains(row, "running") {
		t.Fatalf("the status was truncated away by a long title — the operator's \"you can't see the "+
			"status\"\nrow: %q", row)
	}
	// The row must still fit the pane it was given.
	if w := lipgloss.Width(row); w > 80 {
		t.Errorf("row width = %d, want <= 80\nrow: %q", w, row)
	}
	// The title is SHORTENED, not lost, so the row still identifies itself.
	if !strings.Contains(row, "Execution parity") {
		t.Errorf("the title was dropped entirely; it should be shortened, not lost\nrow: %q", row)
	}
}

// THE ORDER THE OPERATOR ASKED FOR: name … status, left to right, with the status at the end.
func TestListRowPutsTheStatusAtTheEnd(t *testing.T) {
	row := listRow(Item{Title: "some-long-workflow-name-that-overflows", Meta: "failed"}, 40)
	iTitle := strings.Index(row, "some-long")
	iStatus := strings.Index(row, "failed")
	if iTitle < 0 {
		t.Fatalf("the title is not on the row: %q", row)
	}
	if iStatus < 0 {
		t.Fatalf("the status is not on the row: %q", row)
	}
	if iStatus <= iTitle {
		t.Errorf("the status must follow the title (name … status), got %q", row)
	}
}

// A NARROW PANE STILL SHOWS THE STATUS even when the title cannot fit beside it — the status is the
// scan field, so it wins the space.
func TestListRowPrefersTheStatusInANarrowPane(t *testing.T) {
	row := listRow(Item{Title: "a-very-long-title-indeed", Meta: "succeeded"}, 18)
	if !strings.Contains(row, "succeeded") {
		t.Fatalf("in a narrow pane the status must still be shown: %q", row)
	}
	if w := lipgloss.Width(row); w > 18 {
		t.Errorf("row width = %d, want <= 18: %q", w, row)
	}
}

// A row with no Meta is just the title, truncated to the pane — no padding invented, no stray gap.
func TestListRowWithoutMetaIsJustTheTitle(t *testing.T) {
	row := listRow(Item{Title: "just a title"}, 40)
	if strings.TrimRight(row, " ") != "  just a title" {
		t.Errorf("row = %q, want the indented title and nothing else", row)
	}
	long := listRow(Item{Title: strings.Repeat("x", 100)}, 30)
	if w := lipgloss.Width(long); w > 30 {
		t.Errorf("an overlong title painted %d cells, want <= 30", w)
	}
}

// THE INVARIANT, swept across every width a real terminal uses: a row with a status shows that status
// and never overflows the pane. This must hold for EVERY list in the TUI, which is why it lives in the
// shared widget rather than in the two screens that reported it.
func TestListRowAlwaysShowsTheStatusAndFitsTheWidth(t *testing.T) {
	for w := 8; w <= 220; w++ {
		row := listRow(Item{Title: execTitle, Meta: execMeta}, w)
		if got := lipgloss.Width(row); got > w {
			t.Fatalf("width %d: row painted %d cells\nrow: %q", w, got, row)
		}
		// Above a sane floor the status must be readable.
		if w >= 40 && !strings.Contains(row, "running") {
			t.Fatalf("width %d: the status is missing from the row\nrow: %q", w, row)
		}
	}
}

// AND THE LIST REALLY PAINTS THROUGH IT. This is the end-to-end read of the widget's own frame: it
// fails if View ever goes back to truncating a pre-composed row, which is exactly how the bug shipped.
func TestListViewPaintsTheStatusThroughToListRow(t *testing.T) {
	l := List{Width: 80, Height: 5}
	l.SetItems([]Item{{ID: "i", Title: execTitle, Meta: execMeta}}, "")

	frame := l.View(false)
	plain := stripANSI(frame)

	if !strings.Contains(plain, "running") {
		t.Fatalf("the painted list frame lost the status:\n%s", plain)
	}
	// Every painted row fits the width (the List is given the pane's width and must not overflow it).
	for _, ln := range strings.Split(plain, "\n") {
		if w := lipgloss.Width(ln); w > 80 {
			t.Errorf("a painted row is %d cells wide, want <= 80: %q", w, ln)
		}
	}
}

// stripANSI removes SGR sequences so assertions read the operator's text, not the renderer's escapes.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
