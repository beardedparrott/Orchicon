package kit2

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// Kind is a form field type.
type Kind string

const (
	KText        Kind = "text"
	KTextArea    Kind = "textarea"
	KSelect      Kind = "select"
	KMultiSelect Kind = "multi-select"
	KSecret      Kind = "secret"
	KJSON        Kind = "json"
	KYAML        Kind = "yaml"
	KCheckbox    Kind = "checkbox"
	KNumber      Kind = "number"
)

// Option is one select choice.
type Option struct {
	Value string
	Label string
}

// FieldSpec declares one typed field.
type FieldSpec struct {
	Name        string
	Label       string
	Kind        Kind
	Options     []Option
	Required    bool
	Placeholder string
	Initial     string
	// Validate returns a field error ("" = valid). It runs on submit.
	Validate func(string) error
}

// Form is a typed, validated input collection that submits through the
// mutation layer. Values are keyed by FieldSpec.Name.
//
// Security: a `secret` field's raw value is held only in Values (for the
// RPC) and is NEVER rendered — View() draws a fixed mask instead. That is
// asserted by a test.
type Form struct {
	Specs     []FieldSpec
	Values    map[string]string
	Multi     map[string]map[string]bool
	Cursor    int
	Errors    map[string]string
	Submitted bool
	Width     int
	Height    int
	Focused   bool
	Title     string

	// OnSubmit is invoked by Submit with a copy of the collected values. It
	// is where the screen wires the mutation executor; the returned cmd (the
	// mutation's async RPC) is handed back to the bubbletea update loop.
	OnSubmit func(values map[string]string, multi map[string][]string) (tea.Cmd, error)
}

// NewForm builds a form from field specs.
func NewForm(title string, specs ...FieldSpec) *Form {
	f := &Form{
		Title:  title,
		Specs:  specs,
		Values: map[string]string{},
		Multi:  map[string]map[string]bool{},
		Errors: map[string]string{},
	}
	for _, s := range specs {
		if s.Initial != "" {
			f.Values[s.Name] = s.Initial
		}
		if s.Kind == KMultiSelect {
			f.Multi[s.Name] = map[string]bool{}
		}
	}
	return f
}

// current returns the focused field spec (nil when the form is empty).
func (f *Form) current() *FieldSpec {
	if f.Cursor < 0 || f.Cursor >= len(f.Specs) {
		return nil
	}
	return &f.Specs[f.Cursor]
}

// CurrentName returns the focused field's name.
func (f *Form) CurrentName() string {
	if c := f.current(); c != nil {
		return c.Name
	}
	return ""
}

// Set assigns a field value (used by tests and programmatic prefill).
func (f *Form) Set(name, value string) { f.Values[name] = value }

// SetMulti toggles a multi-select option.
func (f *Form) SetMulti(name, value string, on bool) {
	if f.Multi[name] == nil {
		f.Multi[name] = map[string]bool{}
	}
	f.Multi[name][value] = on
}

// FocusName focuses a field by name.
func (f *Form) FocusName(name string) bool {
	for i, s := range f.Specs {
		if s.Name == name {
			f.Cursor = i
			return true
		}
	}
	return false
}

// Next / Prev move field focus (Tab / Shift+Tab).
func (f *Form) Next() {
	if len(f.Specs) == 0 {
		return
	}
	f.Cursor = (f.Cursor + 1) % len(f.Specs)
}

func (f *Form) Prev() {
	if len(f.Specs) == 0 {
		return
	}
	f.Cursor = (f.Cursor - 1 + len(f.Specs)) % len(f.Specs)
}

// multibool: does a select field's value list contain v
func inOptions(spec FieldSpec, v string) bool {
	for _, o := range spec.Options {
		if o.Value == v {
			return true
		}
	}
	return false
}

// HandleKey drives form editing. Returns true when the message was consumed.
// Tab/Shift+Tab move field focus (shared focus model); Enter on the last
// field submits; Esc cancels (the caller closes the form).
func (f *Form) HandleKey(k keyMsg) (tea.Cmd, bool) {
	s := f.current()
	switch k.String() {
	case "tab":
		f.Next()
		return nil, true
	case "shift+tab":
		f.Prev()
		return nil, true
	case "enter":
		if f.Cursor == len(f.Specs)-1 || k.Alt {
			cmd, _ := f.Submit()
			return cmd, true
		}
		f.Next()
		return nil, true
	case "esc":
		return nil, false // caller closes the form
	case "backspace":
		if s != nil && f.editable(s.Kind) {
			v := []rune(f.Values[s.Name])
			if len(v) > 0 {
				f.Values[s.Name] = string(v[:len(v)-1])
			}
		}
		return nil, true
	case " ", "space":
		if s != nil {
			switch s.Kind {
			case KCheckbox:
				f.Values[s.Name] = toggleBool(f.Values[s.Name])
				return nil, true
			case KSelect:
				f.cycleSelect(s, 1)
				return nil, true
			case KMultiSelect:
				f.cycleMulti(s)
				return nil, true
			}
		}
	case "left", "right":
		if s != nil {
			d := 1
			if k.String() == "left" {
				d = -1
			}
			switch s.Kind {
			case KSelect:
				f.cycleSelect(s, d)
				return nil, true
			case KCheckbox:
				f.Values[s.Name] = toggleBool(f.Values[s.Name])
				return nil, true
			}
		}
	}
	if s != nil && len(k.Runes) > 0 && f.editable(s.Kind) {
		f.Values[s.Name] += string(k.Runes)
		return nil, true
	}
	return nil, false
}

func (f *Form) editable(k Kind) bool {
	switch k {
	case KText, KTextArea, KSecret, KJSON, KYAML, KNumber:
		return true
	}
	return false
}

func (f *Form) cycleSelect(s *FieldSpec, d int) {
	if len(s.Options) == 0 {
		return
	}
	cur := -1
	for i, o := range s.Options {
		if o.Value == f.Values[s.Name] {
			cur = i
		}
	}
	n := len(s.Options)
	cur = ((cur+d)%n + n) % n
	f.Values[s.Name] = s.Options[cur].Value
}

func (f *Form) cycleMulti(s *FieldSpec) {
	// space/left-right toggles the FIRST option in the absence of a
	// per-option cursor; real multi-select rows are toggled by SetMulti.
	if len(s.Options) == 0 {
		return
	}
	v := s.Options[0].Value
	if f.Multi[s.Name] == nil {
		f.Multi[s.Name] = map[string]bool{}
	}
	f.Multi[s.Name][v] = !f.Multi[s.Name][v]
}

func toggleBool(s string) string {
	if s == "true" {
		return "false"
	}
	return "true"
}

// Validate runs per-field validation and type checks, populating Errors.
func (f *Form) Validate() bool {
	f.Errors = map[string]string{}
	for _, s := range f.Specs {
		v := f.Values[s.Name]
		switch s.Kind {
		case KMultiSelect:
			if s.Required && len(f.MultiValues(s.Name)) == 0 {
				f.Errors[s.Name] = "select at least one"
			}
			continue
		case KCheckbox:
			continue
		}
		if s.Required && strings.TrimSpace(v) == "" {
			f.Errors[s.Name] = "required"
			continue
		}
		if v != "" {
			switch s.Kind {
			case KNumber:
				if _, err := strconv.ParseFloat(v, 64); err != nil {
					f.Errors[s.Name] = "not a number"
				}
			case KJSON:
				if !json.Valid([]byte(v)) {
					f.Errors[s.Name] = "invalid JSON"
				}
			case KSelect:
				if !inOptions(s, v) {
					f.Errors[s.Name] = "unknown option"
				}
			}
		}
		if s.Validate != nil {
			if err := s.Validate(v); err != nil {
				f.Errors[s.Name] = err.Error()
			}
		}
	}
	return len(f.Errors) == 0
}

// MultiValues returns the selected values of a multi-select field.
func (f *Form) MultiValues(name string) []string {
	m := f.Multi[name]
	out := make([]string, 0, len(m))
	for k, on := range m {
		if on {
			out = append(out, k)
		}
	}
	return out
}

// Submit validates and invokes OnSubmit with a copy of the collected
// values. It returns the mutation cmd produced by OnSubmit (nil when the
// form is invalid or has no handler).
func (f *Form) Submit() (tea.Cmd, error) {
	if !f.Validate() {
		return nil, fmt.Errorf("form has errors")
	}
	vals := make(map[string]string, len(f.Values))
	for k, v := range f.Values {
		vals[k] = v
	}
	multi := map[string][]string{}
	for _, s := range f.Specs {
		if s.Kind == KMultiSelect {
			multi[s.Name] = f.MultiValues(s.Name)
		}
	}
	if f.OnSubmit == nil {
		f.Submitted = true
		return nil, nil
	}
	cmd, err := f.OnSubmit(vals, multi)
	if err == nil {
		f.Submitted = true
	}
	return cmd, err
}

// display renders a field's value for the view. A secret field NEVER
// returns its raw value — it returns a fixed mask.
func (f *Form) display(s FieldSpec) string {
	v := f.Values[s.Name]
	switch s.Kind {
	case KSecret:
		if v == "" {
			return ""
		}
		return strings.Repeat("•", 8)
	case KCheckbox:
		if v == "true" {
			return "[x]"
		}
		return "[ ]"
	case KMultiSelect:
		sel := f.MultiValues(s.Name)
		if len(sel) == 0 {
			return "(none)"
		}
		return strings.Join(sel, ", ")
	case KSelect:
		if v == "" && len(s.Options) > 0 {
			return s.Options[0].Label
		}
		for _, o := range s.Options {
			if o.Value == v {
				if o.Label != "" {
					return o.Label
				}
				return o.Value
			}
		}
		return v
	default:
		if v == "" && f.Focused && f.current() != nil && f.current().Name == s.Name {
			return s.Placeholder
		}
		return v
	}
}

// View renders the form body.
func (f *Form) View() string {
	var b strings.Builder
	if f.Title != "" {
		b.WriteString(theme.ListTitle.Render(f.Title))
		b.WriteString("\n")
	}
	for i, s := range f.Specs {
		label := s.Label
		if label == "" {
			label = s.Name
		}
		if s.Required {
			label += " *"
		}
		cursor := "  "
		if f.Focused && i == f.Cursor {
			cursor = "▸ "
		}
		val := f.display(s)
		line := fmt.Sprintf("%s%s: %s", cursor, label, val)
		if s.Kind == KSecret && val != "" {
			line += theme.HintText.Render("  (hidden)")
		}
		if i == f.Cursor && f.Focused {
			b.WriteString(theme.ListItemSelected.Render(Pad(line, f.Width)))
		} else {
			b.WriteString(theme.ListItem.Render(line))
		}
		b.WriteString("\n")
		if err := f.Errors[s.Name]; err != "" {
			b.WriteString(theme.ErrorText.Render("    ✗ " + err))
			b.WriteString("\n")
		}
	}
	b.WriteString(theme.HintText.Render("tab: next field · enter: submit · esc: cancel"))
	return strings.TrimSuffix(b.String(), "\n")
}
