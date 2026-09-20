package execution

// chords_test.go — the delete chord and the bulk set-model chord.
//
// THE OPERATOR, correcting their own report and then stating the requirement plainly:
//
//	"Bulk and single delete is working for Workflows but it's 'Shift+x' and not 'Ctrl+x' and it does work
//	 for workers but it's 'x'. Let's make those consistent across the board please with 'ctrl+x' for single
//	 and bulk on both and ensure the shortcut text in the composer is there. I still want the bulk set
//	 model though with 'shift+m'."
//
// So there are three properties to pin, and NONE of them is "the constants hold the right string":
//
//	1. the SINGLE delete and the BULK delete answer the SAME chord, on BOTH panes;
//	2. the chord is CTRL+X — asserted as the literal, because a test that compares the constant to itself
//	   would pass with any value at all (which is how the panes ended up on `x` and `shift+x` while every
//	   test stayed green);
//	3. the OLD chords no longer delete, so the change is a move and not an addition.
//
// Plus the bulk set-model act: shift+m over the marked selection.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

// TestDeleteChordIsCtrlXOnBothPanesAndBothScopes is the requirement, read literally.
func TestDeleteChordIsCtrlXOnBothPanesAndBothScopes(t *testing.T) {
	if keyDelete != "ctrl+x" {
		t.Fatalf("the delete chord must be ctrl+x, got %q", keyDelete)
	}
	// The literal is asserted above; these two only have to AGREE with it, which is the other half of
	// "consistent across the board" — a folder row is deleted by the same key as the item inside it.
	if categoryDeleteChordForTest() != keyDelete {
		t.Fatalf("a folder row's delete chord (%q) must be the same as the items' (%q)",
			categoryDeleteChordForTest(), keyDelete)
	}

	for _, tc := range []struct {
		name   string
		source string
	}{
		{"workers", srcWorkers},
		{"workflows", srcWorkflows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := crudExec(t, nil, nil)
			if !m.Base.SelectSource(tc.source) {
				t.Fatalf("fixture: could not focus the %s pane", tc.name)
			}
			m.Base.LoadItems(tc.source, []screenkit.Item{
				{ID: "a1", Title: "one"}, {ID: "b2", Title: "two"},
			}, "")

			// SINGLE: the row's own delete action.
			var single string
			for _, a := range m.actionsForSelection() {
				if a.Label == "delete" {
					single = a.Key
				}
			}
			if single != "ctrl+x" {
				t.Fatalf("the single-row delete must answer ctrl+x, got %q", single)
			}

			// BULK: mark two rows and read the selection's actions.
			m.Base.SelectItem(tc.source, "a1")
			press(t, m, " ")
			press(t, m, " ")
			var bulk string
			for _, a := range m.actionsForSelection() {
				if strings.HasPrefix(a.Label, "delete ") && strings.HasSuffix(a.Label, " selected") {
					bulk = a.Key
				}
			}
			if bulk != "ctrl+x" {
				t.Fatalf("the bulk delete must answer ctrl+x, got %q", bulk)
			}
		})
	}
}

// TestOldDeleteChordsNoLongerDelete: the change is a MOVE, not an addition. `x` and `shift+x` must no
// longer raise a delete — otherwise the operator keeps two ways to delete and the inconsistency survives
// in their muscle memory.
func TestOldDeleteChordsNoLongerDelete(t *testing.T) {
	for _, tc := range []struct{ name, source, old string }{
		{"workers on x", srcWorkers, "x"},
		{"workflows on X", srcWorkflows, "X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := crudExec(t, nil, nil)
			if !m.Base.SelectSource(tc.source) {
				t.Fatalf("fixture: could not focus the pane")
			}
			m.Base.LoadItems(tc.source, []screenkit.Item{{ID: "a1", Title: "one"}}, "")

			if _, handled := m.handleActionKey(tc.old); handled {
				t.Fatalf("%q must not be handled as a delete any more", tc.old)
			}
			if m.DialogOpen() {
				t.Fatalf("%q opened a dialog — it must not delete anything", tc.old)
			}
		})
	}
}

// TestStepEditorKeepsX: the flow editor's `x` (remove step) is a WORKFLOW EDIT MODE chord, and the old
// delete chord had to be arranged AROUND it (`shift+x`, because lowercase `x` was taken). Moving delete
// to ctrl+x removes that collision entirely — but only if `x` still reaches the step editor.
//
// It uses the mode's own fixture (`modeModel`), which establishes the state the guard actually requires —
// a selected workflow AND a loaded detail — rather than reproducing half of it by hand and asserting about
// an empty pane.
func TestStepEditorKeepsX(t *testing.T) {
	m, _ := modeModel(t)
	// ENTER the mode through its real key, so this covers the whole path an operator takes: `e` for the
	// mode, then `x` for a step.
	if _, handled := m.handleActionKey(keyFlowEdit); !handled {
		t.Fatal("fixture: e must enter the workflow edit mode")
	}
	if !m.flowEditing {
		t.Fatal("fixture: the step editor's mode must be on")
	}

	if _, handled := m.handleActionKey("x"); !handled {
		t.Fatal("x must still reach the step editor while the flow mode is on")
	}
}

// TestBulkSetModelOpensForTheMarkedSelection: shift+m is the operator's bulk act, and it opens the model
// picker for EVERY marked worker (a per-worker model edit is a form field, so the modal is the only place
// a batch can go).
func TestBulkSetModelOpensForTheMarkedSelection(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}, "")
	m.SetShell(&groupSpy{categorizeSpy: &categorizeSpy{}})

	// The action bar advertises it once there is a selection, so it is discoverable as well as bound.
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")
	if !hasAction(m.actionsForSelection(), "set model 2 selected") {
		t.Fatalf("marking must offer the bulk set-model: %+v", m.actionsForSelection())
	}

	if _, handled := m.handleActionKey(keyBulkSetModel); !handled {
		t.Fatal("shift+m must be handled on the Workers pane")
	}
	if m.modelPicker == nil {
		t.Fatalf("shift+m must open the model picker (notice=%q)", m.notice)
	}
	if len(m.modelPickerWorkers) != 2 {
		t.Fatalf("the picker must carry the whole marked selection, got %v", m.modelPickerWorkers)
	}
	if m.modelPickerWorkers[0] != "w1" || m.modelPickerWorkers[1] != "w2" {
		t.Fatalf("the picker must carry the marked ids in list order, got %v", m.modelPickerWorkers)
	}
}

// TestBulkSetModelWritesOnceForEveryMarkedWorker drives the whole act through the REAL keys: mark, shift+m,
// change the model in the modal, and assert the ONE write carrying every id.
func TestBulkSetModelWritesOnceForEveryMarkedWorker(t *testing.T) {
	m, _, writes := newPickerExec(t)
	m.SelectSource(srcWorkers)
	// A SHARED current ref, so the picker opens seeded (a mixed selection has nothing to seed from) — and
	// the model is then CHANGED, so the assertion is about the ref that was chosen rather than the one
	// that was already there.
	m.workerMu.Lock()
	m.workerModel["w1"] = "orchicon/anthropic/claude-sonnet-4"
	m.workerModel["w2"] = "orchicon/anthropic/claude-sonnet-4"
	m.workerMu.Unlock()
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}, "")
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")

	if _, handled := m.handleActionKey(keyBulkSetModel); !handled || m.modelPicker == nil {
		t.Fatal("shift+m must open the picker")
	}
	// The three TIERS are covered by the picker's own tests, so the lists are seeded directly and the
	// MODEL IS MOVED — which is the part that has to reach the write.
	m.modelPicker.SetAdapters([]string{"orchicon"}, nil)
	m.modelPicker.SetProviders("orchicon", []kit2.PickerOption{{Value: "anthropic"}})
	m.modelPicker.SetModels("orchicon", "anthropic", []kit2.PickerOption{
		{Value: "claude-sonnet-4"}, {Value: "claude-opus-4"},
	}, false)
	m.modelPicker.HandleKey(kmsg("down"))
	m.modelPicker.HandleKey(kmsg("enter"))
	cmd := m.finishModelPicker(m.modelPicker)

	if len(*writes) != 0 {
		t.Fatalf("nothing may be written by the picker alone: %v", *writes)
	}
	runWrite(t, cmd)
	if len(*writes) != 1 {
		t.Fatalf("want ONE batched write, got %v", *writes)
	}
	if got := (*writes)[0]; got != "w1,w2=orchicon/anthropic/claude-opus-4" {
		t.Fatalf("the write must carry every marked worker with the CHOSEN ref, got %q", got)
	}
}

// TestBulkSetModelRefusesWithTooFewMarked: a chord that silently does nothing reads as broken, so both
// refusals name the route that DOES work.
func TestBulkSetModelRefusesWithTooFewMarked(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"},
	}, "")

	// NOTHING marked: point at the marking gesture.
	m.notice = ""
	if _, handled := m.handleActionKey(keyBulkSetModel); !handled {
		t.Fatal("shift+m must be handled with nothing marked, so it can explain itself")
	}
	if m.modelPicker != nil {
		t.Fatal("nothing marked must not open the picker")
	}
	if !strings.Contains(m.notice, "space") || !strings.Contains(m.notice, keyBulkSetModel) {
		t.Fatalf("the refusal must name both the marking key and the chord: %q", m.notice)
	}

	// EXACTLY ONE marked: one row is not a selection, and a single worker's model is a form field — so
	// the refusal must point at the form rather than at "mark more".
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	m.notice = ""
	if _, handled := m.handleActionKey(keyBulkSetModel); !handled {
		t.Fatal("shift+m must be handled with one marked, so it can explain itself")
	}
	if m.modelPicker != nil {
		t.Fatal("one marked row is not a bulk selection")
	}
	if !strings.Contains(m.notice, "press e") {
		t.Fatalf("the refusal must point at the form that edits one worker: %q", m.notice)
	}
}

// TestBulkSetModelSkipsACategoryFolder: a folder is a row and can be marked, but its id is synthetic —
// the picker must not be pointed at it, and it must not inflate the batch.
func TestBulkSetModelSkipsACategoryFolder(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	folderID := screenkit.GroupRowID("catA")
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: folderID, Title: "Sweepers", HasChildren: true},
		{ID: "w1", Title: "one", Parent: folderID, Depth: 1},
		{ID: "w2", Title: "two", Parent: folderID, Depth: 1},
	}, "")
	m.SetShell(&groupSpy{categorizeSpy: &categorizeSpy{}})

	m.Base.SelectItem(srcWorkers, folderID)
	press(t, m, " ")
	press(t, m, " ")
	press(t, m, " ")

	if _, handled := m.handleActionKey(keyBulkSetModel); !handled || m.modelPicker == nil {
		t.Fatal("shift+m must open the picker for the two real workers")
	}
	if len(m.modelPickerWorkers) != 2 {
		t.Fatalf("the folder must be excluded, leaving 2 workers: %v", m.modelPickerWorkers)
	}
	for _, id := range m.modelPickerWorkers {
		if screenkit.IsGroupRow(id) {
			t.Fatalf("the synthetic folder id reached the batch: %v", m.modelPickerWorkers)
		}
	}
}

// TestBulkSetModelIsWorkersOnly: the RPC is BulkUpdateWorkerModel, and a workflow's model is not a worker
// model at all — a workflow carries a ref per VERSION, edited through its own form. So the chord is scoped
// to the Workers pane exactly like every other worker chord here, and the key FALLS THROUGH elsewhere
// rather than opening a worker picker over a workflow.
func TestBulkSetModelIsWorkersOnly(t *testing.T) {
	for _, src := range []string{srcWorkflows, srcExecutions, srcRuns} {
		m, _, _ := crudExec(t, nil, nil)
		if !m.Base.SelectSource(src) {
			t.Fatalf("fixture: could not focus %s", src)
		}
		m.Base.LoadItems(src, []screenkit.Item{{ID: "x1", Title: "one"}, {ID: "x2", Title: "two"}}, "")
		m.Base.SelectItem(src, "x1")
		press(t, m, " ")
		press(t, m, " ")

		if _, handled := m.handleActionKey(keyBulkSetModel); handled {
			t.Fatalf("%s must not claim the worker set-model chord", src)
		}
		if m.modelPicker != nil {
			t.Fatalf("%s must not open a WORKER model picker", src)
		}
	}
}

// --- the composer's shortcut text ---------------------------------------------------------------

// TestHintLineNamesTheDeleteChordOnBothPanes: "ensure the shortcut text in the composer is there". The
// Workers hint used to advertise `x: delete` (a chord that no longer existed anywhere) and the Workflows
// hint advertised NO delete at all, which is how `shift+x` stayed undiscoverable.
func TestHintLineNamesTheDeleteChordOnBothPanes(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	for _, src := range []string{srcWorkers, srcWorkflows} {
		if !m.Base.SelectSource(src) {
			t.Fatalf("fixture: could not focus %s", src)
		}
		hint := m.HintLine()
		if !strings.Contains(hint, "ctrl+x") {
			t.Fatalf("%s: the composer must name the delete chord: %q", src, hint)
		}
		if strings.Contains(hint, "x: delete") && !strings.Contains(hint, "ctrl+x: delete") {
			t.Fatalf("%s: the hint still advertises the old chord: %q", src, hint)
		}
	}
}

// TestHintLineStatesTheSelectionAndItsChords: with rows marked the composer must say HOW MANY and which
// chords act on them — a bulk act the composer does not mention is a bulk act the operator has to guess.
func TestHintLineStatesTheSelectionAndItsChords(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{
		{ID: "w1", Title: "one"}, {ID: "w2", Title: "two"}, {ID: "w3", Title: "three"},
	}, "")
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")

	hint := m.HintLine()
	for _, want := range []string{"2 marked", "ctrl+x", "delete 2", keyBulkSetModel, "esc"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("the selection hint must contain %q: %q", want, hint)
		}
	}
}

// TestHintLineNamesTheBulkSetModelChord: the new act has to be advertised where the operator reads, or it
// is a chord nobody can find (the same failure the delete chord had).
func TestHintLineNamesTheBulkSetModelChord(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	if !strings.Contains(m.HintLine(), "mark") {
		t.Fatalf("the Workers hint must say how to build a selection: %q", m.HintLine())
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{{ID: "w1"}, {ID: "w2"}}, "")
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")
	if !strings.Contains(m.HintLine(), keyBulkSetModel+": set model") {
		t.Fatalf("the selection hint must name the set-model chord: %q", m.HintLine())
	}
}

// TestBulkSetModelActionCarriesTheMutationLabel: the action bar's label is what a CLICK acts on, so it must
// exist with the count (and refuse honestly, since a modal cannot be opened from the bar's Do).
func TestBulkSetModelActionCarriesTheMutationLabel(t *testing.T) {
	m, _, _ := crudExec(t, nil, nil)
	if !m.Base.SelectSource(srcWorkers) {
		t.Fatal("fixture: could not focus the Workers pane")
	}
	m.Base.LoadItems(srcWorkers, []screenkit.Item{{ID: "w1"}, {ID: "w2"}, {ID: "w3"}}, "")
	m.Base.SelectItem(srcWorkers, "w1")
	press(t, m, " ")
	press(t, m, " ")

	var act kit2.Action
	for _, a := range m.actionsForSelection() {
		if strings.HasPrefix(a.Label, "set model") {
			act = a
		}
	}
	if act.Key != keyBulkSetModel {
		t.Fatalf("the bulk set-model action must answer %q, got %q", keyBulkSetModel, act.Key)
	}
	if !strings.Contains(act.Label, "2") {
		t.Fatalf("the label must state the count: %q", act.Label)
	}
	// There is no confirm — the MODAL is the interaction, and its own choice is the commit — so the Do
	// must refuse with directions rather than quietly succeed or write something unintended.
	if act.NeedsConfirm() {
		t.Fatal("the set-model action must not add a confirm in front of the modal")
	}
	if err := act.Do(context.Background()); err == nil {
		t.Fatal("clicking the bar label must refuse with directions, not write")
	}
}

// categoryDeleteChordForTest exposes the folder-row delete chord, which is declared next to the rename
// chord and has no other reason to be exported.
func categoryDeleteChordForTest() string { return keyDeleteCategory }

var _ = tea.KeyCtrlX
