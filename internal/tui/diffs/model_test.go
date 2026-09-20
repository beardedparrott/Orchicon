package diffs

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// TestModelSelectAndCopy exercises the pane's file selection + OSC 52 copy
// binding (`y`), and the tab switcher keyboard binding.
func TestModelSelectAndCopy(t *testing.T) {
	vecs := vecByName(t)
	modify := vecs["modify-two-lines"]
	create := vecs["create-new-file"]
	model := NewModel(nil, nil)
	model.Width, model.Height = 80, 24
	model.groups = GroupByFile([]*apiv1.FileEdit{makeEdit(create, 1), makeEdit(modify, 2)})
	model.SelectedPath = create.Path
	model.rows = model.rowsForSelected()

	// Select the modify file; its rows derive from modify-two-lines (3 rows).
	model.SelectPath(modify.Path)
	if len(model.rows) == 0 {
		t.Fatal("selected modify file produced no rows")
	}

	// `y` stages an OSC 52 copy whose payload is the verbatim unified diff.
	copied := model.CopySelectedDiff()
	if !copied {
		t.Fatal("CopySelectedDiff returned false on a file with a diff")
	}
	if !strings.HasPrefix(model.copyBuf, "\x1b]52;c;") || !strings.HasSuffix(model.copyBuf, "\x1b\\") {
		t.Errorf("copy buffer not OSC 52: %q", model.copyBuf)
	}
	// The View() emits the pending copy once and clears it.
	out := model.View()
	if !strings.Contains(out, "\x1b]52;c;") {
		t.Errorf("View() did not emit the OSC 52 copy")
	}
	if model.copyActive {
		t.Errorf("copy still pending after View()")
	}

	// Tab switcher via keyboard (h/l).
	model.SetTab(TabTimeline)
	model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if model.Tab != TabDiff {
		t.Errorf("l from timeline should switch to diff, got %s", model.Tab)
	}
}

// TestModelScrollClamps ensures the viewport scroll never goes negative or
// past the row count (no tearing/truncation artifacts).
func TestModelScrollClamps(t *testing.T) {
	vecs := vecByName(t)
	model := NewModel(nil, nil)
	model.Height = 5
	model.rows = ParseUnifiedDiff(deref(vecs["modify-two-lines"].ExpectedUnifiedDiff))
	model.Scroll(-10)
	if model.scroll != 0 {
		t.Errorf("scroll below zero = %d, want 0", model.scroll)
	}
	model.Scroll(1000)
	if model.scroll > model.lineCount()-1 {
		t.Errorf("scroll past end = %d, want <= %d", model.scroll, model.lineCount()-1)
	}
}

// TestMergeEditsDedupAndResume mirrors the GUI mergeEdits discipline:
// live edits are appended only when seq > durable max and deduped by id.
func TestMergeEditsDedupAndResume(t *testing.T) {
	durable := []*apiv1.FileEdit{
		{Id: "a", Seq: 1},
		{Id: "b", Seq: 2},
	}
	live := []*apiv1.FileEdit{
		{Id: "b", Seq: 2}, // dup of durable → dropped
		{Id: "c", Seq: 5},
		{Id: "d", Seq: 1}, // seq <= max durable (2) → dropped
	}
	merged := MergeEdits(durable, live)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3 (a,b,c)", len(merged))
	}
	// Chronological ascending.
	if merged[0].GetId() != "a" || merged[1].GetId() != "b" || merged[2].GetId() != "c" {
		t.Errorf("merge order = %s,%s,%s", merged[0].GetId(), merged[1].GetId(), merged[2].GetId())
	}
	// Out-of-order live events: the port mirrors the GUI mergeEdits — it
	// iterates live in order and updates maxDurableSeq, so a later live
	// event with a LOWER seq is dropped (the GUI pre-sorts its live list
	// ascending, so this never happens with a real monotonic stream).
	live2 := []*apiv1.FileEdit{{Id: "f", Seq: 7}, {Id: "e", Seq: 10}}
	merged2 := MergeEdits(durable, live2)
	if len(merged2) != 4 {
		t.Fatalf("out-of-order live merged len = %d, want 4", len(merged2))
	}
	// Output sorted ascending (a,b,f,e).
	expectedOrder := []string{"a", "b", "f", "e"}
	for i, want := range expectedOrder {
		if merged2[i].GetId() != want {
			t.Errorf("merged2[%d] = %s, want %s", i, merged2[i].GetId(), want)
		}
	}
}
