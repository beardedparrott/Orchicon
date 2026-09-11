// overlay.go — the enforcement screen's modal surfaces: Confirm, Form
// (multi-line fields for Rego bodies / reject reasons), Picker (policy
// versions) and Viewer (a version's Rego body).
//
// Every overlay CLAIMS the keyboard (Model.ClaimsKeys) while it is open:
// the shell hands it every key verbatim, so a Rego body containing q/d/y
// — or a reason string with a "?" — is never eaten by a global chord
// (quit, diff toggle, copy, help). esc always cancels; a form's Enter
// advances fields (a multi-line field inserts a newline) and ctrl+s
// submits.
package enforcement

import (
	"strings"

	"github.com/beardedparrott/orchicon/internal/tui/theme"
)

// ovKind distinguishes the modal surfaces.
type ovKind int

const (
	ovNone ovKind = iota
	ovConfirm
	ovForm
	ovPicker
	ovViewer
)

// ovField is one editable form field. multi = Enter inserts a newline
// (Rego body / long context) and ctrl+s submits.
type ovField struct {
	Key   string
	Label string
	Multi bool
	Value string
}

// ovItem is one selectable row (the policy-version picker).
type ovItem struct {
	Label string
	ID    string
	Meta  string
}

// overlay is one modal surface plus its in-progress input state.
type overlay struct {
	kind   ovKind
	title  string
	hint   string
	body   string
	items  []ovItem
	cursor int
	fields []*ovField
	active int
	action string
	err    string
	scroll int
}

// value returns a form field's current text ("" when absent).
func (o *overlay) value(key string) string {
	if o == nil {
		return ""
	}
	for _, f := range o.fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// setValue writes a form field's text (no-op when absent).
func (o *overlay) setValue(key, v string) {
	for _, f := range o.fields {
		if f.Key == key {
			f.Value = v
			return
		}
	}
}

// newForm builds a form overlay.
func newForm(title, hint, action string, fields ...*ovField) *overlay {
	return &overlay{kind: ovForm, title: title, hint: hint, action: action, fields: fields}
}

// newConfirm builds a Confirm overlay (the guard before every recovery
// action).
func newConfirm(title, body, action string) *overlay {
	return &overlay{kind: ovConfirm, title: title, body: body, action: action}
}

// newPicker builds a selectable list overlay.
func newPicker(title, hint, action string, items []ovItem) *overlay {
	return &overlay{kind: ovPicker, title: title, hint: hint, action: action, items: items}
}

// newViewer builds a read-only body overlay.
func newViewer(title, hint, body string) *overlay {
	return &overlay{kind: ovViewer, title: title, hint: hint, body: body}
}

// view renders the overlay block. The screen frames the result, so this
// only needs to be readable and ANSI-safe (no silent truncation: long
// Rego lines are shown whole; the frame clips the pane, not the value).
func (o *overlay) view() string {
	var b strings.Builder
	b.WriteString(theme.ListTitle.Render(o.title) + "\n")
	if o.hint != "" {
		b.WriteString(theme.HintText.Render(o.hint) + "\n")
	}
	b.WriteString("\n")
	switch o.kind {
	case ovConfirm:
		for _, line := range strings.Split(o.body, "\n") {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
		b.WriteString(theme.HintText.Render("y / enter: confirm   ·   n / esc: cancel") + "\n")
	case ovForm:
		for i, f := range o.fields {
			marker := "  "
			if i == o.active {
				marker = theme.ListItemSelected.Render(">") + " "
			}
			label := f.Label
			if f.Multi {
				label += " (multiline — ctrl+s submits)"
			}
			b.WriteString(marker + theme.DetailKey.Render(label) + "\n")
			val := f.Value
			if val == "" {
				val = "—"
			}
			for _, line := range strings.Split(val, "\n") {
				if strings.TrimSpace(line) == "" && strings.Count(val, "\n") > 0 {
					b.WriteString("      \n")
					continue
				}
				b.WriteString("    " + theme.DetailValue.Render(line) + "\n")
			}
		}
		b.WriteString("\n")
		b.WriteString(theme.HintText.Render("enter: next field (multiline: newline) · tab: next · ctrl+s: submit · esc: cancel") + "\n")
	case ovPicker:
		for i, it := range o.items {
			row := "  " + it.Label
			if it.Meta != "" {
				row += "  " + it.Meta
			}
			if i == o.cursor {
				b.WriteString(theme.ListItemSelected.Render(row) + "\n")
			} else {
				b.WriteString(theme.ListItem.Render(row) + "\n")
			}
		}
		if len(o.items) == 0 {
			b.WriteString(theme.HintText.Render("  (none)") + "\n")
		}
		b.WriteString("\n")
		b.WriteString(theme.HintText.Render("↑/↓: move · enter: inspect · esc: close") + "\n")
	case ovViewer:
		lines := strings.Split(strings.TrimRight(o.body, "\n"), "\n")
		if o.scroll > len(lines) {
			o.scroll = 0
		}
		for _, line := range lines[o.scroll:] {
			b.WriteString("  " + line + "\n")
		}
		b.WriteString("\n")
		b.WriteString(theme.HintText.Render("↑/↓: scroll · esc / enter: close") + "\n")
	}
	if o.err != "" {
		b.WriteString("\n" + theme.ErrorText.Render("⚠ "+o.err) + "\n")
	}
	return b.String()
}
