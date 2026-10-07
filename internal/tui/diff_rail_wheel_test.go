package tui

// diff_rail_wheel_test.go — THE RAIL OWNS THE WHEEL IN ITS COLUMNS.
//
// The operator: "In the TUI diff bar, the scroll doesn't seem to be working."
//
// The rail's handlers were fine — it scrolls on a wheel (diffs.Model.handleMouse) and on the keyboard
// (j/k/pgup/pgdown/g/G via diffMsg). What was wrong is that it never RECEIVED a wheel event: the Ask
// transcript claimed every wheel on the Ask tab and returned, before the message could be routed to the
// pane. Since the rail is painted OVER the transcript, scrolling it moved the transcript underneath while
// the rail itself never budged.
//
// So the test is two-sided, and the second half is what keeps the fix honest: the wheel must reach the rail
// in the rail's columns, and must STILL scroll the transcript everywhere else.

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

func TestTheDiffRailOwnsTheWheelInItsColumns(t *testing.T) {
	m, _, _ := newScopeApp(t)
	// A width where the left rail, the content and the right (conversations) rail all fit, so the test's x
	// values are unambiguous.
	m.width, m.height = 200, 50
	m.active = TabAsk
	if m.chatConvID == "" {
		t.Fatal("no conversation is open — the transcript claim would not be reached at all")
	}
	if m.diffPane == nil {
		t.Fatal("the app has no diff pane")
	}
	m.diffOpen = true

	// A transcript with content to scroll, parked away from the bottom so a wheel has somewhere to go.
	str := kit2.NewStream("transcript", 120, 20)
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	str.SetLines(lines)
	str.Offset = 30
	m.chatStreams[m.chatConvID] = str

	send := func(mo tea.MouseMsg) *App {
		nm, _ := m.Update(mo)
		return nm.(*App)
	}

	// (1) INSIDE the rail: the transcript must not move. This is the report — the wheel was scrolling the
	// conversation instead of the diff.
	m = send(tea.MouseMsg{X: 2, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if str.Offset != 30 {
		t.Errorf("a wheel inside the diff rail scrolled the TRANSCRIPT (offset %d, want 30): the rail is "+
			"painted over the transcript, so the pane the operator aimed at never moved", str.Offset)
	}

	// (2) OUTSIDE the rail (and clear of the conversations rail on the right): the transcript still scrolls,
	// so the fix narrowed the claim rather than deleting it.
	x := m.diffPaneWidth() + 5
	if x >= m.width-ConversationsRailWidth {
		t.Fatalf("test geometry: x=%d is inside the conversations rail (starts at %d), which claims the "+
			"wheel itself — the assertion below would be about the wrong rail", x, m.width-ConversationsRailWidth)
	}
	m = send(tea.MouseMsg{X: x, Y: 10, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if str.Offset != 27 {
		t.Errorf("a wheel outside the rail left the transcript at %d, want 27 (−3): the transcript claim "+
			"must still work where the rail does not reach", str.Offset)
	}
}

// The two callers of the column test must agree, because a column belongs to the rail or it does not — and
// the bug was exactly that one of them tested something else (the tab).
func TestTheRailColumnTestIsTheSameForAScrollAndAClick(t *testing.T) {
	m, _, _ := newScopeApp(t)
	m.width, m.height = 200, 50
	m.diffOpen = true
	w := m.diffPaneWidth()

	for _, c := range []struct {
		x    int
		want bool
	}{{0, true}, {w - 1, true}, {w, false}, {w + 1, false}} {
		if got := m.diffRailOwnsX(c.x); got != c.want {
			t.Errorf("diffRailOwnsX(%d) = %v, want %v (the rail spans columns 0..%d)", c.x, got, c.want, w-1)
		}
	}
	// Closed pane: no column belongs to it, whatever the x.
	m.diffOpen = false
	if m.diffRailOwnsX(0) {
		t.Error("a CLOSED rail claimed a column — a wheel there would be swallowed with nothing to scroll")
	}
}
