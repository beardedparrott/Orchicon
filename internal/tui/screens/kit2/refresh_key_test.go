package kit2

// refresh_key_test.go — `r` must re-read the OPEN DETAIL, not just the list.
//
// REPORTED: "Hitting 'r' in the tui to refresh a workflow run is not working."
//
// It was doing something; it was doing the wrong thing. `r` called Refresh(activeSource), which reloads the
// source LIST — and a run's detail (its step flow, its statuses, its new error field) is produced by the
// DETAIL function, which only RefreshView re-requests. So on a run row the key re-read the list, which is
// invisible because the cursor re-seats by id, and left the detail stale. From the operator's side: nothing.

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// loadAndSelect puts one row in a source and opens its detail.
func loadAndSelect(t *testing.T, b *Base, detailCalls *int) {
	t.Helper()
	b.SetSize(120, 40)
	b.AddSource("runs", "Runs", func(ctx context.Context, pageToken string) ([]Item, string, error) {
		return []Item{{ID: "run-1", Title: "A run"}}, "", nil
	})
	b.SetDetail(func(ctx context.Context, src, id string) (string, []Field, string, error) {
		*detailCalls++
		return "Run " + id, []Field{{Key: "status", Value: "failed"}}, "body", nil
	})
	if cmd := b.loadSource(0, ""); cmd != nil {
		b.Update(cmd())
	}
	if cmd := b.RequestDetail("runs", "run-1"); cmd != nil {
		b.Update(cmd())
	}
}

// THE REPORTED KEY: pressing `r` must re-request the detail.
func TestRefreshKeyReRequestsTheOpenDetail(t *testing.T) {
	b := &Base{}
	calls := 0
	loadAndSelect(t, b, &calls)
	before := calls
	if before == 0 {
		t.Fatal("fixture: the detail was never requested")
	}

	handled, cmd := b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !handled {
		t.Fatal("`r` was not handled at all")
	}
	if cmd == nil {
		t.Fatal("`r` produced no command — nothing would be re-read, which is the reported 'does nothing'")
	}
	// The command must actually re-fetch: RUN it and assert the DETAIL function was called again.
	//
	// tea.Batch resolves to a BatchMsg, which only the Bubble Tea runtime expands into its parts — so a test
	// that simply calls Update(batch) executes NOTHING and would "pass" while nothing happened. Draining it
	// here is what makes this assertion about behaviour rather than about the shape of a return value.
	drain(t, b, cmd)
	if calls <= before {
		t.Fatalf("`r` re-read the list but NOT the open detail (%d -> %d calls) — a run's step flow would "+
			"stay stale, which is what the operator saw as the key doing nothing", before, calls)
	}
}

// drain executes a command and every command inside a tea.Batch, feeding each message back to the screen —
// the expansion the Bubble Tea runtime performs and a unit test must reproduce.
func drain(t *testing.T, b *Base, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			drain(t, b, sub)
		}
		return
	}
	if msg != nil {
		b.Update(msg)
	}
}

// ...and it must not be swallowed by an open editor: the same guards RefreshView carries apply, because it IS
// RefreshView now.
func TestRefreshKeyIsSilentWhileEditing(t *testing.T) {
	b := &Base{}
	calls := 0
	loadAndSelect(t, b, &calls)
	b.BeginDetailEdit("editing", nil)
	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	// Either the editor consumed it (no refresh) or nothing happened — what must NOT happen is a refresh that
	// re-seats rows under the operator mid-edit.
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isFetched := msg.(fetchedMsg); isFetched {
				t.Fatal("`r` refreshed the list while an inline editor was open — that re-seats rows and the " +
					"cursor under the operator mid-edit")
			}
		}
	}
}
