package work

// bulk_set_hint_test.go — THE BULK SET IS DISCOVERABLE FROM THE COMPOSER.
//
// The operator: "We recently added bulk workflow and runtime image setting but the workers didn't add a
// shortcut helper in the composer to tell people that they can and how to do it. We need one for W."
//
// The chord existed and the action bar offered it — but ONLY once a selection existed, which made it
// discoverable only to an operator who had already found it. And below the bulk threshold the key did
// nothing at all: keyBulkSet is the one chord in its key group that is not always in the action list, so
// actionByKey found nothing and the key fell through in SILENCE. A hint that names a chord which then does
// nothing is worse than no hint, so both halves are pinned here.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// plain strips the hint's styling, because the composer's context line is stripped too (the shell does it
// with ansi.Strip) — so this asserts the words the operator actually reads.
func plain(s string) string { return strings.TrimSpace(theme.HintText.Render(s)) }

// THE COMPOSER NAMES THE CHORD, WHAT IT DOES, AND HOW TO GET THERE — with no selection, which is the state
// the operator is in when they go looking for it.
func TestTheWorkItemsHintNamesTheBulkSet(t *testing.T) {
	m := bulkModel(t)
	m.Base.ClearMarks()

	h := plain(m.HintLine())
	if !strings.Contains(h, keyBulkSet+": set workflow & image") {
		t.Fatalf("the Work Items hint does not name the bulk set chord %q: %q", keyBulkSet, h)
	}
	// HOW to get there, not only that it exists: the chord needs marks, and the hint must say so.
	if !strings.Contains(h, "mark") {
		t.Fatalf("the hint names the chord but not how to reach it (marking): %q", h)
	}
}

// AND IT SWITCHES TO THE SELECTION'S OWN LINE, where the count and the chord come first — the shape the
// Workers pane's bulk set-model already uses.
func TestTheWorkItemsHintStatesTheSelection(t *testing.T) {
	m := bulkModel(t)
	mark(t, m, 2)

	h := plain(m.HintLine())
	if !strings.Contains(h, "2 marked") {
		t.Fatalf("the selection hint must state the count for %d marks: %q", m.Base.MarkCount(), h)
	}
	if !strings.Contains(h, keyBulkSet+": set workflow & image") {
		t.Fatalf("the selection hint must name the bulk set chord: %q", h)
	}
	if !strings.Contains(h, "esc: clear") {
		t.Fatalf("the selection hint must say how to drop the selection: %q", h)
	}
}

// THE CHORD IS NOT A DEAD KEY BELOW THE THRESHOLD. It cannot open the picker — there is no selection to
// apply it to — but it must say so rather than doing nothing, which is what made it undiscoverable in the
// first place.
func TestTheBulkSetRefusesOutLoudWithoutASelection(t *testing.T) {
	for _, marks := range []int{0, 1} {
		m := bulkModel(t)
		m.Base.ClearMarks()
		mark(t, m, marks)
		m.notice = ""

		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(keyBulkSet)})
		if m.notice == "" {
			t.Fatalf("with %d mark(s) the bulk set chord did nothing and said nothing — a dead key", marks)
		}
		if !strings.Contains(m.notice, keyBulkSet) {
			t.Fatalf("the refusal does not name the chord it is refusing: %q", m.notice)
		}
		if !strings.Contains(m.notice, "space") {
			t.Fatalf("the refusal must say how to build the selection it needs: %q", m.notice)
		}
		if m.bulkSetIDs != nil {
			t.Fatalf("with %d mark(s) the picker was armed with %v — it must not open", marks, m.bulkSetIDs)
		}
	}
}

// AND IT STILL OPENS THE PICKER WITH A REAL SELECTION — the refusal must not shadow the feature.
func TestTheBulkSetStillOpensWithASelection(t *testing.T) {
	m := bulkModel(t)
	m.Base.ClearMarks()
	mark(t, m, 2)

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(keyBulkSet)})
	if len(m.bulkSetIDs) < 2 {
		t.Fatalf("the bulk set chord armed %v — want the 2 marked ids, so the picker opens", m.bulkSetIDs)
	}
}
