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
