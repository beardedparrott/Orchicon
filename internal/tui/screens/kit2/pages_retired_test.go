package kit2

// pages_retired_test.go — the pane fetches the WHOLE set, and there is no page concept left.
//
// The operator: "I don't understand concept of 'pages'. It's mentioned in the TUI, yet I never page to
// see more work items. I just hold the down arrow and I should be able to see all of them. Same goes
// for any other item in the TUI. Pages are unnecessary imo."
//
// They are right, from the operator's side: the pane IS a scrolling list, and holding DOWN must reach
// the end of it. So the base walks the RPC's cursors INTERNALLY (loadSource) and never surfaces a
// next-page token — which is what removes the "more pages: press f" affordance: not a hidden key, but
// the absence of anything left unfetched.
//
// These tests pin the two halves of that contract: every page is collected, and no token reaches the
// table (so no marker can render).

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pagingSource is a fetch that serves FIXED-SIZE pages, so a single call would truncate — which is
// exactly the situation the operator was in (they held down and never reached the end).
func pagingSource(pageSize int, total int) func(context.Context, string) ([]Item, string, error) {
	return func(_ context.Context, pageToken string) ([]Item, string, error) {
		start := 0
		if pageToken != "" {
			// The token is the last id of the previous page; find it and continue.
			for i := 0; i < total; i++ {
				if "wi-"+itoa(i) == pageToken {
					start = i + 1
					break
				}
			}
		}
		end := start + pageSize
		if end > total {
			end = total
		}
		items := make([]Item, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, Item{ID: "wi-" + itoa(i), Title: "Item " + itoa(i)})
		}
		next := ""
		if end < total {
			next = items[len(items)-1].ID
		}
		return items, next, nil
	}
}

func itoa(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// EVERY page is collected: the pane's fetch returns the whole set even though each RPC call returns a
// page. This is the operator's "I should be able to see all of them".
func TestSourceFetchCollectsEveryPage(t *testing.T) {
	b := &Base{}
	// 30 items, 10 per page: three RPC calls behind one screen fetch.
	b.AddSource("things", "Things", pagingSource(10, 30))

	cmd := b.loadSource(0, "")
	if cmd == nil {
		t.Fatal("loadSource produced no command")
	}
	msg, ok := cmd().(fetchedMsg)
	if !ok {
		t.Fatalf("got %T, want fetchedMsg", cmd())
	}
	if msg.err != nil {
		t.Fatalf("fetch: %v", msg.err)
	}
	if len(msg.items) != 30 {
		t.Fatalf("collected %d items, want all 30 — the pane must reach the end of the list", len(msg.items))
	}
	// And the LAST item is present: the whole point is that holding DOWN gets there.
	if msg.items[len(msg.items)-1].Title != "Item 29" {
		t.Errorf("last item = %q, want Item 29", msg.items[len(msg.items)-1].Title)
	}
}

// NO page token reaches the table, so nothing can advertise a next page.
//
// This is what makes "more pages: press f" impossible rather than merely unwritten: the marker
// rendered on a non-empty NextPageToken, and after a whole-set load there is nothing unfetched.
func TestSourceFetchNeverReportsANextPage(t *testing.T) {
	b := &Base{}
	b.AddSource("things", "Things", pagingSource(10, 30))

	msg := b.loadSource(0, "")()
	fm := msg.(fetchedMsg)
	if fm.next != "" {
		t.Errorf("the fetch reported a next-page token (%q) — there is nothing left unfetched, so no "+
			"pager may be advertised", fm.next)
	}
	// Feed it through Update and confirm the TABLE carries no token either.
	b.Update(fm)
	if tok := b.sources[0].table.NextPageToken; tok != "" {
		t.Errorf("the table holds a next-page token (%q) — a pager would render for it", tok)
	}
}

// A single-page source still works (no needless extra calls, no token).
func TestSourceFetchSinglePage(t *testing.T) {
	b := &Base{}
	calls := 0
	b.AddSource("things", "Things", func(_ context.Context, _ string) ([]Item, string, error) {
		calls++
		return []Item{{ID: "wi-1", Title: "Only"}}, "", nil
	})
	msg := b.loadSource(0, "")()
	fm := msg.(fetchedMsg)
	if len(fm.items) != 1 || fm.next != "" {
		t.Fatalf("items=%d next=%q, want 1 and no token", len(fm.items), fm.next)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// A FAILURE ON A LATER PAGE keeps what was already fetched: a partial list beats an empty pane, and
// the operator keeps the rows they can already see. A failure on the FIRST page is still an error.
func TestSourceFetchKeepsPartialResultsOnALaterPageFailure(t *testing.T) {
	b := &Base{}
	calls := 0
	b.AddSource("things", "Things", func(_ context.Context, pageToken string) ([]Item, string, error) {
		calls++
		if calls == 1 {
			return []Item{{ID: "wi-1", Title: "First"}}, "wi-1", nil
		}
		return nil, "", context.DeadlineExceeded
	})
	fm := b.loadSource(0, "")().(fetchedMsg)
	if fm.err != nil {
		t.Errorf("a later-page failure surfaced as an error (%v) — the rows already fetched must survive", fm.err)
	}
	if len(fm.items) != 1 {
		t.Errorf("items = %d, want the 1 already fetched", len(fm.items))
	}

	// First-page failure: a real error state.
	b2 := &Base{}
	b2.AddSource("things", "Things", func(_ context.Context, _ string) ([]Item, string, error) {
		return nil, "", context.DeadlineExceeded
	})
	fm2 := b2.loadSource(0, "")().(fetchedMsg)
	if fm2.err == nil {
		t.Error("a first-page failure was swallowed — the pane must show the error")
	}
}

// `f` is NOT a pager any more: nothing is left unfetched, and the base does not claim the key.
//
// It falls through rather than refusing, because a refusal for a concept that no longer exists would
// be noise — and because a screen may legitimately want `f` for its own chord (the execution screen's
// message-box stub does).
func TestFIsNoLongerThePager(t *testing.T) {
	b := &Base{}
	calls := 0
	b.AddSource("things", "Things", func(_ context.Context, _ string) ([]Item, string, error) {
		calls++
		return []Item{{ID: "wi-1", Title: "Only"}}, "", nil
	})
	b.Update(b.loadSource(0, "")())
	b.SelectSource("things")
	before := calls

	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	// `f` must not trigger a load. (The key is free for a screen to claim; the BASE must not.)
	if calls != before {
		t.Errorf("f issued %d extra load(s) — the pager is retired", calls-before)
	}
}
