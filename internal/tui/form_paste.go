package tui

// form_paste.go — PASTING INTO A FORM FIELD.
//
// The operator: "The modal does not allow me to paste anything in any of the fields from my clipboard.
// ctrl+v should allow me to paste a secret name, token, etc."
//
// WHY IT NEVER WORKED. The composer has had ctrl+v since attachments existed, but it goes through
// composerBypassKeys → attachOrPasteFromClipboard, which reads the clipboard and attaches the result TO THE
// MESSAGE. A FORM field cannot use that path for two reasons:
//
//   - ctrl+v arrives as tea.KeyCtrlV, not as runes, so no form-insertion path sees it at all; and
//   - reading the clipboard SHELLS OUT, so the text cannot arrive inside a key handler — it has to come
//     back through Update as a message.
//
// So this file adds the missing half: an interception above every form (in dispatch), a command that reads
// the clipboard, and a handler that inserts the text into the TOPMOST form whose focused field can take it.
//
// It is deliberately guarded on a form being open. With no form up, ctrl+v is still the composer's
// attachment chord, exactly as before: pasting an image into a chat message must not change.

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// formClipboardTextMsg carries the system clipboard's TEXT back into the shell for a FORM FIELD.
//
// IT IS ITS OWN TYPE, and not the composer's clipboardTextMsg, because the two have different
// destinations: that one is answered by inserting into the COMPOSER (see the router's handler), while this
// one belongs to whichever form field has the focus. One shared message would have to guess, and guessing
// wrong would paste a token into a chat message.
//
// err is carried because a clipboard read can fail in ways the operator needs named (no reader installed, or
// the clipboard holds no text) — and because a paste that silently does nothing is exactly what was reported.
type formClipboardTextMsg struct {
	text string
	err  string
}

// pasteFromClipboardCmd reads the clipboard's TEXT.
//
// TEXT ONLY, never an image: a form field can only hold text, and the gesture that has to choose between
// the two (a screenshot pasted into a prompt) belongs to the composer, where the attachment lands.
func pasteFromClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		t, err := readClipboardText()
		if err != nil {
			return formClipboardTextMsg{err: err.Error()}
		}
		return formClipboardTextMsg{text: t}
	}
}

// formPasteTargets lists the text forms the shell hosts, TOPMOST FIRST — the same layering the router's key
// chain uses, so a paste lands in the field the operator is looking at rather than in a form behind it.
//
// Only kit2.Forms. The palette and the /models picker edit their own query and carry their own handling, and
// the composer is not a form at all.
func (m *App) formPasteTargets() []*kit2.Form {
	out := make([]*kit2.Form, 0, 5)
	if m.convScopeForm != nil {
		out = append(out, m.convScopeForm)
	}
	if m.assignForm != nil {
		out = append(out, m.assignForm)
	}
	if m.catForm != nil {
		out = append(out, m.catForm)
	}
	if m.renameConv != nil {
		out = append(out, m.renameConv)
	}
	if m.launch != nil && m.launch.Form != nil {
		out = append(out, m.launch.Form)
	}
	return out
}

// pasteIntoForms inserts text into the topmost form whose focused field can take it, reporting whether it
// landed anywhere.
func (m *App) pasteIntoForms(text string) bool {
	for _, f := range m.formPasteTargets() {
		if f.PasteText(text) {
			return true
		}
	}
	return false
}

// onClipboardText is the shell's answer to a clipboard read that ctrl+v started in a form.
func (m *App) onClipboardText(msg formClipboardTextMsg) tea.Cmd {
	if msg.err != "" {
		// SAID OUT LOUD, because the operator pressed a key and nothing else happened — silence would read
		// as "that key does not work here", which is the report being fixed.
		m.dock.SetError("paste: " + msg.err)
		return nil
	}
	if !m.pasteIntoForms(msg.text) {
		m.dock.SetError("paste: no open field to paste into")
	}
	return nil
}

// pasteKey is the paste DECISION, in one place, so it can be pinned without a clipboard.
//
// It is called from dispatch, ABOVE every form, because a form field cannot do this for itself: reading the
// clipboard shells out, so the text returns as a message rather than inside the key handler — and one
// interception serves every form in the shell instead of one per host.
//
// TWO SHAPES OF PASTE, and both must go through the form rather than around it:
//
//   - ctrl+v        a chord; nothing in the shell inserted it, so it has to be read. Answered with the cmd
//                   that reads the clipboard.
//   - bracketed     the TERMINAL's own paste, which arrives as runes and would otherwise be inserted by the
//                   form's ordinary rune path — losing the newline-and-whitespace sanitising PasteText does.
//
// GUARDED ON A FORM BEING OPEN, and that guard is the contract: with no form up, ctrl+v is the composer's
// attachment chord (paste an image, or a file path) and must stay exactly as it was, and a bracketed paste
// must reach the composer untouched.
func (m *App) pasteKey(msg tea.Msg) (tea.Cmd, bool) {
	if cm, ok := msg.(formClipboardTextMsg); ok {
		return m.onClipboardText(cm), true
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil, false
	}
	if len(m.formPasteTargets()) == 0 {
		return nil, false
	}
	if k.String() == "ctrl+v" {
		return pasteFromClipboardCmd(), true
	}
	if k.Paste {
		// It reached a form, so it is not the composer's: insert it the sanitising way and consume it.
		return nil, m.pasteIntoForms(string(k.Runes))
	}
	return nil, false
}
