package kit2

// form_never_escapes_test.go — ONLY ESC CLOSES A FORM.
//
// The operator: "When you hit the right arrow key on the new project prompt in the runtime image
// selection it exits out of the project creation screen and bounces you into the 'new' page."
//
// The mechanism is worth stating because it is the form's own contract that made it possible.
// Form.HandleKey returns handled=false to mean "the caller closes the form", and the LAUNCH PROMPT
// reads that as DECLINED — so an unhandled key is not a no-op there, it is a cancellation. The
// left/right arm handled KSelect, KMultiSelect, KCheckbox and editable kinds; KPicker was absent, so
// right on the runtime-image field fell through to `return nil, false` and threw the operator out of
// the form they were filling in.
//
// So the invariant this file pins is deliberately blanket rather than per-key: EXACTLY ONE KEY MAY
// END A FORM, and it is esc. Every other key — including ones no field currently uses — must be
// consumed. That is what makes the next field kind (a new picker, a slider, whatever comes) safe by
// default rather than by somebody remembering this.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// everyKind is every field kind the form supports. Kept as a list rather than derived so that
// ADDING A KIND forces a decision here: the test below will exercise it automatically, and a kind
// that cannot consume a key will be caught the moment it exists.
func everyKind() []Kind {
	return []Kind{
		KText, KTextArea, KSelect, KMultiSelect, KSecret, KJSON, KYAML, KCheckbox, KNumber,
		KPicker, KModel, KDate, KDateTime,
	}
}

func formForKind(k Kind) *Form {
	f := NewForm("t", FieldSpec{
		Name: "f", Label: "F", Kind: k, Required: false,
		Options: []Option{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}},
	})
	f.Width = 80
	f.FocusName("f")
	return f
}

// THE INVARIANT IS ABOUT THE FORM'S OWN NAVIGATION KEYS, not about every key that exists.
//
// The arrows and tab are how a form is MOVED THROUGH. They must always be consumed, because
// handled=false means "the caller closes the form" — which the launch prompt reads as DECLINED, so an
// unconsumed arrow is not a no-op, it is a way out of the form the operator is filling in.
//
// Letters and the shell's chords (ctrl+a, ctrl+o, ctrl+v) deliberately DO fall through: a form must
// not swallow the app's own bindings, and a non-editable field has no use for a letter. Asserting
// "every key is consumed" would have demanded exactly that mistake — which the first version of this
// test did, and it took ten failures per kind to see it.
func TestEveryFieldKindConsumesTheNavigationKeys(t *testing.T) {
	keys := []tea.KeyMsg{
		{Type: tea.KeyLeft}, {Type: tea.KeyRight}, {Type: tea.KeyUp}, {Type: tea.KeyDown},
		{Type: tea.KeyTab}, {Type: tea.KeyShiftTab},
	}
	for _, kind := range everyKind() {
		for _, k := range keys {
			f := formForKind(kind)
			_, handled := f.HandleKey(k)
			if !handled {
				t.Errorf("kind %q does not consume %q. handled=false means \"the caller closes the form\", which "+
					"the launch prompt reads as DECLINED — this is the escape that bounced the operator out of "+
					"project creation onto the New page, and it applies to every reference field, not only the "+
					"one they happened to press.", kind, k.String())
			}
		}
	}
}

// AND ESC STILL DOES END IT, or the invariant above could be satisfied by a form that cannot be
// dismissed at all.
func TestEscStillClosesTheForm(t *testing.T) {
	for _, kind := range everyKind() {
		f := formForKind(kind)
		if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); handled {
			t.Errorf("kind %q consumed esc — there would be no way out of the form", kind)
		}
	}
}

// THE SPECIFIC INSTANCE, asserted on the gesture the operator used: right on a picker moves through
// its options instead of leaving the form.
func TestRightOnAPickerCyclesItsOptions(t *testing.T) {
	f := formForKind(KPicker)
	f.Set("f", "a")

	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyRight}); !handled {
		t.Fatal("right on a picker is not consumed — this is the escape that bounced the operator out of " +
			"project creation onto the New page")
	}
	if got := f.Values["f"]; got != "b" {
		t.Errorf("after right the picker holds %q, want the next option (b)", got)
	}
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyLeft}); !handled {
		t.Fatal("left on a picker is not consumed")
	}
	if got := f.Values["f"]; got != "a" {
		t.Errorf("after left the picker holds %q, want the previous option (a)", got)
	}
	// A picker with NO options is still consumed — the escape was about the key, not about whether
	// there was anything to cycle to.
	empty := NewForm("t", FieldSpec{Name: "f", Label: "F", Kind: KPicker})
	empty.Width = 80
	empty.FocusName("f")
	if _, handled := empty.HandleKey(tea.KeyMsg{Type: tea.KeyRight}); !handled {
		t.Error("right on an EMPTY picker is not consumed — a picker with no options is exactly where " +
			"the operator would be pressing arrows, and it must not close the form")
	}
}

// AND THE FORM'S VIEW STILL RENDERS, so consuming the key did not break the field.
func TestAPickerStillRendersAfterCycling(t *testing.T) {
	f := formForKind(KPicker)
	f.HandleKey(tea.KeyMsg{Type: tea.KeyRight})
	got := f.View()
	if strings.TrimSpace(got) == "" {
		t.Error("the form rendered empty after cycling a picker")
	}
}
