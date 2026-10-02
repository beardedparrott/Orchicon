package kit2

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCtrlUClearsEveryReferenceField pins the CLASS, not one instance.
//
// ctrl+u cleared only `editable` kinds, and `editable` is false for every REFERENCE kind (KModel,
// KDate, KDateTime) — the fields that hold a CHOSEN value. The operator's report was about the
// scheduled-start field ("There is no way to clear a schedule on a work item"), but the same silent
// no-op applied to a model ref and a plain date, and it is the class that must be fixed: a reference
// value is exactly the kind an operator wants to empty, and the footer advertised the key the whole
// time.
func TestCtrlUClearsEveryReferenceField(t *testing.T) {
	cases := []struct {
		kind Kind
		val  string
	}{
		{KModel, "orchicon/commandcode/deepseek/deepseek-v4-flash"},
		{KDate, "2026-01-01"},
		{KDateTime, "2026-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		f := NewForm("Reference clears",
			FieldSpec{Name: "f", Label: "F", Kind: c.kind, Initial: c.val},
		)
		if !f.FocusName("f") {
			t.Fatalf("%v: could not focus the field", c.kind)
		}
		_, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
		if !handled {
			t.Errorf("%v: ctrl+u must be consumed", c.kind)
		}
		if got := f.Values["f"]; got != "" {
			t.Errorf("%v: ctrl+u left %q — the clear key is a no-op on a reference field", c.kind, got)
		}
	}
}

// TestCtrlUClearFiresOnChange — a programmatic change must notify the host, so a form that couples
// fields (the work-item form's schedule/auto-start) sees the clear. The other value-setting paths
// already call OnChange; the clear is a value change like any other.
func TestCtrlUClearFiresOnChange(t *testing.T) {
	f := NewForm("Clear notifies", FieldSpec{Name: "d", Label: "D", Kind: KDateTime, Initial: "2026-01-01T00:00:00Z"})
	var seen []string
	f.OnChange = func(name, value string) { seen = append(seen, name+"="+value) }
	if !f.FocusName("d") {
		t.Fatal("could not focus")
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	if len(seen) != 1 || seen[0] != "d=" {
		t.Fatalf("ctrl+u must fire OnChange with the empty value, got %v", seen)
	}
}
