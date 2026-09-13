package kit2

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func pickerForm() *Form {
	f := NewForm("New work item",
		FieldSpec{Name: "title", Label: "Title", Kind: KText},
		FieldSpec{Name: "parent", Label: "Parent", Kind: KPicker, Options: []Option{
			{Value: "wi-1", Label: "[epic] Sweeper"},
			{Value: "wi-3", Label: "[task] Sweeper metrics"},
			{Value: "wi-2", Label: "[task] Telemetry overhaul"},
		}},
		FieldSpec{Name: "priority", Label: "Priority", Kind: KNumber},
	)
	f.Width = 60
	f.Focused = true
	f.FocusName("parent")
	return f
}

// The operator: "The Parent work item requires an ID. There is no way a human is
// going to know this ... if you type a letter another box comes up similar to
// the slash command that has all work items that can be selected."
//
// So typing must OPEN a filtered list, and the committed value must be the
// option's ID — chosen from the list, never typed.
func TestPickerTypesToFilterAndCommitsTheID(t *testing.T) {
	f := pickerForm()

	// Nothing is listed until the operator starts typing.
	if v := ansi.Strip(f.View()); strings.Contains(v, "enter select") {
		t.Fatalf("the list must stay closed until asked for:\n%s", v)
	}

	for _, r := range "sweeper" {
		pressKey(f, keyRune(string(r)))
	}
	v := ansi.Strip(f.View())
	if !strings.Contains(v, "Sweeper metrics") || !strings.Contains(v, "[epic] Sweeper") {
		t.Fatalf("typing must list the matching options:\n%s", v)
	}
	if strings.Contains(v, "Telemetry") {
		t.Fatalf("a non-matching option must be filtered out:\n%s", v)
	}
	// The field shows the QUERY being typed, not the old value.
	if !strings.Contains(v, "Parent: sweeper") {
		t.Fatalf("the field must show the query:\n%s", v)
	}

	// Down moves the highlight, enter commits THAT option's ID.
	pressKey(f, tea.KeyMsg{Type: tea.KeyDown})
	pressKey(f, tea.KeyMsg{Type: tea.KeyEnter})
	if got := f.Values["parent"]; got != "wi-3" {
		t.Fatalf("committed value = %q, want the option's id wi-3", got)
	}
	// Closed, and the field now reads as the chosen LABEL.
	v = ansi.Strip(f.View())
	if strings.Contains(v, "enter select") {
		t.Fatalf("choosing must close the list:\n%s", v)
	}
	if !strings.Contains(v, "Parent: [task] Sweeper metrics") {
		t.Fatalf("the field must show the chosen label:\n%s", v)
	}
}

// up/down must still walk FIELDS while the list is closed — otherwise a picker
// would be a keyboard trap.
func TestPickerClosedArrowsWalkFields(t *testing.T) {
	f := pickerForm()
	if f.CurrentName() != "parent" {
		t.Fatalf("focused %q", f.CurrentName())
	}
	pressKey(f, tea.KeyMsg{Type: tea.KeyDown})
	if f.CurrentName() != "priority" {
		t.Fatalf("down with the list closed must move to the next field, got %q", f.CurrentName())
	}
	pressKey(f, tea.KeyMsg{Type: tea.KeyUp})
	if f.CurrentName() != "parent" {
		t.Fatalf("up must come back to the picker, got %q", f.CurrentName())
	}
}

// esc closes the list without touching the form (so esc esc still cancels).
func TestPickerEscClosesTheListFirst(t *testing.T) {
	f := pickerForm()
	pressKey(f, keyRune("a"))
	if f.pickerField == "" {
		t.Fatal("typing must open the list")
	}
	_, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !handled {
		t.Fatal("esc with the list open must be consumed (it closes the list)")
	}
	if f.pickerField != "" {
		t.Fatal("esc must close the list")
	}
	// With the list closed esc falls through so the CALLER closes the form.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); handled {
		t.Fatal("esc with the list closed must fall through to the caller")
	}
}

// A picker offers "none" as a real choice, so an optional reference can be
// cleared rather than only set.
func TestPickerCanChooseNone(t *testing.T) {
	f := NewForm("New",
		FieldSpec{Name: "workflow", Label: "Workflow", Kind: KPicker, Options: []Option{
			{Value: "", Label: "— none —"},
			{Value: "wf-1", Label: "Ship it"},
		}},
	)
	f.Width = 60
	f.Focused = true
	f.FocusName("workflow")
	f.Set("workflow", "wf-1")
	// Typing "n" matches "— none —"; enter commits the empty value.
	pressKey(f, keyRune("none"))
	pressKey(f, tea.KeyMsg{Type: tea.KeyEnter})
	if got := f.Values["workflow"]; got != "" {
		t.Fatalf("choosing none must clear the reference, got %q", got)
	}
}

// The operator wants a picker value to be CUSTOMIZABLE, not only selectable:
// typing a value that matches no option and pressing enter commits the typed
// text as the value (the field's own validator then judges it).
func TestPickerAcceptsACustomValue(t *testing.T) {
	f := NewForm("Schedule",
		FieldSpec{Name: "at", Label: "Start", Kind: KPicker, Options: []Option{
			{Value: "2026-09-13T09:00:00Z", Label: "tomorrow 09:00 — 2026-09-13T09:00:00Z"},
		}, Validate: func(s string) error {
			if s == "" {
				return nil
			}
			if !strings.Contains(s, "T") || !strings.HasSuffix(s, "Z") {
				return errCustomNotATime
			}
			return nil
		}},
	)
	f.Width = 70
	f.Focused = true
	f.FocusName("at")

	// A typed value matching no preset is committed on enter.
	for _, r := range "2026-12-25T08:30:00Z" {
		pressKey(f, keyRune(string(r)))
	}
	if f.pickerField == "" {
		t.Fatal("typing must open the picker")
	}
	pressKey(f, tea.KeyMsg{Type: tea.KeyEnter})
	if got := f.Values["at"]; got != "2026-12-25T08:30:00Z" {
		t.Fatalf("a custom value must be committed, got %q", got)
	}
	if f.pickerField != "" {
		t.Fatal("committing must close the picker")
	}
	if !f.Validate() {
		t.Fatalf("a well-formed custom value must validate: %v", f.Errors)
	}

	// A malformed custom value is committed but FAILS validation, so the form
	// reports it instead of sending it.
	f2 := NewForm("Schedule", f.Specs...)
	f2.Width = 70
	f2.Focused = true
	f2.FocusName("at")
	for _, r := range "whenever" {
		pressKey(f2, keyRune(string(r)))
	}
	pressKey(f2, tea.KeyMsg{Type: tea.KeyEnter})
	if got := f2.Values["at"]; got != "whenever" {
		t.Fatalf("the typed value must be committed, got %q", got)
	}
	if f2.Validate() || f2.Errors["at"] == "" {
		t.Fatal("a malformed custom value must fail validation with a message")
	}
}

var errCustomNotATime = errNotATime{}

type errNotATime struct{}

func (errNotATime) Error() string { return "not a timestamp" }
