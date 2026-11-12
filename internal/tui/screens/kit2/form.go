package kit2

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/md"
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
	// KPicker is a reference-to-another-entity field: instead of typing an ID (which no human knows)
	// the operator types and the matching options are listed as they go.
	KPicker Kind = "picker"
	// KModel is a model_ref field.
	KModel Kind = "model"
	// KDate is a DATE field.
	KDate Kind = "date"
	// KDateTime is a DATE-AND-TIME field: an INSTANT chosen from the host's modal calendar + clock
	// (kit2.DateTimePicker) rather than typed.
	KDateTime Kind = "date-time"
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
	// Visible, when set, decides whether the field exists RIGHT NOW, given the form's current values.
	Visible func(values map[string]string) bool

	// NoPreview declares that this field holds CODE, not prose, so the rendered-markdown view (ctrl+p)
	// is never offered on it.
	NoPreview bool

	// Display, when set, renders the value for the operator WITHOUT changing what is stored.
	Display func(string) string
}

// Security: a `secret` field's raw value is held only in Values (for the RPC) and is NEVER rendered
// — View() draws a fixed mask instead.
type Form struct {
	Specs     []FieldSpec
	Values    map[string]string
	Multi     map[string]map[string]bool
	Cursor    int
	Errors    map[string]string
	Submitted bool
	// SubmitErr is why the last save did NOT go through ("" = none).
	SubmitErr string
	Width     int
	Height    int
	Focused   bool
	Title     string

	// Note is a paragraph drawn under the title: context the operator needs in order to fill the form
	// in correctly that no single field owns.
	Note string

	// pos is the CARET position (a rune index) per editable field.
	pos map[string]int

	// multiCur is the per-option cursor of a multi-select field (which weekday
	// the operator is on), so space toggles the option they are LOOKING at.
	multiCur map[string]int

	// The open KPicker list: which field it belongs to, the query the operator
	// is typing, and the highlighted option.
	pickerField string
	pickerQuery string
	pickerSel   int

	// OnChange is called after a field's value changes programmatically (a picker commit, Set).
	OnChange func(name, value string)

	// OnOpenModelPicker is invoked when the operator ACTIVATES a KModel field (enter or space).
	OnOpenModelPicker func(name, current string) tea.Cmd

	// OnOpenDatePicker is invoked when the operator ACTIVATES a KDate field.
	OnOpenDatePicker func(name, current string) tea.Cmd

	// OnOpenDateTimePicker is invoked when the operator ACTIVATES a KDateTime field.
	OnOpenDateTimePicker func(name, current string) tea.Cmd

	// editing is the field LOCKED for text editing, entered with Enter on a multi-line field and left
	// with Esc.
	editing string

	// preview is the field currently shown as RENDERED MARKDOWN, toggled with ctrl+p.
	preview string

	// expanded is the field currently rendered WIDE, toggled with ctrl+e.
	expanded string

	// OnSubmit is invoked by Submit with a copy of the collected values.
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
		// FOCUSED BY DEFAULT, because a form is only ever constructed to be handed to the operator — there
		// is no such thing as a form being built for nobody.
		Focused: true,
		pos:     map[string]int{},
	}
	for _, s := range specs {
		if s.Initial != "" {
			f.Values[s.Name] = s.Initial
		}
		if s.Kind == KMultiSelect {
			f.Multi[s.Name] = map[string]bool{}
			// Seed from Initial: a COMMA-SEPARATED list of option values.
			for _, v := range strings.Split(s.Initial, ",") {
				if v = strings.TrimSpace(v); v != "" {
					f.Multi[s.Name][v] = true
				}
			}
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

// pickerKey handles a key aimed at a focused KPicker.
func (f *Form) pickerKey(s *FieldSpec, k keyMsg) (tea.Cmd, bool) {
	open := f.pickerField == s.Name
	switch k.String() {
	case "enter":
		if open {
			opts := f.pickerOptions(s)
			if len(opts) == 0 && strings.TrimSpace(f.pickerQuery) != "" {
				// FREE-FORM: the operator typed an exact value that matches no preset.
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

// moveCaretLine moves the caret to the same COLUMN on the previous/next logical line, which is how
// every text editor's up/down behaves.
func (f *Form) moveCaretLine(name string, delta int) {
	val := []rune(f.Values[name])
	pos := f.caret(name)
	if pos > len(val) {
		pos = len(val)
	}
	// The line the caret is on, and the column within it.
	start := pos
	for start > 0 && val[start-1] != '\n' {
		start--
	}
	col := pos - start
	// The target line's start.
	var tStart int
	if delta < 0 {
		if start == 0 {
			return // already on the first line
		}
		tStart = start - 1
		for tStart > 0 && val[tStart-1] != '\n' {
			tStart--
		}
	} else {
		end := pos
		for end < len(val) && val[end] != '\n' {
			end++
		}
		if end >= len(val) {
			return // already on the last line
		}
		tStart = end + 1
	}
	// The target line's end, so the caret clamps to the shorter line's length.
	tEnd := tStart
	for tEnd < len(val) && val[tEnd] != '\n' {
		tEnd++
	}
	t := tStart + col
	if t > tEnd {
		t = tEnd
	}
	f.setCaret(name, t)
}

// EditingField returns the name of the field currently locked for editing, or "".
func (f *Form) EditingField() string { return f.editing }

// Wheel scrolls the locked field's view by delta lines, by moving the CARET — the caret IS the
// scroll position (see wrappedBody).
func (f *Form) Wheel(delta int) bool {
	if f.editing == "" {
		return false
	}
	d := 0
	switch {
	case delta < 0:
		d = -1
	case delta > 0:
		d = 1
	}
	for i := 0; i < abs(delta); i++ {
		f.moveCaretLine(f.editing, d)
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
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

// caret returns the rune index of the caret within the named field's value, clamped to the value's
// length (an unvisited field starts at the end, which is where appending used to happen.
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

// PasteText inserts PASTED text into the focused field, and is the one path a paste takes — so the
// sanitising below is stated once rather than at every call site.
func (f *Form) PasteText(text string) bool {
	f.normalizeCursor()
	s := f.current()
	if s == nil {
		return false
	}
	// Newlines and tabs collapse to spaces for every field that shows ONE LINE, and are preserved for the
	// multi-line kinds, where they are the content.
	if s.Kind != KTextArea && s.Kind != KJSON && s.Kind != KYAML {
		text = strings.Join(strings.Fields(text), " ")
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	// A PICKER IS A CHOSEN VALUE, NOT A TEXT BUFFER.
	if s.Kind == KPicker {
		text = sanitizeInput(text)
		f.Values[s.Name] = text
		f.setCaret(s.Name, len([]rune(text)))
		if f.OnChange != nil {
			f.OnChange(s.Name, text)
		}
		return true
	}
	if !f.editable(s.Kind) {
		return false
	}
	f.insertRunes(s.Name, text)
	if f.OnChange != nil {
		f.OnChange(s.Name, f.Values[s.Name])
	}
	return true
}

// sanitizeInput removes CONTROL RUNES that no field may hold, and it exists because of a real report.
//
// REPORTED (Windows TUI, creating a project):
//
//	internal: db: create project: ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)
//
// A NUL had reached a TEXT column, and Postgres refuses one outright. It is INVISIBLE on screen, which is
// what makes it so expensive: the field reads "Temp" and the server rejects it for a byte the operator
// cannot see. NULs arrive from a TERMINAL far more readily than from a keyboard — a Windows console
// delivers 0x00 for several chords and a paste can carry a trailing one — so the form must never be able to
// hold one, whatever produced it.
//
// Newline and tab SURVIVE, because the multi-line kinds legitimately contain them (this function is applied
// before the single-line collapse in PasteText, which turns them into spaces where they are not wanted).
// Everything else below 0x20, and DEL, is dropped: there is no field in this product whose value is a
// control character, and every one of them is invisible.
func sanitizeInput(s string) string {
	if !strings.ContainsFunc(s, isControlRune) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControlRune(r) {
			return -1
		}
		return r
	}, s)
}

// isControlRune reports whether r is a C0 control character or DEL, other than newline/tab.
func isControlRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r < 0x20 || r == 0x7f
}

// insertRunes splices text into a field at the caret.
func (f *Form) insertRunes(name, ins string) {
	ins = sanitizeInput(ins)
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
func (f *Form) Spec(name string) *FieldSpec {
	for i := range f.Specs {
		if f.Specs[i].Name == name {
			return &f.Specs[i]
		}
	}
	return nil
}

// FocusName moves the cursor to the named field. Reports false when the form has no such field.
func (f *Form) FocusName(name string) bool {
	for i, s := range f.Specs {
		if s.Name == name {
			f.Cursor = i
			// A lock belongs to the field it was taken on; focusing elsewhere releases it
			// for the same reason step() does (see there).
			if f.editing != name {
				f.editing = ""
			}
			return true
		}
	}
	return false
}

// Next / Prev move field focus (Tab / Shift+Tab).
func (f *Form) Next() { f.step(1) }

func (f *Form) Prev() { f.step(-1) }

// step moves the cursor by delta, landing only on a visible field. With no visible field it
// leaves the cursor where it is.
func (f *Form) step(delta int) {
	n := len(f.Specs)
	if n == 0 {
		return
	}
	for i := 1; i <= n; i++ {
		idx := ((f.Cursor+delta*i)%n + n) % n
		if f.visibleAt(idx) {
			f.Cursor = idx
			// MOVING FIELDS ALWAYS RELEASES THE EDIT LOCK.
			f.editing = ""
			return
		}
	}
}

// visibleAt reports whether the spec at index idx is currently visible.
func (f *Form) visibleAt(idx int) bool {
	if idx < 0 || idx >= len(f.Specs) {
		return false
	}
	return f.fieldVisible(f.Specs[idx])
}

// fieldVisible evaluates a spec's Visible predicate against the CURRENT values.
func (f *Form) fieldVisible(s FieldSpec) bool {
	if s.Visible == nil {
		return true
	}
	return s.Visible(f.Values)
}

// normalizeCursor moves the cursor off a field that has just become invisible — the state a value
// change leaves behind (choosing "approval" in the kind select hides the worker fields the cursor.
func (f *Form) normalizeCursor() {
	if f.visibleAt(f.Cursor) {
		return
	}
	n := len(f.Specs)
	for i := 1; i <= n; i++ {
		idx := (f.Cursor + i) % n
		if f.visibleAt(idx) {
			f.Cursor = idx
			f.editing = ""
			return
		}
	}
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

// HandleKey drives form editing.
func (f *Form) HandleKey(k keyMsg) (tea.Cmd, bool) {
	// A value change may have hidden the field the cursor is on (choosing a new kind in the step editor
	// hides the previous kind's fields), so normalise BEFORE reading the focused spec.
	f.normalizeCursor()
	s := f.current()
	// THE FIELD EDIT LOCK.
	if f.editing != "" {
		if s == nil || s.Name != f.editing {
			f.editing = "" // the cursor moved off the field (tab/next) — release
		} else {
			switch k.String() {
			case "esc":
				// Leave the FIELD, not the form. Esc again then cancels the edit, which
				// is the nested behaviour the operator expects from a locked surface.
				f.editing = ""
				return nil, true
			case "up":
				f.moveCaretLine(f.editing, -1)
				return nil, true
			case "down":
				f.moveCaretLine(f.editing, 1)
				return nil, true
			case "enter", "alt+enter":
				// This is the ONLY way to type one: Enter was the sole key that could and it is what locks the
				// field, so without this a text area could only ever hold a single line unless the text arrived.
				f.insertRunes(f.editing, "\n")
				return nil, true
			}
		}
	}
	// PREVIEW MODE (kPreview).
	if f.preview != "" {
		if k.String() == kPreview {
			f.preview = ""
			return nil, true
		}
		// Esc CLOSES THE PREVIEW rather than cancelling the whole edit.
		if k.String() == "esc" {
			f.preview = ""
			return nil, true
		}
		f.preview = ""
	}
	// A model field is a REFERENCE, not text: enter/space opens the host's model picker instead of
	// advancing or editing (there is nothing a human could usefully type into it).
	if s != nil && s.Kind == KModel {
		switch k.String() {
		case "enter", " ", "space":
			if f.OnOpenModelPicker != nil {
				return f.OnOpenModelPicker(s.Name, f.Values[s.Name]), true
			}
			return nil, true
		}
	}
	// A date field is the same shape: enter/space opens the host's calendar.
	if s != nil && s.Kind == KDate {
		switch k.String() {
		case "enter", " ", "space":
			if f.OnOpenDatePicker != nil {
				return f.OnOpenDatePicker(s.Name, f.Values[s.Name]), true
			}
			return nil, true
		}
	}
	// A date-AND-TIME field is the same shape again, opening the host's combined calendar and clock.
	if s != nil && s.Kind == KDateTime {
		switch k.String() {
		case "enter", " ", "space":
			if f.OnOpenDateTimePicker != nil {
				return f.OnOpenDateTimePicker(s.Name, f.Values[s.Name]), true
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
	case "ctrl+e":
		// Expand / collapse the focused field. Only free-text multi-line kinds
		// benefit: a one-line value cannot use the extra rows.
		if s == nil || !f.expandable(s.Kind) {
			return nil, true
		}
		if f.expanded == s.Name {
			f.expanded = ""
		} else {
			f.expanded = s.Name
			f.preview = "" // one rendering of the field at a time
		}
		return nil, true
	case kPreview:
		// Show the focused field's value as RENDERED markdown.
		if s == nil || !f.canPreviewName(*s) {
			return nil, true
		}
		f.preview = s.Name
		f.expanded = ""
		return nil, true
	case "shift+tab":
		f.Prev()
		return nil, true
	case "down":
		// Vertical keys walk the FIELDS.
		f.Next()
		return nil, true
	case "up":
		f.Prev()
		return nil, true
	case "ctrl+s":
		// Save.
		cmd, _ := f.Submit()
		return cmd, true
	case "enter":
		// Enter NEVER submits: on a form full of pickers and selectors it is the CHOOSE gesture, and
		// overloading it made selecting a value commit the whole form.
		if s != nil && f.expandable(s.Kind) {
			f.editing = s.Name
			return nil, true
		}
		// Enter advances to the next field, and is a
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
		// Clear the field.
		if s != nil && (f.editable(s.Kind) || f.reference(s.Kind)) {
			f.Values[s.Name] = ""
			f.setCaret(s.Name, 0)
			if f.OnChange != nil {
				f.OnChange(s.Name, "")
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
				f.toggleMultiAt(s, f.MultiCursor(s))
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
			case KPicker:
				// A PICKER MUST CONSUME LEFT/RIGHT, and cycling its options is the useful way to do it.
				f.cycleSelect(s, d)
				return nil, true
			case KMultiSelect:
				// LEFT/RIGHT walk the options INSIDE the field (up/down keep walking fields, matching KSelect).
				f.moveMultiCursor(s, d)
				return nil, true
			case KCheckbox:
				f.Values[s.Name] = toggleBool(f.Values[s.Name])
				return nil, true
			case KModel, KDate, KDateTime:
				// A REFERENCE field is CHOSEN, not moved through: enter opens the control that sets it (the model
				// picker, the calendar), so there is nothing for an arrow to do here.
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

// reference reports whether a field is CHOSEN from a control rather than typed into — the family
// whose `enter` OPENS something instead of advancing.
func (f *Form) reference(k Kind) bool {
	switch k {
	case KModel, KDate, KDateTime, KPicker:
		return true
	}
	return false
}

// referenceControlName names the control an `enter` opens, in prose, for the footer.
func (f *Form) referenceControlName(s FieldSpec) string {
	switch s.Kind {
	case KModel:
		return "model picker"
	case KDate:
		return "calendar"
	case KDateTime:
		return "date & time picker"
	case KPicker:
		return "list"
	}
	return "control"
}

// referenceGesture is the affordance a reference field's ROW carries, with the current value folded
// in, so every kind advertises itself the same way.
func (f *Form) referenceGesture(s FieldSpec) string {
	unset := f.Values[s.Name] == ""
	switch s.Kind {
	case KModel:
		if unset {
			return "enter to choose a model"
		}
		return "enter: change"
	case KDate:
		if unset {
			return "enter to pick a date"
		}
		return "enter: change"
	case KDateTime:
		if unset {
			return "enter to pick a date & time"
		}
		return "enter: change"
	case KPicker:
		return "enter: choose"
	}
	return ""
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

// MultiCursor is the option index the operator is on inside a multi-select.
func (f *Form) MultiCursor(s *FieldSpec) int {
	if len(s.Options) == 0 {
		return 0
	}
	i := f.multiCur[s.Name]
	if i < 0 {
		i = 0
	}
	if i >= len(s.Options) {
		i = len(s.Options) - 1
	}
	return i
}

// moveMultiCursor walks the option cursor, wrapping at the ends: a weekday row is
// a ring the operator cycles, not a list they fall off.
func (f *Form) moveMultiCursor(s *FieldSpec, d int) {
	if len(s.Options) == 0 {
		return
	}
	if f.multiCur == nil {
		f.multiCur = map[string]int{}
	}
	i := (f.MultiCursor(s) + d + len(s.Options)) % len(s.Options)
	f.multiCur[s.Name] = i
}

// toggleMultiAt flips ONE option — the one under the cursor, which is what makes the control
// usable: toggling a fixed option (the old behaviour) meant only the first could ever be selected.
func (f *Form) toggleMultiAt(s *FieldSpec, i int) {
	if len(s.Options) == 0 {
		return
	}
	if i < 0 || i >= len(s.Options) {
		return
	}
	if f.Multi[s.Name] == nil {
		f.Multi[s.Name] = map[string]bool{}
	}
	v := s.Options[i].Value
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
		// A HIDDEN field cannot be required: the operator cannot see or reach it, so failing the save on it
		// would be unfixable from the form.
		if !f.fieldVisible(s) {
			continue
		}
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
	// Iterate the SPEC's option order, never the map: a map has no order, so the returned slice
	// reshuffled on every call.
	for _, s := range f.Specs {
		if s.Name != name {
			continue
		}
		for _, o := range s.Options {
			if m[o.Value] {
				out = append(out, o.Value)
			}
		}
		// A value set programmatically for something the options do not enumerate
		// still counts, ordered so the result stays deterministic.
		var extras []string
		for k, on := range m {
			if !on {
				continue
			}
			known := false
			for _, o := range s.Options {
				if o.Value == k {
					known = true
					break
				}
			}
			if !known {
				extras = append(extras, k)
			}
		}
		sortStrings(extras)
		return append(out, extras...)
	}
	return out
}

// Submit validates and invokes OnSubmit with a copy of the collected values.
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
		// A rejected submit must SAY why.
		f.SubmitErr = err.Error()
		return cmd, err
	}
	f.SubmitErr = ""
	f.Submitted = true
	return cmd, nil
}

// VisibleFieldNames lists the fields the form would actually DRAW for its current values, in order.
func (f *Form) VisibleFieldNames() []string {
	out := make([]string, 0, len(f.Specs))
	for _, s := range f.Specs {
		if f.fieldVisible(s) {
			out = append(out, s.Name)
		}
	}
	return out
}

// NormalizeCursorForTest re-runs the cursor normalisation a value change triggers.
func (f *Form) NormalizeCursorForTest() { f.normalizeCursor() }

// FormCursorMark is the glyph View() prints at the start of the FOCUSED field's
// first row.
const FormCursorMark = "▸"

// kPreview is the chord that shows the focused field's value as RENDERED
// markdown. ctrl+p is otherwise unbound in the TUI (the form uses ctrl+e/s/u).
const kPreview = "ctrl+p"

// canPreviewName reports whether a field has a rendered view worth showing: a PROSE field whose
// value actually reads as markdown.
func (f *Form) canPreviewName(s FieldSpec) bool {
	return !s.NoPreview && s.Kind == KTextArea && md.LooksLikeMarkdown(f.Values[s.Name])
}

// FocusedRow returns the 0-based row of the focused field's FIRST line within a render produced by
// View() — the offset a host needs to scroll the form so the field being edited is on screen.
func (f *Form) FocusedRow(rendered string) int {
	for i, line := range strings.Split(rendered, "\n") {
		// The marker is the FIRST content on the row: the cursor prefix is written before the label.
		if strings.HasPrefix(strings.TrimLeft(ansi.Strip(line), " "), FormCursorMark) {
			return i
		}
	}
	return 0
}

// DisplayForTest renders a single field the way View would, so a test can assert that a
// stored id shows as its human label rather than as a bare id.
func (f *Form) DisplayForTest(name string) string {
	for _, s := range f.Specs {
		if s.Name == name {
			return f.display(s)
		}
	}
	return ""
}

// display renders a field's value for the view. A secret field NEVER returns its raw value — it
// returns a fixed mask.
func (f *Form) multiLine(s FieldSpec) string {
	cur := f.MultiCursor(&s)
	var b strings.Builder
	for i, o := range s.Options {
		lbl := o.Label
		if lbl == "" {
			lbl = o.Value
		}
		if len(lbl) > 2 {
			lbl = lbl[:2]
		}
		mark := "\u25a2" // ▢
		if f.Multi[s.Name][o.Value] {
			mark = "\u25a3" // ▣
		}
		cell := mark + lbl
		switch {
		case i == cur:
			b.WriteString(theme.ListItemSelected.Render(cell))
		case f.Multi[s.Name][o.Value]:
			b.WriteString(theme.StatusOK.Render(cell))
		default:
			b.WriteString(theme.HintText.Render(cell))
		}
		if i < len(s.Options)-1 {
			b.WriteString(" ")
		}
	}
	return b.String()
}

func (f *Form) display(s FieldSpec) string {
	v := f.Values[s.Name]
	// A field may override how its value READS without changing what is STORED (see FieldSpec.Display).
	// Checked first, so an override is total rather than a special case of one kind.
	if s.Display != nil {
		return s.Display(v)
	}
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
	case KModel, KDate, KDateTime:
		// A REFERENCE FIELD ALWAYS NAMES ITS GESTURE, whatever the cursor is doing.
		shown := v
		// Only while the form is FOCUSED at all: an unfocused form is background (the
		// list beside it owns the keyboard) and the value alone is the right amount.
		if !f.Focused {
			if shown == "" {
				if s.Placeholder != "" {
					return s.Placeholder
				}
				return "— none —"
			}
			return shown
		}
		if shown == "" {
			base := "— none —"
			if s.Placeholder != "" {
				base = s.Placeholder
			}
			if g := f.referenceGesture(s); g != "" {
				return base + " (" + g + ")"
			}
			return base
		}
		if g := f.referenceGesture(s); g != "" {
			return shown + "  " + g
		}
		return shown
	default:
		// AN EMPTY FIELD SHOWS ITS PLACEHOLDER, whoever is on it.
		if v == "" {
			return placeholderText(s.Placeholder)
		}
		return v
	}
}

// hintForPlaceholder renders a field's placeholder as ghost text followed by the caret, fitted to
// the cells available.
func hintForPlaceholder(placeholder string, avail int) string {
	const caretRune = "\u258f" // ▏
	cells := avail - 1         // one reserved for the caret
	if cells < 1 {
		return caretRune
	}
	text := ansi.Truncate(placeholderText(placeholder), cells, "…")
	// Pad to the caret so a shorter hint still ends with the caret at the value position,
	// rather than jittering left and right as the operator tabs between fields.
	if pad := cells - lipgloss.Width(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	return text + caretRune
}

// placeholderText renders a placeholder in the PARENTHESES IDIOM this form already uses for
// "nothing is set, and here is what that means".
func placeholderText(placeholder string) string {
	p := strings.TrimSpace(placeholder)
	if p == "" {
		return ""
	}
	// Do not double up when a placeholder is already written as a parenthetical.
	if strings.HasPrefix(p, "(") && strings.HasSuffix(p, ")") {
		return p
	}
	return "(" + p + ")"
}

// valueWithCaret renders an editable field's value with a caret at the edit position, windowed to
// at most `avail` cells so the caret is ALWAYS visible.
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
	// Keep the cursor on a visible field even if a value changed outside HandleKey (a
	// programmatic Set, a test): rendering must never highlight a row it does not draw.
	f.normalizeCursor()
	// A form rendered without an explicit width still needs a sane one: Pad truncates to it, so width 0
	// erased every line (the Work screen's forms never set it) and windowing needs it to keep.
	width := f.Width
	if width <= 0 {
		width = 64
	}
	var b strings.Builder
	if f.Title != "" {
		b.WriteString(theme.ListTitle.Render(f.Title))
		b.WriteString("\n")
	}
	if f.Note != "" {
		// Padded like a field row and wrapped to the form's width, so the note cannot
		// shift the fields below it or bleed past the panel.
		for _, line := range wrapHint(theme.HintText.Render(f.Note), width) {
			b.WriteString(theme.HintText.Render(Pad(line, width)))
			b.WriteString("\n")
		}
	}
	for i, s := range f.Specs {
		if !f.fieldVisible(s) {
			// A field that does not apply to the current selection is not drawn at all: the step editor reveals
			// a kind's own fields.
			continue
		}
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
		case focused && f.preview == s.Name:
			// THE FIELD AS RENDERED MARKDOWN — a READ-ONLY view of the value being edited.
			budget := f.focusedWrapRows() - 2 // the label row and the hint row are ours too
			if budget < 3 {
				budget = 3
			}
			lines := md.RenderOn(f.Values[s.Name], max(8, width-2), md.SurfaceTokens(theme.Text, theme.Bg))
			hidden := 0
			if len(lines) > budget {
				hidden = len(lines) - budget
				lines = lines[:budget]
			}
			b.WriteString(theme.ListItemSelected.Render(Pad(cursor+label+":", width)) + "\n")
			for _, l := range lines {
				b.WriteString(Pad("  "+l, width) + "\n")
			}
			note := ""
			if len(lines) == 0 {
				note = " (the value is empty)"
			} else if hidden > 0 {
				note = fmt.Sprintf(" (+%d more rendered lines)", hidden)
			}
			b.WriteString(theme.HintText.Render(Pad(
				"  "+label+": PREVIEW (rendered) — ctrl+p returns to the raw text"+note, width)) + "\n")
			continue
		case focused && f.expanded == s.Name && f.expandable(s.Kind):
			// EXPANDED (ctrl+e): the value wraps across as many rows as it needs, so the operator reads the
			// whole thing instead of one windowed slice.
			for _, l := range f.expandedBody(s.Name, width-lipgloss.Width(prefix)) {
				line = prefix + l
				b.WriteString(theme.ListItemSelected.Render(Pad(line, width)) + "\n")
				prefix = strings.Repeat(" ", lipgloss.Width(prefix))
			}
			b.WriteString(theme.HintText.Render(Pad(strings.Repeat(" ", 2)+label+": (expanded — ctrl+e to collapse)", width)) + "\n")
			continue
		case focused && f.expandable(s.Kind):
			// A FOCUSED multi-line field is WRAPPED by DEFAULT: the operator edits prose, a prompt or a JSON
			// blob and must be able to read it as they write, without knowing a chord.
			valueIndent := strings.Repeat(" ", 2)
			rows, hiddenAbove, hiddenBelow := f.wrappedBody(s.Name, max(8, width-lipgloss.Width(valueIndent)), f.focusedWrapRows())
			// The label row, so the field is still named while it is being edited — carrying the CURSOR MARKER
			// (FormCursorMark), which FocusedRow scans for to scroll this pane to the field being edited.
			labelRow := cursor + label + ":"
			if f.editing == s.Name {
				labelRow = cursor + label + ": EDITING"
			}
			b.WriteString(theme.ListItemSelected.Render(Pad(labelRow, width)) + "\n")
			for _, l := range rows {
				b.WriteString(theme.ListItemSelected.Render(Pad(valueIndent+l, width)) + "\n")
			}
			// The hint NAMES THE CHORDS AVAILABLE ON THIS FIELD, and it is kept short on purpose: the host PADS
			// (truncates) it to the pane width.
			var hint string
			switch {
			case hiddenAbove > 0 && hiddenBelow > 0:
				hint = fmt.Sprintf("  (+%d above · +%d below)", hiddenAbove, hiddenBelow)
			case hiddenAbove > 0:
				hint = fmt.Sprintf("  (+%d above)", hiddenAbove)
			case hiddenBelow > 0:
				hint = fmt.Sprintf("  (+%d below)", hiddenBelow)
			default:
				hint = fmt.Sprintf("  (%d lines)", lineCount(f.Values[s.Name]))
			}
			if f.editing == s.Name {
				hint += " · ↑/↓: move · enter: newline · esc: leave field"
			} else {
				hint += " · enter: edit · ctrl+e: expand"
			}
			// A multi-line field's hint sits BELOW its value, so on a tall field it is
			// pushed off the pane. The footer carries a short form of it too.
			if f.canPreviewName(s) {
				hint += " · ctrl+p: preview"
			}
			b.WriteString(theme.HintText.Render(Pad(strings.Repeat(" ", 2)+label+":"+hint, width)) + "\n")
			continue
		case focused && s.Kind == KMultiSelect:
			// While focused the options are SHOWN, each marked selected or not, with the
			// cursor on one — the operator has to see what they are toggling.
			line = prefix + f.multiLine(s)
		case focused && s.Kind == KPicker:
			// While the list is open the field shows the QUERY being typed;
			// closed it shows the choice that was made.
			if f.pickerField == s.Name {
				line = prefix + f.pickerQuery + "\u258f"
			} else {
				line = prefix + f.display(s)
			}
		case focused && f.editable(s.Kind) && width > 0:
			// An editable field shows a CARET and horizontally windows its value, so the character being edited
			// is always on screen.
			avail := width - lipgloss.Width(prefix)
			if avail < 8 {
				avail = 8
			}
			// AN EMPTY FIELD SHOWS ITS PLACEHOLDER, then the caret.
			if f.Values[s.Name] == "" && s.Placeholder != "" {
				// THE HINT FITS THE ACTUAL SPACE, without the 8-cell floor above.
				hintAvail := width - lipgloss.Width(prefix)
				if hintAvail < 1 {
					hintAvail = 1
				}
				line = prefix + hintForPlaceholder(s.Placeholder, hintAvail)
			} else {
				line = prefix + f.valueWithCaret(s.Name, avail)
			}
		default:
			val := f.display(s)
			// A PLACEHOLDER ON AN UNFOCUSED ROW IS FITTED TOO, exactly as it is on the cursor's row.
			if s.Placeholder != "" && f.editable(s.Kind) && f.Values[s.Name] == "" {
				if avail := width - lipgloss.Width(prefix); avail > 0 {
					// NO CARET: this row does not hold the cursor. The hint still ends where the
					// operator's typing will appear, so the form reads consistently field to field.
					val = ansi.Truncate(val, avail, "…")
				}
			}
			// A multi-line value on an UNFOCUSED row must not emit its newlines.
			if f.expandable(s.Kind) {
				if n := lineCount(f.Values[s.Name]); n > 1 {
					val = oneLine(val)
					line = prefix + val + theme.HintText.Render(fmt.Sprintf("  (%d lines)", n))
					break
				}
			}
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
			if s.Kind == KDate && focused {
				// Same affordance as KModel: the field names the control that sets it.
				hint := "  enter: pick a date"
				if f.Values[s.Name] != "" {
					hint = "  enter: change date"
				}
				line += theme.HintText.Render(hint)
			}
			if s.Kind == KDateTime && focused {
				// The combined control, named the same way.
				hint := "  enter: pick a date & time"
				if f.Values[s.Name] != "" {
					hint = "  enter: change date & time"
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
		// An open picker lists its matches directly under the field, so the operator SEES the choices while
		// typing (the operator's "another box comes up similar to the slash command").
		if s.Kind == KPicker && f.pickerField == s.Name {
			f.writePickerList(&b, &f.Specs[i], width)
		}
	}
	// The hint row wraps at its " · " boundaries so a KEY and its meaning stay together.
	footer := "↑/↓ or tab: field · ←/→: move · ctrl+u: clear · enter: next · ctrl+s: save · esc: cancel"
	if s := f.current(); f.editing != "" {
		footer = "editing text · ↑/↓: move · enter: newline · esc: leave field · ctrl+s: save"
	} else if f.preview != "" {
		footer = "markdown preview · ctrl+p or esc: back to the raw text · ctrl+s: save"
	} else {
		switch {
		case s != nil && f.canPreviewName(*s):
			footer = "↑/↓ or tab: field · ←/→: move · enter: edit · ctrl+p: preview markdown · ctrl+s: save · esc: cancel"
		case s != nil && f.expandable(s.Kind):
			footer = "↑/↓ or tab: field · ←/→: move · enter: edit · ctrl+e: expand · ctrl+s: save · esc: cancel"
		case s != nil && f.reference(s.Kind):
			// A REFERENCE FIELD IS OPENED, NOT ADVANCED — so "enter: next" is simply WRONG here, and it was the
			// line the operator was reading while hunting for the model.
			footer = "↑/↓ or tab: field · ←/→: move · ctrl+u: clear · enter: open the " + f.referenceControlName(*s) + " · ctrl+s: save · esc: cancel"
		}
	}
	for _, l := range wrapHint(theme.HintText.Render(footer), width) {
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

// WrapHint is the exported form of the segment-aware hint wrap, so the composer (a different
// package) wraps its affordance row the same way instead of truncating it.
func WrapHint(line string, width int) []string { return wrapHint(line, width) }

// expandable reports whether a field kind benefits from the expanded editor: free-text multi-line
// values, which are otherwise edited one horizontally windowed line at a time.
func (f *Form) expandable(k Kind) bool {
	switch k {
	case KTextArea, KJSON, KYAML:
		return true
	}
	return false
}

// Expanded reports the field currently rendered wide ("" = none).
func (f *Form) Expanded() string { return f.expanded }

// maxWrappedRows bounds the DEFAULT wrapped view of a multi-line field.
const maxWrappedRows = 6

// focusedWrapRows is the row budget for the FOCUSED multi-line field.
func (f *Form) focusedWrapRows() int {
	if f.Height <= 0 {
		return maxWrappedRows
	}
	avail := f.Height - 10
	if avail < maxWrappedRows {
		return maxWrappedRows
	}
	return avail
}

// oneLine flattens a value for SINGLE-ROW display, marking the line breaks instead of emitting
// them.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	if !strings.Contains(s, "\n") {
		return s
	}
	parts := strings.Split(s, "\n")
	// Trailing blank lines carry no information in a summary.
	for len(parts) > 1 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	trimmed := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed = append(trimmed, strings.TrimRight(p, " "))
	}
	return strings.Join(trimmed, " \u21b5 ") // ↵ between logical lines
}

// lineCount reports how many logical lines a value has (0 or 1 = single-line).
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.ReplaceAll(s, "\r\n", "\n"), "\n") + 1
}

// wrappedBody renders a multi-line value the way it should be READ: the value's OWN line breaks
// preserved, long lines soft-wrapped to `width`, and a caret at the edit position.
func (f *Form) wrappedBody(name string, width, maxRows int) (rows []string, hiddenAbove, hiddenBelow int) {
	val := f.Values[name]
	pos := f.caret(name)
	runes := []rune(val)
	if pos > len(runes) {
		pos = len(runes)
	}
	const caretRune = "\u258f" // ▏
	withCaret := string(runes[:pos]) + caretRune + string(runes[pos:])

	withCaret = strings.ReplaceAll(withCaret, "\r\n", "\n")
	withCaret = strings.ReplaceAll(withCaret, "\r", "\n")
	withCaret = strings.ReplaceAll(withCaret, "\t", "    ")
	caretRow := -1
	for _, logical := range strings.Split(withCaret, "\n") {
		for _, w := range wrapPreservingSpaces(logical, width) {
			if caretRow < 0 && strings.Contains(w, caretRune) {
				caretRow = len(rows)
			}
			rows = append(rows, w)
		}
	}
	if len(rows) == 0 {
		rows = []string{""}
		caretRow = 0
	}
	if maxRows <= 0 || len(rows) <= maxRows {
		return rows, 0, 0
	}
	// Put the caret in the middle when there is room, then clamp so the window
	// never runs past either end of the value.
	start := caretRow - maxRows/2
	if start < 0 {
		start = 0
	}
	if start > len(rows)-maxRows {
		start = len(rows) - maxRows
	}
	return rows[start : start+maxRows], start, len(rows) - (start + maxRows)
}

// wrapPreservingSpaces soft-wraps ONE logical line to `width` cells, leaving the line untouched
// when it already fits.
func wrapPreservingSpaces(line string, width int) []string {
	if width < 8 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	var out []string
	rest := line
	for lipgloss.Width(rest) > width {
		// Prefer the last space that fits — a word boundary.
		cut, w := -1, 0
		for i, r := range rest {
			rw := lipgloss.Width(string(r))
			if w+rw > width {
				break
			}
			w += rw
			if r == ' ' {
				cut = i
			}
		}
		if cut <= 0 {
			// One token longer than the row (a minified JSON blob, a long URL):
			// hard-split at the boundary rather than emit an over-wide row.
			cut, w = 0, 0
			for i, r := range rest {
				rw := lipgloss.Width(string(r))
				if w+rw > width {
					cut = i
					break
				}
				w += rw
			}
			if cut <= 0 {
				break
			}
		}
		out = append(out, rest[:cut])
		rest = strings.TrimPrefix(rest[cut:], " ")
	}
	out = append(out, rest)
	return out
}

// expandedBody renders the focused field's value WRAPPED across as many rows as it needs, with a
// caret at the edit position.
func (f *Form) expandedBody(name string, width int) []string {
	val := f.Values[name]
	pos := f.caret(name)
	runes := []rune(val)
	if pos > len(runes) {
		pos = len(runes)
	}
	const caretRune = "\u258f" // ▏
	withCaret := string(runes[:pos]) + caretRune + string(runes[pos:])
	return wrapFormLine(withCaret, width)
}

// wrapFormLine breaks a rendered line so it fits `width` cells, wrapping at word boundaries and
// indenting continuations to match the leading prefix.
func wrapFormLine(line string, width int) []string {
	if width < 8 || lipgloss.Width(line) <= width {
		return []string{line}
	}
	// THE ROW'S OWN LEADING WHITESPACE IS PART OF THE ROW.
	lead := line[:len(line)-len(strings.TrimLeft(line, " "))]
	// The continuation indent mirrors the field's leading marker + label so a wrapped value reads as
	// part of the same field.
	body := strings.TrimLeft(line, " ")
	indent := lead + "    "
	if i := strings.Index(body, ": "); i >= 0 && i < width/2 {
		indent = lead + strings.Repeat(" ", i+2)
	}
	words := strings.Fields(line)
	if len(words) == 0 {
		return []string{line}
	}
	out := make([]string, 0, 2)
	// The FIRST line re-attaches the row's own indent, so a wrapped row lands in the
	// same column as an unwrapped one.
	cur := lead + words[0]
	for _, w := range words[1:] {
		if lipgloss.Width(cur)+1+lipgloss.Width(w) <= width {
			cur += " " + w
			continue
		}
		out = append(out, cur)
		cur = indent + w
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// sortStrings orders a small slice deterministically (extras in a multi-select).
func sortStrings(v []string) { sort.Strings(v) }
