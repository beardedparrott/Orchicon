package execution

// bulk_delete_cursor_test.go — A MARKED SELECTION IS WHAT THE DELETE CHORD ACTS ON.
//
// The operator: "If I bulk select items but then move the selector onto a single item and hit ctrl+x, it
// asks to delete the item you have currently selected versus the bulk items you selected. That is wrong."
//
// REPRODUCED, and the mechanism is a count that disagreed with a write:
//
//	marked=[ grp:cat-1 a1 ]                                  <- TWO rows marked, as the operator was told
//	hint:  2 marked · ctrl+x: delete 2                        <- the UI PROMISED two
//	delete: label="delete" confirm="Delete worker three?"     <- the confirm named the CURSOR's row
//
// A category FOLDER row can be marked (it is a row) but markableIDs filters it out, so the selection
// contained only ONE writable row — below BulkThreshold — and the bulk action did not exist. The chord
// then fell through to the list's SINGLE-ROW delete: the operator marks two things and is offered a
// third, which is the worst shape a destructive chord can take.
//
// TWO FIXES, both asserted here:
//
//  1. THE COUNT THE OPERATOR SEES IS THE COUNT THE WRITE TOUCHES. The hint used MarkCount (every mark)
//     while the actions used markableIDs, so it advertised a number the confirm would not honour.
//  2. A MARKED SELECTION WITH NO BULK ACTION REFUSES AND SAYS WHY, instead of retargeting onto the
//     cursor. The check has to run BEFORE the action lookup, because the single-row action always
//     matches — placing it after made it unreachable.

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// bulkPane focuses src with rows loaded, and returns the model.
func bulkPane(t *testing.T, src string, items []kit2.Item) *Model {
	t.Helper()
	m := newModel(t, execPlane())
	if !m.Base.SelectSource(src) {
		t.Fatalf("fixture: cannot focus %s", src)
	}
	if !m.Base.LoadItems(src, items, "") {
		t.Fatalf("fixture: cannot load %s", src)
	}
	m.refreshActionBar()
	return m
}

// rowsFor is three ordinary rows on the pane.
func rowsFor(src string) []kit2.Item {
	return []kit2.Item{
		{ID: "a1", Title: "one", Meta: "running"},
		{ID: "a2", Title: "two", Meta: "running"},
		{ID: "a3", Title: "three", Meta: "running"},
	}
}

// deleteChordFor is the chord the pane actually binds (schedules deletes on `x`).
func deleteChordFor(src string) string {
	if src == srcSchedules {
		return keySchedDelete
	}
	return kit2.DeleteChord
}

// mark marks the given row ids, in order.
func markRows(t *testing.T, m *Model, src string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if !m.Base.SelectItem(src, id) {
			t.Fatalf("fixture: cannot select %q on %s", id, src)
		}
		m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	}
}

// ── 1. the healthy case: a real bulk selection names the whole set ───────────────────────────────

// THE CONFIRM NAMES THE MARKED SET, not the cursor. Checked on every pane that offers a bulk delete,
// with the cursor deliberately parked on an UNMARKED row.
func TestBulkDeleteNamesTheMarkedSetNotTheCursor(t *testing.T) {
	for _, src := range []string{srcWorkers, srcWorkflows, srcExecutions, srcRuns, srcSchedules} {
		t.Run(src, func(t *testing.T) {
			m := bulkPane(t, src, rowsFor(src))
			markRows(t, m, src, "a1", "a2")
			// Move the selector onto the THIRD, unmarked row — the operator's gesture.
			m.Base.SelectItem(src, "a3")

			_, handled := m.handleActionKey(deleteChordFor(src))
			if !handled {
				t.Fatalf("%s: the delete chord was not handled", src)
			}
			if !m.DialogOpen() {
				t.Fatalf("%s: no confirm was raised for a two-row bulk selection (notice=%q)",
					src, m.Notice())
			}
			body := m.Open.Body
			// The confirm must be about TWO rows — the marked set — and must not be about the cursor's.
			if !strings.Contains(body, "2") {
				t.Errorf("%s: the confirm does not name the marked set: %q", src, body)
			}
			if strings.Contains(body, "three") {
				t.Errorf("%s: the confirm names the CURSOR's row (the unmarked one) — the operator's "+
					"\"it asks to delete the item you have currently selected versus the bulk items you "+
					"selected\": %q", src, body)
			}
		})
	}
}

// ── 2. the bug's case: marks that yield no bulk action must refuse ───────────────────────────────

// A SELECTION CONTAINING A CATEGORY FOLDER MUST NOT RETARGET ONTO THE CURSOR.
//
// This is the reproduction. One writable row plus a folder is TWO marks — enough that the operator
// expects a bulk operation — but only one is writable, so there is no bulk action. Before the fix the
// chord opened the confirm for whatever the cursor was on.
func TestAFolderInTheMarksRefusesInsteadOfRetargeting(t *testing.T) {
	for _, src := range []string{srcWorkers, srcWorkflows} {
		t.Run(src, func(t *testing.T) {
			items := append([]kit2.Item{
				{ID: screenkit.GroupRowID("cat-1"), Title: "Nightly", HasChildren: true},
			}, rowsFor(src)...)
			m := bulkPane(t, src, items)
			markRows(t, m, src, "a1", screenkit.GroupRowID("cat-1"))
			if got := len(m.Base.MarkedIDs()); got != 2 {
				t.Fatalf("fixture: expected two marks, got %d", got)
			}
			// Park the cursor on a row that is NOT in the marks — what makes a retarget visible.
			m.Base.SelectItem(src, "a3")

			_, handled := m.handleActionKey(deleteChordFor(src))
			if !handled {
				t.Fatalf("%s: the delete chord was not handled", src)
			}
			if m.DialogOpen() {
				t.Errorf("%s: the chord opened a confirm (%q) while the marks yield no bulk delete — "+
					"the operator's \"it asks to delete the item you have currently selected versus the "+
					"bulk items you selected\"", src, strings.SplitN(m.Open.Body, "\n", 2)[0])
			}
			notice := m.Notice()
			if notice == "" {
				t.Fatalf("%s: the refusal was silent — a chord that appears to do nothing reads as broken", src)
			}
			if !strings.Contains(notice, "marked") {
				t.Errorf("%s: the refusal does not mention the marks: %q", src, notice)
			}
			// AND NOTHING WAS DELETED.
			if strings.Contains(m.Notice(), "deleted") && !strings.Contains(m.Notice(), "Nothing was deleted") {
				t.Errorf("%s: the refusal implies a write happened: %q", src, notice)
			}
		})
	}
}

// ── 3. the count the operator reads is the count the write touches ───────────────────────────────

// THE HINT AND THE CONFIRM AGREE. The hint said "3 marked"-worth of rows while the confirm named 2,
// because the hint counted every mark and the actions counted only the writable ones.
func TestTheHintCountsWhatTheWriteWillTouch(t *testing.T) {
	items := append([]kit2.Item{
		{ID: screenkit.GroupRowID("cat-1"), Title: "Nightly", HasChildren: true},
	}, rowsFor(srcWorkers)...)
	m := bulkPane(t, srcWorkers, items)
	markRows(t, m, srcWorkers, "a1", "a2", screenkit.GroupRowID("cat-1"))

	if got := len(m.Base.MarkedIDs()); got != 3 {
		t.Fatalf("fixture: expected three marks, got %d", got)
	}
	hint := stripANSI(m.HintLine())
	if !strings.Contains(hint, "2 marked") {
		t.Errorf("the hint does not state the WRITABLE count (2 of 3 marks are workers): %q", hint)
	}
	if strings.Contains(hint, "delete 3") {
		t.Errorf("the hint promises a delete of THREE while the write can only touch two: %q", hint)
	}
	// And the confirm agrees with the hint.
	if _, ok := m.actionByKey(kit2.DeleteChord); !ok {
		t.Fatal("fixture: no delete action")
	}
	acts := m.actionsForSelection()
	for _, a := range acts {
		if a.Key != kit2.DeleteChord {
			continue
		}
		if !strings.Contains(a.Confirm, "2 workers") {
			t.Errorf("the confirm does not name the writable count the hint stated: %q", a.Confirm)
		}
	}
}

// A REFUSAL MUST NOT SWALLOW A KEY THE PANE DOES NOT OWN. The check is gated on the pane's OWN delete
// chord, so an unrelated key still falls through to the navigation layer.
func TestTheRefusalOnlyClaimsThePanesOwnDeleteChord(t *testing.T) {
	m := bulkPane(t, srcWorkers, rowsFor(srcWorkers))
	markRows(t, m, srcWorkers, "a1")
	// `x` is the Work panes' delete alias, NOT this pane's chord; unrelated keys must not be eaten.
	for _, key := range []string{"up", "down", "j", "k", "r", "enter"} {
		if _, handled := m.handleActionKey(key); handled {
			t.Errorf("the refusal claimed %q, a key this pane does not bind as a delete", key)
		}
	}
	_ = fmt.Sprint
}
