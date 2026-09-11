package screenkit

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// FormField is one editable row in a Form.
type FormField struct {
	Key      string
	Label    string
	Value    string
	Hint     string
	Options  []string // non-empty = cycle-select (←/→ or space); empty = free text
	Required bool
	Validate func(value string) error
}

// Form is the small validated input modal a screen renders in place of its
// panes: text fields + cycle-selects, per-field validation on submit, and
// cancel/submit signals. Keys: ↑/↓ (or tab) move, ←/→ (or space) cycle a
// select, backspace edits, ctrl+u clears the focused field, ctrl+s (or
// enter) submits, esc cancels.
//
// The screen owns the Form for its whole lifetime: while one is open the
// screen answers ClaimsKeys()=true so the shell routes every key to the
// form (a typed 'q' must never quit, a typed space must never open the tab
// menu).
type Form struct {
	Title  string
	Fields []FormField
	Cursor int
	// Err names the last submit's validation failure ("" = valid). It
	// always identifies the offending field.
	Err string
}

// NewForm builds a form over the given fields (values pre-filled).
func NewForm(title string, fields []FormField) *Form {
	return &Form{Title: title, Fields: fields}
}

// Field returns the field with the given key (nil when absent).
func (f *Form) Field(key string) *FormField {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	return nil
}

// Value returns a field's current value ("" when the field is absent).
func (f *Form) Value(key string) string {
	if fld := f.Field(key); fld != nil {
		return fld.Value
	}
	return ""
}

// SetValue sets a field's value by key (no-op when the field is absent).
func (f *Form) SetValue(key, val string) {
	if fld := f.Field(key); fld != nil {
		fld.Value = val
	}
}

// Validate checks every field (Required + the field's own validator),
// moves the cursor to the first offender and records Err. Returns the
// first failure, or nil when the form is submittable.
func (f *Form) Validate() error {
	for i := range f.Fields {
		fld := &f.Fields[i]
		if fld.Required && strings.TrimSpace(fld.Value) == "" {
			f.Cursor = i
			f.Err = fld.Label + " is required"
			return fmt.Errorf("%s", f.Err)
		}
		if fld.Validate != nil {
			if err := fld.Validate(fld.Value); err != nil {
				f.Cursor = i
				f.Err = fld.Label + ": " + err.Error()
				return err
			}
		}
	}
	f.Err = ""
	return nil
}

// Update handles one key. submitted/cancelled are the form's two exits;
// both are false for ordinary editing (and for a rejected submit — the
// form stays open with Err set).
func (f *Form) Update(msg tea.KeyMsg) (submitted, cancelled bool) {
	switch msg.String() {
	case "esc":
		return false, true
	case "ctrl+s", "enter":
		return f.Validate() == nil, false
	case "up", "shift+tab":
		f.move(-1)
		return false, false
	case "down", "tab":
		f.move(1)
		return false, false
	case "left":
		f.cycle(-1)
		return false, false
	case "right":
		f.cycle(1)
		return false, false
	case " ":
		if fld := f.current(); fld != nil {
			if len(fld.Options) > 0 {
				f.cycle(1)
			} else {
				fld.Value += " "
			}
		}
		return false, false
	case "backspace":
		if fld := f.current(); fld != nil {
			fld.Value = dropLastRune(fld.Value)
		}
		return false, false
	case "ctrl+u":
		if fld := f.current(); fld != nil {
			fld.Value = ""
		}
		return false, false
	}
	if msg.Type != tea.KeyRunes {
		return false, false
	}
	if fld := f.current(); fld != nil && len(fld.Options) == 0 {
		fld.Value += string(msg.Runes)
	}
	return false, false
}

func (f *Form) current() *FormField {
	if f.Cursor < 0 || f.Cursor >= len(f.Fields) {
		return nil
	}
	return &f.Fields[f.Cursor]
}

func (f *Form) move(delta int) {
	if len(f.Fields) == 0 {
		return
	}
	f.Cursor = ((f.Cursor+delta)%len(f.Fields) + len(f.Fields)) % len(f.Fields)
}

// cycle moves a select field through its options (no-op on text fields).
func (f *Form) cycle(delta int) {
	fld := f.current()
	if fld == nil || len(fld.Options) == 0 {
		return
	}
	idx := 0
	for i, o := range fld.Options {
		if o == fld.Value {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%len(fld.Options) + len(fld.Options)) % len(fld.Options)
	fld.Value = fld.Options[idx]
}

// View renders the form (Frame clips it to the region).
func (f *Form) View() string {
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render(f.Title) + "\n")
	for i := range f.Fields {
		fld := &f.Fields[i]
		cursor := "  "
		if i == f.Cursor {
			cursor = "▸ "
		}
		val := fld.Value
		if strings.TrimSpace(val) == "" {
			val = theme.HintText.Render("—")
		}
		line := cursor + fmt.Sprintf("%-14s", fld.Label) + " " + val
		if i == f.Cursor {
			b.WriteString(theme.ListItemSelected.Render(line) + "\n")
			if len(fld.Options) > 0 {
				b.WriteString(theme.HintText.Render("   options: "+strings.Join(fld.Options, "/")+"  (←/→ or space)") + "\n")
			} else if fld.Hint != "" {
				b.WriteString(theme.HintText.Render("   "+fld.Hint) + "\n")
			}
		} else {
			b.WriteString(theme.ListItem.Render(line) + "\n")
		}
	}
	if f.Err != "" {
		b.WriteString(theme.ErrorText.Render("⚠ "+f.Err) + "\n")
	}
	return b.String()
}

// FieldError is the validating constructor a screen can use inline:
// required + a shape check in one step.
func Required(field *FormField) *FormField {
	field.Required = true
	return field
}

func dropLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}
