package kit2

// base_edit_esc_test.go — Esc through the HOST, not just through the form.
//
// WHY THIS FILE EXISTS AS A SEPARATE THING FROM form_edit_lock_test.go. The form's
// Esc handling (release the edit lock, close the markdown preview) was written and
// tested at the FORM level, by calling f.HandleKey directly. The host never let it
// run: Base.Update intercepts "esc" while an inline editor is open and closes the
// editor outright, before consulting the form. So the operator's real experience was
// "Esc is not only escaping out of the field but also the edit form" — while every
// form-level test passed.
//
// A test that drives f.HandleKey proves what the FORM does with a key. It says
// nothing about what the OPERATOR gets, because the key may never reach the form.
// These tests drive b.Update, which is the path a keystroke actually takes.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// escHost builds a Base with one inline editor open, and returns the form.
func escHost(t *testing.T, value string, kind Kind) (*Base, *Form) {
	t.Helper()
	b := &Base{}
	b.HideSources = true
	b.SetSize(120, 40)
	f := NewForm("Edit something", FieldSpec{Name: "body", Label: "Body", Kind: kind, Initial: value})
	f.Focused = true
	f.FocusName("body")
	b.BeginDetailEdit("Edit something", f)
	return b, f
}

func esc() tea.Msg { return tea.KeyMsg{Type: tea.KeyEsc} }

// THE REPORT. Esc with a field LOCKED releases the field and leaves the editor OPEN.
func TestHostEscReleasesTheLockWithoutClosingTheEditor(t *testing.T) {
	var long strings.Builder
	for i := 0; i < 30; i++ {
		long.WriteString("line of content\n")
	}
	b, f := escHost(t, long.String(), KTextArea)

	// Enter locks the field.
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if f.EditingField() != "body" {
		t.Fatalf("fixture: Enter did not lock the field (editing = %q)", f.EditingField())
	}

	// Esc: the FIELD is released, the EDITOR stays open.
	b.Update(esc())

	if f.EditingField() != "" {
		t.Error("Esc did not release the edit lock")
	}
	if !b.EditingDetail() {
		t.Fatal("Esc closed the EDIT FORM as well as the field — this is the operator's report: " +
			"\"Esc is not only escaping out of the field but also the edit form\"")
	}
	if b.OnEditDone != nil {
		t.Error("fixture assumption broken: OnEditDone is set but the editor should not have closed")
	}
}

// And a SECOND Esc, with nothing left for the form to use it for, closes the editor —
// the original meaning of Esc must survive.
func TestHostSecondEscClosesTheEditor(t *testing.T) {
	b, f := escHost(t, "# Title\n\nsome **markdown**\n", KTextArea)

	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if f.EditingField() != "body" {
		t.Fatal("fixture: field not locked")
	}

	b.Update(esc()) // releases the lock
	if !b.EditingDetail() {
		t.Fatal("the first Esc should only release the field")
	}
	b.Update(esc()) // now cancels
	if b.EditingDetail() {
		t.Error("the second Esc did not close the editor — Esc can no longer cancel an edit")
	}
}

// An editor with NO lock and NO preview still cancels on the first Esc.
func TestHostEscStillCancelsAPlainEditor(t *testing.T) {
	b, _ := escHost(t, "just some text", KText)
	if !b.EditingDetail() {
		t.Fatal("fixture: editor not open")
	}
	b.Update(esc())
	if b.EditingDetail() {
		t.Error("Esc did not cancel a plain edit — the first-refusal change broke the common case")
	}
}

// Esc while PREVIEWING closes the preview and leaves the editor open. The
// preview's own test drives the form; this one proves the host lets it run.
func TestHostEscClosesThePreviewWithoutClosingTheEditor(t *testing.T) {
	b, f := escHost(t, "# Title\n\nsome **markdown**\n", KTextArea)

	b.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	if f.preview == "" {
		t.Fatal("fixture: ctrl+p did not open the preview")
	}

	b.Update(esc())

	if f.preview != "" {
		t.Error("Esc did not close the preview")
	}
	if !b.EditingDetail() {
		t.Fatal("Esc closed the EDIT FORM as well as the preview")
	}
	if f.Values["body"] != "# Title\n\nsome **markdown**\n" {
		t.Error("the value changed")
	}

	// The editor is still usable: type, and the character lands.
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}})
	if !strings.HasSuffix(f.Values["body"], "X") {
		t.Errorf("the editor is not usable after Esc closed the preview: %q", f.Values["body"])
	}
}

// CLOSING THE EDITOR REPORTS THE OUTCOME EXACTLY ONCE, so a screen's notice is not
// duplicated or lost by the new two-step Esc.
func TestHostEscReportsTheCloseOnce(t *testing.T) {
	b := &Base{}
	b.HideSources = true
	b.SetSize(120, 40)
	f := NewForm("Edit", FieldSpec{Name: "body", Label: "Body", Kind: KTextArea, Initial: "x\ny\n"})
	f.Focused = true
	f.FocusName("body")

	closes := 0
	b.SetOnEditDone(func(submitted bool) {
		if submitted {
			t.Error("an Esc cancel must not report submitted=true")
		}
		closes++
	})
	b.BeginDetailEdit("Edit", f)

	b.Update(tea.KeyMsg{Type: tea.KeyEnter}) // lock
	b.Update(esc())                          // release the field
	if closes != 0 {
		t.Fatalf("closes = %d after the first Esc, want 0 — the editor should still be open", closes)
	}
	b.Update(esc()) // cancel
	if closes != 1 {
		t.Fatalf("closes = %d after the second Esc, want exactly 1", closes)
	}
}
