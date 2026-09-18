package tui

// composer_select_all_test.go — ctrl+a SELECTS THE WHOLE COMPOSER.
//
// The operator: "It would be nice to allow a ctrl+a to select all text in the composer so a user can easily
// copy or delete it all if possible."
//
// "IF POSSIBLE" IS THE HONEST PART. bubbles' textarea has NO selection model — v1.0.0 has no Select, no
// SelectAll, no selection state of any kind — so there is nothing to highlight and nothing for a later ctrl+c
// to read. What the operator wants is the two OUTCOMES, so the implementation delivers those: the SHELL copies
// the buffer on the way in (that half is in the route) and the composer enters a state where the next
// replacing key throws it away. These pin the composer's half.
//
// The state must also be IMPOSSIBLE TO GET STUCK IN: any key that is not a replacing one drops the selection
// and then behaves exactly as it did before, because a mode that swallows arrows and ctrl chords would be far
// worse than the missing feature it was added for.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/dock"
)

// dockWith builds a dock holding text, focused, as the composer is while typing.
func dockWith(t *testing.T, text string) *dock.Model {
	t.Helper()
	m := newTestApp()
	m.dock.SetValue(text)
	m.dock.Focus()
	return &m.dock
}

// pressDock runs one key through the dock and returns the command it produced.
func pressDock(m *dock.Model, k tea.KeyMsg) tea.Cmd {
	_, cmd := m.Update(k)
	return cmd
}

func runeK(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// SELECTING MARKS THE WHOLE BUFFER and says how much, so the operator can tell the chord did something.
func TestSelectAllMarksTheWholeComposer(t *testing.T) {
	m := dockWith(t, "a draft I might want to copy")

	m.SelectAll()
	if !m.SelectAllActive() {
		t.Fatal("SelectAll did not mark the composer as selected")
	}
	if !strings.Contains(m.Notice, "selected") {
		t.Errorf("the notice does not say what happened: %q", m.Notice)
	}
	// The text itself is untouched — selecting is not editing.
	if m.Value() != "a draft I might want to copy" {
		t.Errorf("SelectAll changed the text: %q", m.Value())
	}
}

// AN EMPTY COMPOSER SELECTS NOTHING, and does not enter the state: an "empty selection" would swallow the
// operator's next key for no benefit.
func TestSelectAllOnAnEmptyComposerDoesNothing(t *testing.T) {
	m := dockWith(t, "")
	m.SelectAll()
	if m.SelectAllActive() {
		t.Error("an empty composer entered the selected state")
	}
}

// TYPING REPLACES THE SELECTION — the "delete it all and start again" case.
func TestTypingReplacesTheSelection(t *testing.T) {
	m := dockWith(t, "the old draft")
	m.SelectAll()

	pressDock(m, runeK("n"))

	if got := m.Value(); got != "n" {
		t.Errorf("typing on a selection left %q, want the rune alone — the selection is what it overwrites", got)
	}
	if m.SelectAllActive() {
		t.Error("the selection survived the key that replaced it")
	}
}

// BACKSPACE CLEARS THE WHOLE BUFFER — the "delete it all" case.
func TestBackspaceClearsTheSelection(t *testing.T) {
	m := dockWith(t, "the old draft")
	m.SelectAll()

	pressDock(m, tea.KeyMsg{Type: tea.KeyBackspace})

	if got := m.Value(); got != "" {
		t.Errorf("backspace on a selection left %q, want the composer cleared", got)
	}
	if m.SelectAllActive() {
		t.Error("the selection survived backspace")
	}
}

// ESC CANCELS WITHOUT TOUCHING THE TEXT. A cancel that discarded the draft would be a data-loss trap.
func TestEscapeCancelsTheSelectionAndKeepsTheText(t *testing.T) {
	m := dockWith(t, "the old draft")
	m.SelectAll()

	pressDock(m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.SelectAllActive() {
		t.Error("esc did not clear the selection")
	}
	if got := m.Value(); got != "the old draft" {
		t.Errorf("esc changed the text: %q, want it untouched", got)
	}
}

// A NON-REPLACING KEY DROPS THE SELECTION AND STILL WORKS. This is the property that keeps the state from
// trapping the operator: the arrows, tab and every ctrl chord must behave exactly as they did before ctrl+a
// was pressed.
func TestANonReplacingKeyDropsTheSelectionAndStillActs(t *testing.T) {
	m := dockWith(t, "the old draft")
	m.SelectAll()

	// Left is movement: it must move the caret and NOT clear the buffer.
	pressDock(m, tea.KeyMsg{Type: tea.KeyLeft})

	if m.SelectAllActive() {
		t.Error("a movement key did not drop the selection")
	}
	if got := m.Value(); got != "the old draft" {
		t.Errorf("a movement key changed the text: %q, want it untouched", got)
	}
}

// THE DRAFT SURVIVES UNLESS A KEY ACTS ON IT. Stated as its own property because it is the risk of the whole
// feature: entering a state must not be able to cost the operator their text by itself.
func TestSelectingAloneNeverLosesTheDraft(t *testing.T) {
	m := dockWith(t, "precious words")
	m.SelectAll()
	m.ClearSelectAll()
	if got := m.Value(); got != "precious words" {
		t.Errorf("selecting and deselecting lost the draft: %q", got)
	}
}

// INSERTTEXT APPENDS AT THE CURSOR — the paste path (ctrl+v with text on the clipboard).
func TestInsertTextAppendsToTheComposer(t *testing.T) {
	m := dockWith(t, "hello ")
	m.InsertText("world")
	if got := m.Value(); got != "hello world" {
		t.Errorf("InsertText produced %q, want the text appended", got)
	}
	// And it does not enter the selected state, which would swallow the operator's next keystroke.
	if m.SelectAllActive() {
		t.Error("InsertText left the composer in the selected state")
	}
}

// THE CHORD IS REACHABLE. ctrl+a is bound by the textarea to "line start", so the shell has to keep it out of
// the textarea's hands — otherwise the route never sees the key and the feature is bound but dead, which is
// the failure mode this codebase keeps producing.
func TestCtrlAIsRegisteredAndBypassesTheComposer(t *testing.T) {
	m := askRelaunched(t, "c1")

	var registered bool
	for _, r := range m.routes {
		if r.Keys == "ctrl+a" {
			registered = true
		}
	}
	if !registered {
		t.Error("ctrl+a is not registered as a route, so the help overlay never lists it")
	}
	if !composerBypassKeys["ctrl+a"] {
		t.Error("ctrl+a is not in composerBypassKeys — the textarea would consume it as \"line start\" " +
			"before the route ran")
	}
}

// AND PRESSING IT THROUGH THE SHELL SELECTS AND COPIES. Driven through the real dispatch, because that is the
// layer at which "the chord works while the composer is focused" is true or false.
func TestCtrlAThroughTheShellSelectsTheComposer(t *testing.T) {
	m := askRelaunched(t, "c1")
	m.chatFocus = focusComposer
	m.dock.Focus()
	m.dock.SetValue("a draft to keep")

	nm, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = nm

	if !m.dock.SelectAllActive() {
		t.Fatalf("ctrl+a through the shell did not select the composer. notice=%q", m.dock.Notice)
	}
	if !strings.Contains(m.dock.Notice, "selected") {
		t.Errorf("the shell did not report the selection: %q", m.dock.Notice)
	}
	// THE COPY LEAVES THROUGH dispatch's RETURN VALUE, not the staging field: the route stages it (a route
	// returns only a bool) and drainStaged re-emits it on the way out, clearing the field. Asserting the
	// FIELD here was wrong — it is always nil by the time the caller sees the App.
	if cmd == nil {
		t.Error("ctrl+a selected the text but produced no command, so the clipboard write never happens — " +
			"the operator's \"easily copy ... it all\" half would be missing")
	}
}
