package kit2

// form_paste_test.go — PASTING INTO A FIELD.
//
// The operator: "The modal does not allow me to paste anything in any of the fields from my clipboard.
// ctrl+v should allow me to paste a secret name, token, etc."
//
// The shell's half of that (reading the clipboard and routing the text to a form) is pinned in the tui
// package; what is pinned HERE is the field's half, which is where the interesting rule lives: a paste is
// not typing, and a SINGLE-LINE field must not be able to receive a newline from it. A token copied out of a
// config file, a browser or `cat` very often carries a trailing newline, and the operator can see only one
// line of the field it would land in.

import (
	"strings"
	"testing"
)

func TestPasteTextInsertsIntoTheFocusedField(t *testing.T) {
	f := NewForm("Store MCP credential",
		FieldSpec{Name: "key", Label: "Credential key", Kind: KText},
		FieldSpec{Name: "value", Label: "Value", Kind: KSecret},
	)
	if f.current() == nil || f.current().Name != "key" {
		t.Fatalf("the form did not open focused on its first field")
	}
	if !f.PasteText("MCP_GITHUB_TOKEN") {
		t.Fatal("PasteText reported nothing inserted into an editable field")
	}
	if got := f.Values["key"]; got != "MCP_GITHUB_TOKEN" {
		t.Errorf("the pasted value = %q, want the whole token", got)
	}
}

func TestPasteTextCollapsesNewlinesInASingleLineField(t *testing.T) {
	// THE REASON THIS IS NOT JUST insertRunes: a pasted token usually ends in a newline, and a KText field
	// that accepted it would hold a line break the operator cannot see — which then fails validation or, far
	// worse, is STORED with a newline inside a credential name.
	f := NewForm("Key", FieldSpec{Name: "key", Label: "Key", Kind: KText, Initial: "PREFIX_"})
	f.PasteText("MCP_TOKEN\r\n")
	got := f.Values["key"]
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("a single-line field received whitespace it cannot show: %q", got)
	}
	if got != "PREFIX_MCP_TOKEN" {
		t.Errorf("value = %q, want the pasted text spliced in at the caret and trimmed", got)
	}
}

func TestPasteTextKeepsNewlinesInAMultiLineField(t *testing.T) {
	// The other half of the same rule: in a textarea the line breaks ARE the content (a skill list, a
	// permissions blob), so they survive.
	f := NewForm("Env", FieldSpec{Name: "env", Label: "Env", Kind: KTextArea})
	f.PasteText("A=1\nB=2\n")
	if got := f.Values["env"]; got != "A=1\nB=2\n" {
		t.Errorf("a multi-line field lost its line breaks: %q", got)
	}
}

func TestPasteTextRefusesAFieldThatCannotHoldText(t *testing.T) {
	// A checkbox has nothing to paste into, and reporting false is what lets the shell tell "pasted into the
	// field" from "there was no field for it" — so it never silently swallows the text.
	f := NewForm("Enabled", FieldSpec{Name: "enabled", Label: "Enabled", Kind: KCheckbox})
	if f.PasteText("true") {
		t.Error("PasteText claimed to paste into a checkbox")
	}
}

func TestPasteTextIgnoresEmptyText(t *testing.T) {
	// A clipboard holding only whitespace must not report a successful paste: the shell shows a notice on
	// success, and "pasted 0 characters" reads as a dead key.
	f := NewForm("Key", FieldSpec{Name: "key", Label: "Key", Kind: KText})
	if f.PasteText("  \n\t ") {
		t.Error("PasteText reported a paste for whitespace-only text")
	}
	if got := f.Values["key"]; got != "" {
		t.Errorf("the field was changed by a whitespace paste: %q", got)
	}
}

func TestPasteTextReplacesAPickersValue(t *testing.T) {
	// THE FIELD THAT MATTERS MOST IN PRACTICE: the credential form's first two fields are pickers (the env
	// var/header key, and the stored secret), so pasting a token name into a picker IS the report.
	//
	// A picker has no caret, so "insert" would be meaningless — and APPENDING to one that already holds a
	// selection yields a value matching no option. The paste is the whole choice, so it replaces.
	f := NewForm("Credential key",
		FieldSpec{Name: "key", Label: "Credential key", Kind: KPicker, Initial: "OLD_KEY",
			Options: []Option{{Value: "OLD_KEY", Label: "OLD_KEY"}, {Value: "NEW_KEY", Label: "NEW_KEY"}}},
	)
	if !f.PasteText("MCP_GITHUB_TOKEN\n") {
		t.Fatal("PasteText refused a picker — the credential form's key and secret fields are pickers")
	}
	if got := f.Values["key"]; got != "MCP_GITHUB_TOKEN" {
		t.Errorf("picker value = %q, want the pasted name replacing the old one (never appended)", got)
	}
}
