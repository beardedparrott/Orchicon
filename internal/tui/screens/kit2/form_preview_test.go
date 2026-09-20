package kit2

// form_preview_test.go — the RENDERED markdown view inside the editor (ctrl+p).
//
// The operator: "in edit mode you could see the markdown. Like maybe a markdown
// switcher to view what it looks like but raw would be the only thing ever used."
//
// So the contract these pin is deliberately narrow: preview changes what the
// field DRAWS and nothing else. Values, the caret, submission and every edit stay
// on the raw text. A preview that could alter what gets saved would be a data-loss
// bug wearing a convenience's clothes.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/md"
)

const previewMarkdown = "# Heading\n\nSome **bold** text and a `code` span.\n\n- one\n- two\n"

func previewForm(value string) *Form {
	f := NewForm("Edit worker",
		FieldSpec{Name: "role", Label: "Role", Kind: KTextArea, Initial: value},
		FieldSpec{Name: "tag", Label: "Tag", Kind: KText, Initial: "t"},
	)
	f.Focused, f.Width, f.Height = true, 70, 30
	f.FocusName("role")
	return f
}

func ctrlP() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlP} }

// THE PREVIEW RENDERS THE MARKDOWN, and the raw value is untouched.
//
// The assertion is the MARKERS: raw shows them, rendered consumes them. That is
// also exactly why the gate exists — rendering is lossy by design, so it must
// never be the path a value is saved through.
func TestPreviewRendersMarkdownAndLeavesTheValueAlone(t *testing.T) {
	f := previewForm(previewMarkdown)

	raw := ansi.Strip(f.View())
	for _, marker := range []string{"# Heading", "**bold**", "`code`", "- one"} {
		if !strings.Contains(raw, marker) {
			t.Fatalf("the RAW view should show %q verbatim:\n%s", marker, raw)
		}
	}

	f.HandleKey(ctrlP())
	view := ansi.Strip(f.View())

	if !strings.Contains(view, "PREVIEW") {
		t.Fatalf("ctrl+p did not enter preview:\n%s", view)
	}
	// Rendered: the markers are gone and the text survives.
	for _, consumed := range []string{"**bold**", "`code`", "- one", "# Heading"} {
		if strings.Contains(view, consumed) {
			t.Errorf("preview still shows the raw marker %q — it is not rendering:\n%s", consumed, view)
		}
	}
	for _, survives := range []string{"Heading", "Some bold text and a code span."} {
		if !strings.Contains(view, survives) {
			t.Errorf("preview lost the text %q:\n%s", survives, view)
		}
	}

	// THE VALUE IS UNTOUCHED — this is the guarantee that makes preview safe.
	if got := f.Values["role"]; got != previewMarkdown {
		t.Fatalf("preview changed the value:\n got %q\nwant %q", got, previewMarkdown)
	}

	// And ctrl+p again returns the editable raw view.
	f.HandleKey(ctrlP())
	if back := ansi.Strip(f.View()); !strings.Contains(back, "**bold**") {
		t.Fatalf("ctrl+p did not return to the raw view:\n%s", back)
	}
}

// PREVIEW IS NOT A SAVE PATH: what OnSubmit receives is the raw text.
func TestPreviewSubmitsTheRawText(t *testing.T) {
	f := previewForm(previewMarkdown)
	var submitted string
	f.OnSubmit = func(v map[string]string, _ map[string][]string) (tea.Cmd, error) {
		submitted = v["role"]
		return nil, nil
	}

	f.HandleKey(ctrlP()) // preview on
	if _, err := f.Submit(); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitted != previewMarkdown {
		t.Fatalf("submit received %q, want the RAW text %q — the preview must never be what is written",
			submitted, previewMarkdown)
	}
}

// TYPING IN PREVIEW RESUMES EDITING WITHOUT LOSING THE KEYSTROKE.
//
// The alternative — swallowing editable keys until the operator remembers the
// toggle — makes the preview a mode they can get stuck in, and the first
// character they type after coming back would be lost.
func TestTypingInPreviewReturnsToRawEditing(t *testing.T) {
	f := previewForm("plain text")
	f.Set("role", previewMarkdown)
	f.HandleKey(ctrlP())
	if f.preview == "" {
		t.Fatal("fixture: preview did not open")
	}

	f.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'Z'}})

	if f.preview != "" {
		t.Error("typing did not leave preview, so the operator is stuck in a read-only view")
	}
	if got := f.Values["role"]; !strings.HasSuffix(got, "Z") {
		t.Errorf("the keystroke that left preview was SWALLOWED: value = %q", got)
	}
}

// THE CHORD IS INERT ON A FIELD WITH NOTHING TO PREVIEW.
//
// A Dockerfile, a shell script or a JSON blob: rendering CONSUMES markers, so
// previewing one would show the operator something other than what they typed.
// The chord does nothing and — just as important — the hint does not advertise it.
func TestPreviewIsInertOnNonProseFields(t *testing.T) {
	// A Dockerfile that WOULD trip the markdown heuristic: a leading "#" reads as a
	// heading. This is why FieldSpec.NoPreview exists rather than trusting the guess.
	const dockerfile = "# syntax=docker/dockerfile:1\nFROM golang:1.24\nRUN go build ./...\n"
	f := NewForm("Edit runtime image",
		FieldSpec{Name: "dockerfile_override", Label: "Dockerfile override", Kind: KTextArea,
			Initial: dockerfile, NoPreview: true},
	)
	f.Focused, f.Width, f.Height = true, 80, 30
	f.FocusName("dockerfile_override")

	// The heuristic alone would say yes; the declaration overrides it.
	if !md.LooksLikeMarkdown(dockerfile) {
		t.Fatal("fixture: this Dockerfile was supposed to trip the markdown heuristic, which is the " +
			"whole reason NoPreview is needed")
	}
	if f.canPreviewName(f.Specs[0]) {
		t.Error("a field declared NoPreview must never be previewable, even when its contents look " +
			"like markdown")
	}

	view := ansi.Strip(f.View())
	if strings.Contains(view, "ctrl+p") {
		t.Errorf("the hint advertises ctrl+p on a code field, inviting a rendering that would show "+
			"code as prose:\n%s", view)
	}

	f.HandleKey(ctrlP())
	if after := ansi.Strip(f.View()); strings.Contains(after, "PREVIEW") {
		t.Errorf("ctrl+p entered preview on a NoPreview field:\n%s", after)
	}
	if f.Values["dockerfile_override"] != dockerfile {
		t.Error("the field's value changed")
	}
}

// ctrl+e and ctrl+p are two renderings of one field, so they cannot both be on.
func TestPreviewAndExpandAreMutuallyExclusive(t *testing.T) {
	f := previewForm(previewMarkdown)

	f.HandleKey(ctrlP())
	if f.preview == "" {
		t.Fatal("fixture: preview did not open")
	}
	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE})
	if f.preview != "" {
		t.Error("ctrl+e left preview on, so the field is drawn twice over")
	}

	f.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlE}) // expand on
	f.HandleKey(ctrlP())
	if f.expanded != "" {
		t.Error("ctrl+p left expanded on, so the field is drawn twice over")
	}
}

// STRUCTURED FIELDS ARE NOT PROSE, even when their contents contain markdown
// characters.
//
// KJSON and KYAML are `expandable` — seeing all of a structured value is what
// ctrl+e is for — but rendering them as markdown would show mangled data: a JSON
// blob holding a `command` string ("`ls -la`") satisfies the markdown heuristic and
// would come back with its backticks consumed. Same failure as the Dockerfile, found
// by surveying every KTextArea/KJSON field rather than by waiting for a report.
func TestPreviewIsNotOfferedOnStructuredFields(t *testing.T) {
	const jsonWithMarkdownChars = `{"command":"` + "`ls -la`" + `","note":"**important**"}`

	if !md.LooksLikeMarkdown(jsonWithMarkdownChars) {
		t.Fatal("fixture: this JSON was supposed to trip the markdown heuristic, which is the whole " +
			"reason structured kinds need excluding")
	}
	f := NewForm("New runtime image",
		FieldSpec{Name: "env", Label: "Env", Kind: KJSON, Initial: jsonWithMarkdownChars},
	)
	f.Focused, f.Width, f.Height = true, 90, 30
	f.FocusName("env")

	if f.canPreviewName(f.Specs[0]) {
		t.Error("a KJSON field must never be previewed as markdown — its backticks and asterisks are " +
			"data, not markup")
	}
	view := ansi.Strip(f.View())
	if strings.Contains(view, "ctrl+p") {
		t.Errorf("the hint advertises ctrl+p on a JSON field:\n%s", view)
	}
	f.HandleKey(ctrlP())
	if after := ansi.Strip(f.View()); strings.Contains(after, "PREVIEW") {
		t.Errorf("ctrl+p entered preview on a JSON field:\n%s", after)
	}
	if f.Values["env"] != jsonWithMarkdownChars {
		t.Error("the field's value changed")
	}
}

// ESC CLOSES THE PREVIEW, NOT THE WHOLE EDIT.
//
// It used to clear the preview and then FALL THROUGH to the form's esc, which the
// host treats as "cancel the edit". So leaving a preview with Esc also threw away
// every unrelated field the operator had changed — a lot of damage for a key whose
// meaning inside a preview is "go back one step".
func TestEscInPreviewClosesThePreviewNotTheEdit(t *testing.T) {
	f := previewForm(previewMarkdown)
	// An unrelated edit elsewhere in the form, which must survive.
	f.Set("tag", "changed-by-the-operator")
	f.HandleKey(ctrlP())
	if f.preview == "" {
		t.Fatal("fixture: preview did not open")
	}

	handled := false
	if _, handled = f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); !handled {
		t.Fatal("Esc in preview must be consumed by the form to close the preview, not fall through " +
			"to the host, which would cancel the whole edit")
	}
	if f.preview != "" {
		t.Error("Esc did not close the preview")
	}
	if f.Submitted {
		t.Error("Esc closed the preview AND submitted/abandoned the form")
	}
	if got := f.Values["tag"]; got != "changed-by-the-operator" {
		t.Errorf("the unrelated edit was lost: tag = %q", got)
	}
	if got := f.Values["role"]; got != previewMarkdown {
		t.Errorf("the previewed field's value changed: %q", got)
	}

	// A second Esc is then the form's own cancel: not handled, so the host closes it.
	if _, handled := f.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); handled {
		t.Error("the second Esc must fall through to the host")
	}
}
