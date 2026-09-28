package kit2

// group_row_detail_test.go — a CATEGORY row has no detail to load, and asking the server for one is
// not merely wasted: its id is a sentinel that the server cannot accept.
//
// Found by driving the real TUI while capturing screenshots. The Workflows pane opened showing
//
//	couldn't load this item
//	  error internal: db: get workflow: ERROR: invalid byte sequence for encoding "UTF8": 0x00
//	  (SQLSTATE 22021)
//
// before anything had been selected — because the FIRST row of a grouped list is its category folder,
// and a folder's id is `screenkit.groupRowIDPrefix + <category>` (a NUL byte, chosen precisely so it
// can never collide with a ULID). `loadDetail` guarded an EMPTY id but not a SENTINEL one, so the
// Workflows detail function passed the sentinel straight to Postgres, which rejects a NUL in a query
// parameter outright. Every other source happened to ignore an unknown id, which is why only this pane
// showed it.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

func groupRowHarness(t *testing.T) (*Base, *[]string) {
	t.Helper()
	b := &Base{}
	b.HideSources = true
	b.SetSize(120, 40)
	b.AddSource("workflows", "Workflows", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return nil, "", nil
	})
	asked := &[]string{}
	b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
		*asked = append(*asked, src+"/"+id)
		return "detail " + id, nil, "", nil
	})
	b.SelectSource("workflows")
	// The first row is a FOLDER, exactly as the grouped workflows list renders it.
	folder := screenkit.GroupRowID("software-development")
	b.LoadItems("workflows", []Item{
		{ID: folder, Title: "Software Development", HasChildren: true},
		{ID: "01KZ1W513F25ASPZM1XW4ZJ2MB", Title: "SDLC (Human)"},
	}, "")
	b.ClearDetail()
	return b, asked
}

// A folder row must produce NO detail request at all.
func TestLoadingAFolderRowNeverAsksTheServer(t *testing.T) {
	b, asked := groupRowHarness(t)
	if id := b.curTable().SelectedID(); !screenkit.IsGroupRow(id) {
		t.Fatalf("fixture: the selected row should be the folder, got %q", id)
	}
	cmd := b.loadDetail()
	if cmd == nil {
		return // nothing requested: the desired behaviour
	}
	b.Update(cmd())
	if len(*asked) != 0 {
		t.Fatalf("a FOLDER row asked the server for detail: %v — its id is a NUL-prefixed sentinel, "+
			"and a source whose detail reaches the database rejects it outright (SQLSTATE 22021)", *asked)
	}
}

// And an ITEM row in the same list still loads normally — the guard must not smother the source.
func TestAnItemRowBelowAFolderStillLoads(t *testing.T) {
	b, asked := groupRowHarness(t)
	b.Update(tea.KeyMsg{Type: tea.KeyDown})
	if id := b.curTable().SelectedID(); screenkit.IsGroupRow(id) {
		t.Fatalf("expected to have moved onto an item, still on the folder %q", id)
	}
	cmd := b.loadDetail()
	if cmd == nil {
		t.Fatal("an ITEM row produced no detail request")
	}
	b.Update(cmd())
	if len(*asked) != 1 {
		t.Fatalf("asked %v, want exactly one item load", *asked)
	}
}

// TestABackgroundRefreshDoesNotChangeThePanesHeight — the operator's "you can see the screen blink
// if you watch it", measured at the only layer that can answer it: the RENDER.
//
// The rolling window runs every five seconds (refresh.go) and re-reads the source plus the open
// detail. Nothing about the DATA is wrong when it does, so a test that checked the fetched items
// would pass while the pane flickered. This one compares the rendered frame across a background
// refresh instead.
//
// WHY IT IS HERE, AND WHAT IT IS NOT. It was written while chasing that report, alongside a
// production change that suppressed the table's loading flag on background refreshes — because the
// theory was that a `loading…` line was being added and removed every tick. MEASURING FALSIFIED THE
// THEORY: in kit2 the table's `Loading` flag is never rendered at all (nothing calls Base.Loading(),
// and no capture of the Workflows pane contains the word "loading"), so that change was a no-op built
// on a wrong premise and it was reverted rather than committed.
//
// What the test DOES establish is the invariant that has to hold either way: a background refresh is
// invisible. It is kept because it is cheap, it is true today, and a future change that makes the
// refresh visible — in the list OR the detail — fails here rather than in the operator's eyes.
func TestABackgroundRefreshDoesNotChangeThePanesHeight(t *testing.T) {
	// SOURCES VISIBLE: this measures the LIST pane's render, and HideSources would draw only the
	// detail pane.
	b := &Base{}
	b.SetSize(120, 40)
	items := []Item{{ID: "wf-1", Title: "SDLC (Non-human)"}, {ID: "wf-2", Title: "SDLC (Human)"}}
	b.AddSource("workflows", "Workflows", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return items, "", nil
	})
	cmd := b.loadSource(0, "")
	if cmd == nil {
		t.Fatal("fixture: the source has no fetch")
	}
	if handled, _ := b.Update(cmd()); !handled {
		t.Fatal("fixture: the fetched message was not handled")
	}
	before := b.View()
	if !strings.Contains(before, "SDLC (Non-human)") {
		t.Fatalf("fixture: the rows did not land:\n%s", before)
	}

	// A background refresh, with the fetch still in flight: what is on screen must not move.
	refresh := b.RefreshView()
	if refresh == nil {
		t.Fatal("RefreshView returned nothing — nothing would ever refresh")
	}
	during := b.View()
	if during != before {
		t.Errorf("the background refresh CHANGED the render — the pane blinks on every tick.\n"+
			"--- before (%d lines)\n--- during (%d lines)",
			strings.Count(before, "\n"), strings.Count(during, "\n"))
	}
	// And after it lands, for the same reason.
	if handled, _ := b.Update(refresh()); !handled {
		t.Fatal("the refresh produced no handled message")
	}
	if after := b.View(); after != before {
		t.Errorf("the refresh LANDING changed the render (\n%s\n)", after)
	}
}

// TestAScreenPaintingItsOwnDetailIsNotRefreshedOver — the operator's "it seems to be going back and
// forth between two screens very quickly", as a test.
//
// The rolling window runs every five seconds (refresh.go) and kit2's default RefreshView re-requests
// the READ-ONLY detail for the selected row. A screen with its OWN detail renderer — the workflow
// FLOW editor — was therefore overwritten by that fetch and then repainted itself over the top: two
// bodies alternating, once per tick, for ever. The operator captured both forms.
//
// THE GUARD COULD NOT SEE IT. It refuses while `editForm` is open, which covers the INLINE form
// editor; the workflow edit mode is the SCREEN's own flag, and kit2 has no way to know it exists. So
// the screen answers the question instead, and this pins both halves: the default still refreshes,
// and an owning screen is left alone.
func TestAScreenPaintingItsOwnDetailIsNotRefreshedOver(t *testing.T) {
	b := &Base{}
	b.SetSize(120, 40)
	b.AddSource("workflows", "Workflows", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "wf-1", Title: "SDLC (Non-human)"}}, "", nil
	})
	b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
		return "Workflow: SDLC (Non-human)", nil, "FLOW\nVERSIONS", nil
	})
	// Land the rows, then settle the auto-detail the landing asks for — a real screen does the same,
	// and it keeps the fixture from holding a pending command.
	if handled, cmd := b.Update(b.loadSource(0, "")()); !handled {
		t.Fatal("fixture: the rows did not land")
	} else if cmd != nil {
		b.Update(cmd())
	}
	if !strings.Contains(b.View(), "SDLC (Non-human)") {
		t.Fatalf("fixture: the row is not on screen:\n%s", b.View())
	}

	// WITH NO OWNER (every other screen) the refresh still runs — the fix must not disable it.
	if cmd := b.RefreshView(); cmd == nil {
		t.Fatal("a screen with no detail owner must still refresh")
	}

	// WITH AN OWNER, the whole refresh is refused: re-loading the source would also re-seat the list
	// and the cursor underneath an operator who is editing steps.
	owned := true
	b.SetDetailPaintOwner(func() bool { return owned })
	if cmd := b.RefreshView(); cmd != nil {
		t.Fatal("a screen painting its own detail must not be refreshed over — the pane would " +
			"alternate between the two bodies on every tick")
	}

	// And it resumes the moment the screen stops owning the paint (the editor closed), so the
	// refusal is a mode and not a permanent mute.
	owned = false
	if cmd := b.RefreshView(); cmd == nil {
		t.Fatal("the refresh must resume once the screen stops painting its own detail")
	}
}
