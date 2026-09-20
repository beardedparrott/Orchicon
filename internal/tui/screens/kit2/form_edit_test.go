package kit2

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func pressKey(f *Form, k tea.KeyMsg) { f.HandleKey(k) }

func keyRune(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// The operator: "Editing a work item pops up a modal and I can move through the
// items but I can't actually edit anything." The form appended every keystroke
// to the END of the prefilled value with no cursor and no clear, so an existing
// value could not be corrected — only extended.
func TestFormCaretEditing(t *testing.T) {
	f := NewForm("Edit",
		FieldSpec{Name: "title", Label: "Title", Kind: KText, Initial: "Original"},
	)
	f.Focused = true
	f.FocusName("title")

	// A fresh field starts with the caret at the end (typing extends).
	if got := f.caret("title"); got != len("Original") {
		t.Fatalf("caret starts at %d, want end (%d)", got, len("Original"))
	}
	pressKey(f, keyRune("!"))
	if f.Values["title"] != "Original!" {
		t.Fatalf("typing at the end = %q", f.Values["title"])
	}

	// Left moves the caret back; typing then INSERTs mid-string.
	for i := 0; i < 7; i++ {
		pressKey(f, tea.KeyMsg{Type: tea.KeyLeft})
	}
	if got := f.caret("title"); got != 2 {
		t.Fatalf("caret after 7 lefts = %d, want 2", got)
	}
	pressKey(f, keyRune("X"))
	if f.Values["title"] != "OrXiginal!" {
		t.Fatalf("mid-string insert = %q, want OrXiginal!", f.Values["title"])
	}

	// home / end.
	pressKey(f, tea.KeyMsg{Type: tea.KeyHome})
	if got := f.caret("title"); got != 0 {
		t.Fatalf("home -> %d", got)
	}
	pressKey(f, keyRune(">"))
	if f.Values["title"] != ">OrXiginal!" {
		t.Fatalf("insert at home = %q", f.Values["title"])
	}
	pressKey(f, tea.KeyMsg{Type: tea.KeyEnd})
	if got := f.caret("title"); got != len([]rune(f.Values["title"])) {
		t.Fatalf("end -> %d", got)
	}

	// backspace deletes BEFORE the caret; delete deletes AT it.
	pressKey(f, tea.KeyMsg{Type: tea.KeyBackspace})
	want := ">OrXiginal"
	if f.Values["title"] != want {
		t.Fatalf("backspace = %q, want %q", f.Values["title"], want)
	}
	pressKey(f, tea.KeyMsg{Type: tea.KeyHome})
	pressKey(f, tea.KeyMsg{Type: tea.KeyDelete})
	if f.Values["title"] != "OrXiginal" {
		t.Fatalf("delete at caret = %q, want OrXiginal", f.Values["title"])
	}

	// ctrl+u clears the field entirely — the gesture that was missing.
	pressKey(f, tea.KeyMsg{Type: tea.KeyCtrlU})
	if f.Values["title"] != "" {
		t.Fatalf("ctrl+u must clear the field, got %q", f.Values["title"])
	}
	if got := f.caret("title"); got != 0 {
		t.Fatalf("caret after clear = %d, want 0", got)
	}
}

// Left/Right must still cycle a SELECT value (a horizontal gesture on a value),
// and must not be hijacked by the text-field caret path.
func TestFormLeftRightStillCyclesSelects(t *testing.T) {
	f := NewForm("New",
		FieldSpec{Name: "kind", Label: "Kind", Kind: KSelect, Options: []Option{
			{Value: "task", Label: "task"}, {Value: "epic", Label: "epic"},
		}},
	)
	f.Focused = true
	f.FocusName("kind")
	before := f.Values["kind"]
	pressKey(f, tea.KeyMsg{Type: tea.KeyRight})
	if f.Values["kind"] == before {
		t.Fatal("right must cycle a select's value")
	}
}

// The caret must stay ON SCREEN: with a value far wider than the field, the
// rendered line still shows the caret (and the line stays within the form
// width). Before this the caret position did not exist at all and every
// keystroke landed off the visible width.
func TestFormCaretStaysVisibleOnALongValue(t *testing.T) {
	long := strings.Repeat("abcdefghij", 12) // 120 chars
	f := NewForm("Edit", FieldSpec{Name: "title", Label: "Title", Kind: KText, Initial: long})
	f.Width = 40
	f.Focused = true
	f.FocusName("title")

	// The caret starts at the end — the far side of the value.
	fieldLine := func() string {
		for _, l := range strings.Split(ansi.Strip(f.View()), "\n") {
			if strings.Contains(l, "Title") {
				return l
			}
		}
		return ""
	}
	title := fieldLine()
	if title == "" {
		t.Fatalf("the title field line must render:\n%s", ansi.Strip(f.View()))
	}
	if !strings.Contains(title, "\u258f") {
		t.Fatalf("the focused field must render a caret: %q", title)
	}
	if w := len([]rune(title)); w > f.Width {
		t.Fatalf("field line is %d cells, wider than the form (%d): %q", w, f.Width, title)
	}
	// The tail of the value is what is shown, because the caret is at the end.
	if !strings.Contains(title, "…") {
		t.Fatalf("a clipped value must be marked with an ellipsis: %q", title)
	}
	// Moving home brings the HEAD of the value into view instead.
	pressKey(f, tea.KeyMsg{Type: tea.KeyHome})
	head := fieldLine()
	if !strings.HasPrefix(strings.TrimPrefix(head, "▸ Title: "), "\u258f") {
		t.Fatalf("home must put the caret at the start of the window: %q", head)
	}
}
