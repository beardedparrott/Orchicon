package work

// delete_chord_test.go — ONE DELETE GESTURE, TWO KEYS.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// BOTH KEYS OPEN THE DELETE CONFIRM, on every Work pane.
func TestBothDeleteKeysWorkOnEveryWorkPane(t *testing.T) {
	for _, src := range []string{srcProjects, srcWorkItems, srcImages} {
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyCtrlX},
			{Type: tea.KeyRunes, Runes: []rune("x")},
		} {
			m := deleteFixture(t, src)
			m.Update(key)
			if !m.DialogOpen() {
				t.Errorf("%s + %q: no confirm dialog opened. ctrl+x is the client's delete chord and what the "+
					"operator asked for; `x` is what these panes have always used. Both must reach the action.",
					src, key.String())
			}
		}
	}
}

// AND THE OTHER ACTION CHORDS ARE UNTOUCHED, so splitting the delete case out of the shared
// `x`/`y`/`a`/`R` arm did not quietly unhook the rest of them.
func TestTheOtherActionChordsStillWork(t *testing.T) {
	p := newPlane()
	pr := p.seedProject("proj-1", "Thing")
	pr.Status = apiv1.ProjectStatus_PROJECT_STATUS_DRAFTING
	m := newModel(t, p)
	m.SelectSource(srcProjects)
	load(t, m, srcProjects)

	// `a` is bound to activate on a drafting project: pressing it must dispatch something.
	if _, ok := m.actionByKey("a"); !ok {
		t.Fatal("fixture: a drafting project does not bind `a`, so there is nothing to assert")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}); cmd == nil && !m.DialogOpen() {
		t.Error("`a` is bound to activate but pressing it dispatched nothing — the delete chord was split out of " +
			"the shared arm and the other keys must have come with it")
	}

	// The pane's remaining letter chords are still routed, asked for rather than assumed.
	for _, k := range []string{"y", "R"} {
		m := deleteFixture(t, srcWorkItems)
		if _, ok := m.actionByKey(k); !ok {
			continue // this pane does not bind it; nothing to assert
		}
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if cmd == nil && !m.DialogOpen() {
			t.Errorf("the pane binds %q but pressing it dispatched nothing", k)
		}
	}
}

// THE TWO CHORDS ARE DECLARED ONCE, and the Work panes read the canonical one.
func TestTheDeleteChordIsShared(t *testing.T) {
	if kit2.DeleteChord == kit2.DeleteChordAlias {
		t.Fatalf("the delete chord and its alias are both %q — one of them is wrong", kit2.DeleteChord)
	}
	if kit2.DeleteChord != "ctrl+x" {
		t.Errorf("the canonical delete chord is %q, want ctrl+x — every other pane in the client answers it, "+
			"and it is the one the operator asked for by name", kit2.DeleteChord)
	}
	// The Work panes carry the canonical chord, so the bar advertises the same binding every
	// other pane does.
	m := deleteFixture(t, srcProjects)
	for _, a := range m.actionsForSelection() {
		if a.Label == "delete" && a.Key != kit2.DeleteChord {
			t.Errorf("a Work pane's delete action answers %q, want the shared %q", a.Key, kit2.DeleteChord)
		}
	}
}

// deleteFixture builds a model on one source with a row to act on.
func deleteFixture(t *testing.T, src string) *Model {
	t.Helper()
	p := newPlane()
	p.seedProject("proj-1", "Orchicon")
	p.addItem(&apiv1.WorkItem{Id: "wi-1", Title: "Item", Kind: apiv1.WorkItemKind_WORK_ITEM_KIND_TASK,
		ProjectId: "proj-1", Status: apiv1.WorkItemStatus_WORK_ITEM_STATUS_PENDING})
	p.seedImage("img-1", "runtime", apiv1.RuntimeImageStatus_RUNTIME_IMAGE_STATUS_READY)
	m := newModel(t, p)
	m.SelectSource(src)
	load(t, m, src)
	return m
}
