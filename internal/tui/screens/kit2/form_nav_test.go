package kit2

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The operator's report: while the New Work Item modal was open "the arrow
// keys still control the work item list versus moving through the New Work
// Item fields". A form must therefore own the two VERTICAL keys: Up/Down walk
// the fields exactly like Shift+Tab/Tab, so no keystroke an operator aims at
// the modal can reach a widget behind it.
func TestFormUpDownWalkFields(t *testing.T) {
	f := NewForm("New work item",
		FieldSpec{Name: "title", Label: "Title", Kind: KText, Required: true},
		FieldSpec{Name: "project", Label: "Project", Kind: KSelect, Options: []Option{{Value: "p1"}}},
		FieldSpec{Name: "parent", Label: "Parent", Kind: KText},
	)
	if f.CurrentName() != "title" {
		t.Fatalf("initial field = %q, want title", f.CurrentName())
	}

	// Down advances, matching Tab.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyDown}); !handled {
		t.Fatal("down must be consumed by the form")
	}
	if f.CurrentName() != "project" {
		t.Fatalf("down -> %q, want project", f.CurrentName())
	}
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyDown}); !handled {
		t.Fatal("down must be consumed by the form")
	}
	if f.CurrentName() != "parent" {
		t.Fatalf("down -> %q, want parent", f.CurrentName())
	}

	// Up retreats, matching Shift+Tab.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyUp}); !handled {
		t.Fatal("up must be consumed by the form")
	}
	if f.CurrentName() != "project" {
		t.Fatalf("up -> %q, want project", f.CurrentName())
	}

	// Down wraps forward, exactly like Tab (a modal is never a dead end).
	// From the middle field (project, index 1 of 3) wrapping forward reaches
	// the first field in TWO steps: project -> parent -> title. (Three steps
	// would land back on project, since Next() wraps modulo the field count.)
	for i := 0; i < 2; i++ {
		f.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	}
	if f.CurrentName() != "title" {
		t.Fatalf("two downs must wrap to title, got %q", f.CurrentName())
	}

	// Left/Right keep the horizontal value gesture (select cycling) — Down
	// must NOT cycle the select it lands on.
	f.FocusName("project")
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyDown}); !handled {
		t.Fatal("down must be consumed")
	}
	if f.CurrentName() != "parent" {
		t.Fatalf("down on a select must move on, not cycle it: %q", f.CurrentName())
	}
}
