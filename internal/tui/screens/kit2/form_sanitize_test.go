package kit2

// form_sanitize_test.go — a form field can never hold a control character.
//
// REPORTED (Windows TUI, creating a project):
//
//	internal: db: create project: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//
// A NUL reached a TEXT column. It is INVISIBLE, which is what made it so expensive: the field read "Temp" on
// screen and the server refused it for a byte nobody could see. NULs arrive from a terminal far more readily
// than from a keyboard — a Windows console produces 0x00 for several chords, and a paste can carry one — so
// the guard is on the FORM rather than on any one key handler: whatever the source, the value can never hold
// one.
//
// The API refuses them too (internal/project validateName), because that boundary protects clients that are
// not this TUI. Two guards for one bug is deliberate: the form keeps it out of a value the operator may save
// later, and the API keeps it out of the database.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sanitizeForm(kind Kind) *Form {
	f := NewForm("t", FieldSpec{Name: "v", Label: "V", Kind: kind})
	f.Focused = true
	f.Width = 60
	return f
}

func TestAFieldNeverHoldsAControlCharacter(t *testing.T) {
	// INSERTED BY KEY, which is how a chord that yields 0x00 would arrive.
	for _, r := range []rune{0x00, 0x01, 0x07, 0x1b, 0x7f} {
		f := sanitizeForm(KText)
		f.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', r, 'b'}})
		if strings.ContainsFunc(f.Values["v"], func(x rune) bool { return x < 0x20 || x == 0x7f }) {
			t.Errorf("a typed %#U was stored: %q", r, f.Values["v"])
		}
		if f.Values["v"] != "ab" {
			t.Errorf("the surrounding text was not preserved: %q, want %q", f.Values["v"], "ab")
		}
	}
}

func TestPastedTextIsSanitized(t *testing.T) {
	// PASTED, which is the other way one arrives — and the one that also carries NULs from other tools.
	f := sanitizeForm(KText)
	f.PasteText("Temp\x00")
	if strings.ContainsRune(f.Values["v"], 0) {
		t.Errorf("a pasted NUL was stored: %q", f.Values["v"])
	}
	if f.Values["v"] != "Temp" {
		t.Errorf("paste lost the visible text: %q, want \"Temp\"", f.Values["v"])
	}
}

func TestATextAreaKeepsItsNewlinesAndTabs(t *testing.T) {
	// THE INVARIANT IS NOT "NO CONTROL CHARACTERS", it is "no invisible ones". A multi-line field's newlines
	// AND tabs are its content, and stripping them would break the paste it exists for.
	f := sanitizeForm(KTextArea)
	f.PasteText("one\ntwo\tthree")
	if got := f.Values["v"]; got != "one\ntwo\tthree" {
		t.Errorf("textarea paste = %q, want the newline and tab preserved", got)
	}
	// …but a NUL in the same paste is still dropped.
	f2 := sanitizeForm(KTextArea)
	f2.PasteText("one\x00two")
	if strings.ContainsRune(f2.Values["v"], 0) {
		t.Errorf("a NUL survived in a textarea: %q", f2.Values["v"])
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	// VACUITY: sanitizeInput must be a no-op for everything normal, or it would quietly edit values.
	for _, s := range []string{"Temp", "C:\\Users\\justi\\AppData\\Local\\Temp", "proj-1", "Ünïcode", ""} {
		if got := sanitizeInput(s); got != s {
			t.Errorf("sanitizeInput(%q) = %q, want unchanged", s, got)
		}
	}
}
