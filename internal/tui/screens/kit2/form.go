package kit2

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	// KPicker is a reference-to-another-entity field: instead of typing an ID
	// (which no human knows) the operator types and the matching options are
	// listed as they go, so the choice is MADE from the list. The operator:
	// "The Parent work item requires an ID. There is no way a human is going to
	// know this ... if you type a letter another box comes up similar to the
	// slash command that has all work items that can be selected. Same should
	// go for workflows and runtime images as well."
	KPicker Kind = "picker"
	// KModel is a model_ref field. The concrete choice is made in the dedicated
	// modal ModelPicker (adapter → provider → model, with search) rather than
	// typed, because no human knows a model ref by heart — the operator's "when
	// you enter the field, this one deserves its own modal popup". The field
	// itself is read-only and DISPLAYS the committed adapter/provider/model ref
	// (the operator's "the text it displays after the model is selected is the
	// adapter/provider/model name").
	KModel Kind = "model"
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
	// SubmitErr is why the last save did NOT go through ("" = none). The host
	// keeps the form open on a rejected submit, so this is the only signal the
	// operator gets.
	SubmitErr string
	Width     int
	Height    int
	Focused   bool
	Title     string

	// pos is the CARET position (a rune index) per editable field. Without it
	// the only edit available was appending to the end of the prefilled value,
	// which is why editing an existing item read as "I can't actually edit
	// anything": no way to correct mid-string and no way to clear a field.
	pos map[string]int

	// The open KPicker list: which field it belongs to, the query the operator
	// is typing, and the highlighted option.
	pickerField string
	pickerQuery string
	pickerSel   int

	// OnChange is called after a field's value changes programmatically (a
	// picker commit, Set). A screen uses it to keep DERIVED fields consistent
	// — e.g. the work-item Kind follows the chosen Parent, so the form can
	// never offer a combination the server rejects.
	OnChange func(name, value string)

	// OnOpenModelPicker is invoked when the operator ACTIVATES a KModel field
	// (enter or space). The host opens its ModelPicker seeded with the field's
	// current ref and writes the committed ref back with Set. The picker is the
	// SCREEN's, so it can be layered over either a modal form or the inline
	// detail editor.
	OnOpenModelPicker func(name, current string) tea.Cmd

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
		pos:    map[string]int{},
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

// pickerRows is how many option rows an open picker shows at once.
const pickerRows = 8

// pickerOptions is the options matching the operator's query, so the list
// narrows as they type (matching the label OR the value/id).
func (f *Form) pickerOptions(s *FieldSpec) []Option {
	q := strings.ToLower(strings.TrimSpace(f.pickerQuery))
	out := make([]Option, 0, len(s.Options))
	for _, o := range s.Options {
		if q == "" ||
			strings.Contains(strings.ToLower(o.Label), q) ||
			strings.Contains(strings.ToLower(o.Value), q) {
			out = append(out, o)
		}
	}
	return out
}

func (f *Form) pickerOpen(s *FieldSpec) {
	f.pickerField, f.pickerQuery, f.pickerSel = s.Name, "", 0
}

func (f *Form) pickerClose() {
	f.pickerField, f.pickerQuery, f.pickerSel = "", "", 0
}

// pickerMove moves the option highlight within the FILTERED list.
func (f *Form) pickerMove(delta int) {
	n := len(f.pickerOptions(f.current()))
	if n == 0 {
		return
	}
	f.pickerSel += delta
	if f.pickerSel < 0 {
		f.pickerSel = 0
	}
	if f.pickerSel >= n {
		f.pickerSel = n - 1
	}
}

// pickerChoose commits the highlighted option and closes the list. The VALUE is
// the option's ID — the operator never had to know it or type it.
func (f *Form) pickerChoose(s *FieldSpec) {
	opts := f.pickerOptions(s)
	if len(opts) == 0 {
		return
	}
	sel := f.pickerSel
	if sel < 0 || sel >= len(opts) {
		sel = 0
	}
	f.Values[s.Name] = opts[sel].Value
	f.setCaret(s.Name, len([]rune(opts[sel].Value)))
	if f.OnChange != nil {
		f.OnChange(s.Name, opts[sel].Value)
	}
	f.pickerClose()
}

// pickerKey handles a key aimed at a focused KPicker. handled=false falls
// through to the ordinary field handling (so up/down still walk FIELDS while
// the list is closed, and tab still leaves the field).
func (f *Form) pickerKey(s *FieldSpec, k keyMsg) (tea.Cmd, bool) {
	open := f.pickerField == s.Name
	switch k.String() {
	case "enter":
		if open {
			opts := f.pickerOptions(s)
			if len(opts) == 0 && strings.TrimSpace(f.pickerQuery) != "" {
				// FREE-FORM: the operator typed an exact value that matches no
				// preset. Commit it, so a value can be CUSTOMIZED — with the
				// field's own validator reporting a malformed one.
				v := strings.TrimSpace(f.pickerQuery)
				f.Values[s.Name] = v
				f.setCaret(s.Name, len([]rune(v)))
				if f.OnChange != nil {
					f.OnChange(s.Name, v)
				}
				f.pickerClose()
			} else {
				f.pickerChoose(s)
			}
		} else {
			f.pickerOpen(s)
		}
		return nil, true
	case " ", "space":
		if !open {
			f.pickerOpen(s)
			return nil, true
		}
		// While the list is open a space is part of the QUERY ("sweeper
		// retry"), not a commit.
		f.pickerQuery += " "
		f.pickerSel = 0
		return nil, true
	case "up":
		if open {
			f.pickerMove(-1)
			return nil, true
		}
		return nil, false
	case "down":
		if open {
			f.pickerMove(1)
			return nil, true
		}
		return nil, false
	case "esc":
		if open {
			f.pickerClose()
			return nil, true
		}
		return nil, false // the caller closes the form
	case "backspace":
		if open {
			q := []rune(f.pickerQuery)
			if len(q) > 0 {
				f.pickerQuery = string(q[:len(q)-1])
				f.pickerSel = 0
			}
			return nil, true
		}
		return nil, false
	case "tab", "shift+tab":
		// Leaving the field closes the list.
		if open {
			f.pickerClose()
		}
		return nil, false
	}
	if len(k.Runes) > 0 {
		// Typing OPENS the list and filters it — the gesture the operator asked
		// for ("if you type a letter another box comes up").
		if !open {
			f.pickerOpen(s)
		}
		f.pickerQuery += string(k.Runes)
		f.pickerSel = 0
		return nil, true
	}
	return nil, false
}

// Set assigns a field value (used by tests and programmatic prefill). The caret
// moves to the end, which is where an operator continues typing.
func (f *Form) Set(name, value string) {
	f.Values[name] = value
	if f.pos == nil {
		f.pos = map[string]int{}
	}
	f.pos[name] = len([]rune(value))
	if f.OnChange != nil {
		f.OnChange(name, value)
	}
}

// caret returns the rune index of the caret within the named field's value,
// clamped to the value's length (an unvisited field starts at the end, which is
// where appending used to happen, so nothing about typing at a fresh field
// changes).
func (f *Form) caret(name string) int {
	n := len([]rune(f.Values[name]))
	p, ok := f.pos[name]
	if !ok || p > n {
		return n
	}
	if p < 0 {
		return 0
	}
	return p
}

func (f *Form) setCaret(name string, p int) {
	if f.pos == nil {
		f.pos = map[string]int{}
	}
	n := len([]rune(f.Values[name]))
	if p < 0 {
		p = 0
	}
	if p > n {
		p = n
	}
	f.pos[name] = p
}

// insertRunes splices text into a field at the caret.
func (f *Form) insertRunes(name, ins string) {
	v := []rune(f.Values[name])
	p := f.caret(name)
	out := make([]rune, 0, len(v)+len(ins))
	out = append(out, v[:p]...)
	out = append(out, []rune(ins)...)
	out = append(out, v[p:]...)
	f.Values[name] = string(out)
	f.setCaret(name, p+len([]rune(ins)))
}

// backspaceRune deletes the rune BEFORE the caret; deleteRune deletes the rune
// AT the caret.
func (f *Form) backspaceRune(name string) {
	v := []rune(f.Values[name])
	p := f.caret(name)
	if p == 0 || len(v) == 0 {
		return
	}
	f.Values[name] = string(append(v[:p-1], v[p:]...))
	f.setCaret(name, p-1)
}

func (f *Form) deleteRune(name string) {
	v := []rune(f.Values[name])
	p := f.caret(name)
	if p >= len(v) {
		return
	}
	f.Values[name] = string(append(v[:p], v[p+1:]...))
	f.setCaret(name, p)
}

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
// Tab/Shift+Tab and Up/Down move field focus (shared focus model); Enter on
// the last field submits; Esc cancels (the caller closes the form).
//
// Up/Down walk the fields and are CONSUMED (handled=true) rather than falling
// through: a modal must own the vertical keys, so the list behind it can never
// scroll while it is up (the operator's "the arrow keys control the work item
// list instead of moving through the field values").
func (f *Form) HandleKey(k keyMsg) (tea.Cmd, bool) {
	s := f.current()
	// A model field is a REFERENCE, not text: enter/space opens the host's model
	// picker instead of advancing or editing (there is nothing a human could
	// usefully type into it).
	if s != nil && s.Kind == KModel {
		switch k.String() {
		case "enter", " ", "space":
			if f.OnOpenModelPicker != nil {
				return f.OnOpenModelPicker(s.Name, f.Values[s.Name]), true
			}
			return nil, true
		}
	}
	if s != nil && s.Kind == KPicker {
		if cmd, handled := f.pickerKey(s, k); handled {
			return cmd, true
		}
	}
	switch k.String() {
	case "tab":
		f.Next()
		return nil, true
	case "shift+tab":
		f.Prev()
		return nil, true
	case "down":
		// Vertical keys walk the FIELDS. The operator's report was that the
		// arrows "still control the work item list versus moving through the
		// New Work Item fields": a form must own the two vertical keys so
		// nothing can move behind an open modal. Left/Right keep the
		// select/checkbox cycling below (a horizontal gesture on a value).
		f.Next()
		return nil, true
	case "up":
		f.Prev()
		return nil, true
	case "ctrl+s":
		// Save. The operator: "On new work items, I think submit should be
		// ctrl+s not enter. Enter is already used on individual items to
		// select things so this is not working well."
		cmd, _ := f.Submit()
		return cmd, true
	case "enter":
		// Enter NEVER submits: on a form full of pickers and selectors it is
		// the CHOOSE gesture, and overloading it made selecting a value
		// commit the whole form. It advances to the next field, and is a
		// no-op on the last one (ctrl+s is the one save chord).
		if f.Cursor < len(f.Specs)-1 {
			f.Next()
		}
		return nil, true
	case "esc":
		return nil, false // caller closes the form
	case "backspace":
		if s != nil && f.editable(s.Kind) {
			f.backspaceRune(s.Name)
		}
		return nil, true
	case "delete":
		if s != nil && f.editable(s.Kind) {
			f.deleteRune(s.Name)
		}
		return nil, true
	case "home":
		if s != nil && f.editable(s.Kind) {
			f.setCaret(s.Name, 0)
		}
		return nil, true
	case "end":
		if s != nil && f.editable(s.Kind) {
			f.setCaret(s.Name, len([]rune(f.Values[s.Name])))
		}
		return nil, true
	case "ctrl+u":
		// Clear the field. Without a clear gesture an operator could only ever
		// APPEND to a prefilled value, which is the difference between editing
		// an item and being unable to.
		if s != nil && f.editable(s.Kind) {
			f.Values[s.Name] = ""
			f.setCaret(s.Name, 0)
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
			// An editable TEXT field uses left/right to move the CARET — the
			// gesture an operator expects inside a text box.
			if f.editable(s.Kind) {
				f.setCaret(s.Name, f.caret(s.Name)+d)
				return nil, true
			}
		}
	}
	if s != nil && len(k.Runes) > 0 && f.editable(s.Kind) {
		f.insertRunes(s.Name, string(k.Runes))
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
	if err != nil {
		// A rejected submit must SAY why. The host keeps the form open (Submitted
		// stays false), so without this the operator pressed save and saw
		// nothing happen — the same silent-rejection class as a swallowed RPC
		// error.
		f.SubmitErr = err.Error()
		return cmd, err
	}
	f.SubmitErr = ""
	f.Submitted = true
	return cmd, nil
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
	case KPicker:
		// Show the CHOSEN option's label — never the raw id the operator
		// cannot be expected to know.
		for _, o := range s.Options {
			if o.Value == v {
				if o.Label != "" {
					return o.Label
				}
				return o.Value
			}
		}
		return v
	case KModel:
		// Show the committed adapter/provider/model ref; when unset, always show
		// the affordance (not only while focused) so a blank model row reads as
		// "unset — go choose one" rather than "not applicable".
		if v == "" {
			if s.Placeholder != "" {
				return s.Placeholder
			}
			return "— none —"
		}
		return v
	default:
		if v == "" && f.Focused && f.current() != nil && f.current().Name == s.Name {
			return s.Placeholder
		}
		return v
	}
}

// valueWithCaret renders an editable field's value with a caret at the edit
// position, windowed to at most `avail` cells so the caret is ALWAYS visible.
// A clipped edge is marked with an ellipsis (the same cell the character would
// have used, so the width stays exact).
func (f *Form) valueWithCaret(name string, avail int) string {
	const caretRune = "\u258f" // ▏
	if avail < 2 {
		avail = 2
	}
	runes := []rune(f.Values[name])
	pos := f.caret(name)
	if pos > len(runes) {
		pos = len(runes)
	}
	// The whole value plus the caret fits: no windowing needed.
	if len(runes)+1 <= avail {
		return string(runes[:pos]) + caretRune + string(runes[pos:])
	}
	win := avail - 1 // one cell reserved for the caret
	start := 0
	if pos > win-1 {
		start = pos - (win - 1)
	}
	if start > len(runes)-win {
		start = len(runes) - win
	}
	if start < 0 {
		start = 0
	}
	end := start + win
	shown := append([]rune{}, runes[start:end]...)
	if start > 0 {
		shown[0] = '…'
	}
	if end < len(runes) {
		shown[len(shown)-1] = '…'
	}
	idx := pos - start
	if idx < 0 {
		idx = 0
	}
	if idx > len(shown) {
		idx = len(shown)
	}
	return string(shown[:idx]) + caretRune + string(shown[idx:])
}

// writePickerList renders an open picker's matches (windowed around the
// highlight) plus the gesture hint.
func (f *Form) writePickerList(b *strings.Builder, s *FieldSpec, width int) {
	opts := f.pickerOptions(s)
	if len(opts) == 0 {
		// Nothing matches, but the typed text can still be COMMITTED as a custom
		// value (enter), so the row explains that instead of dead-ending.
		b.WriteString(theme.HintText.Render(Pad("    (no match — enter uses \""+strings.TrimSpace(f.pickerQuery)+"\")", width)) + "\n")
		b.WriteString(theme.HintText.Render(Pad("    ↑/↓ pick · enter select · esc close", width)) + "\n")
		return
	}
	start := 0
	if f.pickerSel >= pickerRows {
		start = f.pickerSel - pickerRows + 1
	}
	end := start + pickerRows
	if end > len(opts) {
		end = len(opts)
	}
	for j := start; j < end; j++ {
		mark := "    "
		if j == f.pickerSel {
			mark = "  ▸ "
		}
		line := mark + opts[j].Label
		if j == f.pickerSel {
			b.WriteString(theme.ListItemSelected.Render(Pad(line, width)) + "\n")
		} else {
			b.WriteString(theme.ListItem.Render(line) + "\n")
		}
	}
	b.WriteString(theme.HintText.Render(Pad("    ↑/↓ pick · enter select · esc close", width)) + "\n")
}

// View renders the form body.
func (f *Form) View() string {
	// A form rendered without an explicit width still needs a sane one: Pad
	// truncates to it, so width 0 erased every line (the Work screen's forms
	// never set it) and windowing needs it to keep the caret visible.
	width := f.Width
	if width <= 0 {
		width = 64
	}
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
		focused := f.Focused && i == f.Cursor
		if focused {
			cursor = "▸ "
		}
		prefix := cursor + label + ": "
		var line string
		switch {
		case focused && s.Kind == KPicker:
			// While the list is open the field shows the QUERY being typed;
			// closed it shows the choice that was made.
			if f.pickerField == s.Name {
				line = prefix + f.pickerQuery + "\u258f"
			} else {
				line = prefix + f.display(s)
			}
		case focused && f.editable(s.Kind) && width > 0:
			// An editable field shows a CARET and horizontally windows its
			// value, so the character being edited is always on screen. The
			// append-only editor it replaced silently pushed every keystroke
			// past the pane width, which read as "I can't edit anything".
			avail := width - lipgloss.Width(prefix)
			if avail < 8 {
				avail = 8
			}
			line = prefix + f.valueWithCaret(s.Name, avail)
		default:
			val := f.display(s)
			line = prefix + val
			if s.Kind == KSecret && val != "" {
				line += theme.HintText.Render("  (hidden)")
			}
			if s.Kind == KModel && focused {
				// The field names the modal that sets it, so the enter gesture is
				// discoverable from the field itself.
				hint := "  enter: choose model"
				if f.Values[s.Name] != "" {
					hint = "  enter: change model"
				}
				line += theme.HintText.Render(hint)
			}
		}
		if i == f.Cursor && f.Focused {
			for _, l := range wrapFormLine(line, width) {
				b.WriteString(theme.ListItemSelected.Render(Pad(l, width)))
				b.WriteString("\n")
			}
		} else {
			for _, l := range wrapFormLine(line, width) {
				b.WriteString(theme.ListItem.Render(l))
				b.WriteString("\n")
			}
		}
		if err := f.Errors[s.Name]; err != "" {
			for _, l := range wrapFormLine("    ✗ "+err, width) {
				b.WriteString(theme.ErrorText.Render(l))
				b.WriteString("\n")
			}
		}
		// An open picker lists its matches directly under the field, so the
		// operator SEES the choices while typing (the operator's "another box
		// comes up similar to the slash command").
		if s.Kind == KPicker && f.pickerField == s.Name {
			f.writePickerList(&b, &f.Specs[i], width)
		}
	}
	// The hint row wraps at its " · " boundaries so a KEY and its meaning stay
	// together — the operator's "it is wrapping the tool title and shortcut on
	// separate lines, it should keep those together". A plain word wrap broke
	// "↑/↓ or tab: field" mid-phrase.
	for _, l := range wrapHint(theme.HintText.Render("↑/↓ or tab: field · ←/→: move · ctrl+u: clear · enter: next · ctrl+s: save · esc: cancel"), width) {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if f.SubmitErr != "" {
		for _, l := range wrapFormLine(theme.ErrorText.Render("✗ "+f.SubmitErr), width) {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// wrapHint breaks a " · "-separated hint into lines that fit `width` cells
// WITHOUT splitting a segment: "ctrl+s: save · esc: cancel" becomes its own
// line rather than "ctrl+s:" / "save · esc:" / "cancel". A single segment that
// cannot fit is wrapped by words as a last resort. Shared with the composer,
// which previously truncated the same hint.
func wrapHint(line string, width int) []string {
	if width < 8 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	segs := strings.Split(line, " · ")
	out := make([]string, 0, len(segs))
	cur := ""
	for i, seg := range segs {
		piece := seg
		if i > 0 {
			piece = "· " + seg
		}
		switch {
		case cur == "":
			cur = piece
		case lipgloss.Width(cur)+1+lipgloss.Width(piece) <= width:
			cur += " " + piece
		default:
			out = append(out, cur)
			cur = piece
		}
		// A segment too long for a line on its own falls back to a word wrap.
		for lipgloss.Width(cur) > width {
			words := strings.SplitAfter(cur, " ")
			filled := ""
			rest := ""
			for _, w := range words {
				if rest != "" || lipgloss.Width(filled)+lipgloss.Width(w) > width {
					rest += w
					continue
				}
				filled += w
			}
			if filled == "" {
				break
			}
			out = append(out, strings.TrimRight(filled, " "))
			cur = strings.TrimLeft(rest, " ")
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// WrapHint is the exported form of the segment-aware hint wrap, so the composer
// (a different package) wraps its affordance row the same way instead of
// truncating it.
func WrapHint(line string, width int) []string { return wrapHint(line, width) }

// wrapFormLine breaks a rendered line so it fits `width` cells, wrapping at
// word boundaries and indenting continuations to match the leading prefix. The
// form's host pads (and truncates) each body line to the pane width, so a line
// longer than the form was silently CUT — long labels, long values and the hint
// row all lost their tails.
func wrapFormLine(line string, width int) []string {
	if width < 8 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	// The continuation indent mirrors the field's leading marker + label so a
	// wrapped value reads as part of the same field.
	indent := "    "
	if i := strings.Index(line, ": "); i >= 0 && i < width/2 {
		indent = strings.Repeat(" ", i+2)
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return []string{line}
	}
	out := make([]string, 0, 2)
	cur := ""
	for _, w := range words {
		switch {
		case cur == "":
			cur = w
		case lipgloss.Width(cur)+1+lipgloss.Width(w) <= width:
			cur += " " + w
		default:
			out = append(out, cur)
			cur = indent + w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
