package kit2

// base_enter_test.go — what ENTER does on a selected row.
//
// The operator: "In Ask Orchicon, hitting enter on a conversation doesn't bring it up.
// Only clicking on it does."
//
// The cause is that activation never loaded anything. Base.Update's enter case TOGGLES
// FOCUS between the list and the detail and returns, while every other way of choosing
// a row loads it:
//
//	up/down   → curTable().Move(±1); return b.loadDetail()
//	space     → ToggleMark;        return b.loadDetail()
//	mouse     → table.Click(row);  return b.loadDetail()
//	enter     → b.focusD = !b.focusD   ... and nothing else
//
// That is why a click opens a conversation and Enter does not: the Ask rail OPENS a
// conversation from its detail landing (onDetail → shell.OpenAskConversation), so with
// no load there is no landing and nothing opens.
//
// The comment above that case already claimed the right behaviour — "otherwise
// activation opens the row's detail" — so the code and its own documentation disagreed.
// These tests pin the documentation.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// enterHarness builds a Base with one source whose rows have no detail loaded yet.
func enterHarness(t *testing.T) (*Base, *[]string) {
	t.Helper()
	b := &Base{}
	b.HideSources = true
	b.SetSize(120, 40)
	b.AddSource("things", "Things", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "t1", Title: "one"}, {ID: "t2", Title: "two"}}, "", nil
	})
	asked := &[]string{}
	b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
		*asked = append(*asked, src+"/"+id)
		return "detail " + id, nil, "", nil
	})
	b.SelectSource("things")
	b.LoadItems("things", []Item{{ID: "t1", Title: "one"}, {ID: "t2", Title: "two"}}, "")
	// LoadItems sets rows and a cursor but loads NO detail — which is the state the
	// operator is in when they arrive on the screen and press Enter.
	b.ClearDetail()
	return b, asked
}

// THE BUG. Enter must load the selected row, because that load is what opens it.
func TestEnterLoadsTheSelectedRow(t *testing.T) {
	b, asked := enterHarness(t)
	if b.DetailID() != "" {
		t.Fatal("fixture: the detail should start empty")
	}

	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter produced NO command: activation does not load the selected row, so nothing " +
			"opens. This is the operator's report — a click opens a conversation, Enter does nothing")
	}
	b.Update(cmd())

	if len(*asked) == 0 {
		t.Fatal("Enter produced a command but the detail was never requested")
	}
	if got := (*asked)[len(*asked)-1]; !strings.HasSuffix(got, "/t1") {
		t.Fatalf("Enter loaded %q, want the SELECTED row (t1)", got)
	}
	if b.DetailID() != "t1" {
		t.Errorf("detailID = %q, want t1", b.DetailID())
	}
}

// And Enter still moves focus to the detail, which is what it did before and what the
// Ask screen's own hint advertises ("enter: detail focus").
func TestEnterStillFocusesTheDetail(t *testing.T) {
	b, _ := enterHarness(t)
	if b.focusD {
		t.Fatal("fixture: the list should have focus")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !b.focusD {
		t.Error("Enter did not move focus to the detail")
	}
}

// A row whose detail is ALREADY showing must not be re-fetched on Enter: Enter there
// means "move me into what I am looking at". Re-loading would re-run the detail hook —
// and for a live surface that is a transcript reset, not a refresh.
func TestEnterDoesNotReloadTheRowAlreadyShowing(t *testing.T) {
	b, asked := enterHarness(t)

	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	b.Update(fetchOrNil(t, b))
	before := len(*asked)
	if before == 0 {
		t.Fatal("fixture: the first Enter should have loaded t1")
	}

	// Focus is now on the detail; move back to the list without changing the row.
	b.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if b.focusD {
		t.Fatal("fixture: left should return focus to the list")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := len(*asked); got != before {
		t.Errorf("Enter re-loaded the row already in the detail (%d -> %d requests); the detail "+
			"hook is expected to run once per row change", before, got)
	}
	if !b.focusD {
		t.Error("Enter did not move focus to the detail")
	}
}

// fetchOrNil re-runs the detail load for the selection, so a following Enter sees a
// populated detail. It is the test's stand-in for the shell finishing a fetch.
func fetchOrNil(t *testing.T, b *Base) tea.Msg {
	t.Helper()
	if cmd := b.loadDetail(); cmd != nil {
		return cmd()
	}
	return nil
}

// SPACE MARKS ONLY WHERE A BULK ACTION CAN CONSUME THE MARK.
//
// The operator: "spacebar does multi-select even on theme lists. That doesn't make
// any sense as there isn't any bulk operations on themes." A mark is only meaningful
// because something acts on it; on a list with nothing to act, it is invisible state
// the operator can neither use nor explain. There, Space does what Enter does.
func TestSpaceMarksOnlyOnAMarkableList(t *testing.T) {
	build := func(markable bool) (*Base, *int) {
		b := &Base{}
		b.HideSources = true
		b.SetSize(120, 40)
		b.AddSource("things", "Things", func(ctx context.Context, pageToken string) ([]Item, string, error) {
			return nil, "", nil
		})
		b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
			return "d", nil, "", nil
		})
		activations := 0
		b.OnActivate = func() (bool, tea.Cmd) {
			activations++
			return true, nil
		}
		b.SetMarkable("things", markable)
		b.SelectSource("things")
		b.LoadItems("things", []Item{{ID: "t1", Title: "one"}, {ID: "t2", Title: "two"}}, "")
		return b, &activations
	}

	t.Run("markable list: space marks", func(t *testing.T) {
		b, activations := build(true)
		b.Update(tea.KeyMsg{Type: tea.KeySpace})
		// MarkCount, not BulkIDs: BulkIDs reports a BULK SELECTION (>1 marked) and is
		// deliberately nil for a single mark, so asserting on it would test the
		// threshold rather than whether space marked anything.
		if got := b.curTable().MarkCount(); got != 1 {
			t.Errorf("space marked %d rows, want 1", got)
		}
		if *activations != 0 {
			t.Error("space ACTIVATED on a markable list instead of marking")
		}
	})

	t.Run("unmarkable list: space activates and marks nothing", func(t *testing.T) {
		b, activations := build(false)
		b.Update(tea.KeyMsg{Type: tea.KeySpace})
		if got := b.curTable().MarkCount(); got != 0 {
			t.Errorf("space marked %d rows on a list with no bulk operations — the mark is "+
				"unusable state", got)
		}
		if *activations != 1 {
			t.Errorf("activations = %d, want 1 — on a list with no bulk ops space should do what "+
				"Enter does", *activations)
		}
	})
}
