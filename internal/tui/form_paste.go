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

// pasteIntoForms inserts text into the topmost form that can take it, reporting whether it landed anywhere.
//
// TWO HOSTS, in layering order. The SHELL's modals first (they are drawn above every screen), then the ACTIVE
// SCREEN's own form — which is where most forms live, in the details pane, and where the report's secret
// form was.
func (m *App) pasteIntoForms(text string) bool {
	for _, f := range m.formPasteTargets() {
		if f.PasteText(text) {
			return true
		}
	}
	if p, ok := m.pasteScreen(); ok {
		return p.PasteIntoForm(text)
	}
	return false
}

// cardPaster is a screen whose CARD hosts a free-text input row (the ask cards' "Other" row, and the
// recorded card's draft row).
//
// IT EXISTS FOR THE SAME REASON pasteIntoForm DOES, and that reason is the whole shape of this file: the
// interception is one place ABOVE the surfaces, so every surface that can take a paste must be reachable
// from it. Cards were not, because a card's row is a plain string on the card's state rather than a
// kit2.Form — so ctrl+v fell through to the composer and pasted into the message while the row sat there
// visibly able to accept typing but not a paste.
//
// The Ask screen implements it. It is asked for by CAPABILITY rather than by name so a second screen with
// card inputs needs no change here.
type cardPaster interface {
	// CardInputOpen reports a free-text row that is up AND owns the keyboard (a row the operator deferred
	// with ctrl+g does not, or the paste would land where their typing is not going).
	CardInputOpen() bool
	// PasteIntoCardInput inserts the text, reporting whether it landed and the repaint the row needs.
	PasteIntoCardInput(text string) (bool, tea.Cmd)
}

// cardPasteScreen is the ACTIVE screen, when it hosts card input rows at all.
func (m *App) cardPasteScreen() (cardPaster, bool) {
	s, ok := m.screens[m.active]
	if !ok || s == nil {
		return nil, false
	}
	p, ok := s.(cardPaster)
	return p, ok
}

// cardInputOpen reports whether the active screen has a card free-text row owning the keyboard.
func (m *App) cardInputOpen() bool {
	p, ok := m.cardPasteScreen()
	return ok && p.CardInputOpen()
}

// pasteIntoCardInput routes text to that row, reporting whether it landed and returning its repaint.
//
// IT RETURNS (landed, cmd) IN THE SAME ORDER AS THE CAPABILITY IT DELEGATES TO, so the two signatures
// cannot be transposed — which is exactly the bug this helper had in its first draft (the compile error
// was the least bad outcome; the same shape with two bools would have swapped silently).
func (m *App) pasteIntoCardInput(text string) (bool, tea.Cmd) {
	p, ok := m.cardPasteScreen()
	if !ok || !p.CardInputOpen() {
		return false, nil
	}
	return p.PasteIntoCardInput(text)
}

// pasteScreen is the ACTIVE screen, when it can take a paste at all.
//
// The capability is ASKED FOR rather than listed: screens embed kit2.Base, which hosts their forms, so one
// assertion reaches every screen's form and no screen has to be remembered here.
func (m *App) pasteScreen() (interface{ PasteIntoForm(string) bool }, bool) {
	s, ok := m.screens[m.active]
	if !ok || s == nil {
		return nil, false
	}
	p, ok := s.(interface{ PasteIntoForm(string) bool })
	return p, ok
}

// hasPasteTarget reports whether ANY form is open to paste into — the shell's, or the active screen's.
//
// IT ASKS FOR AN OPEN FORM, NOT FOR THE CAPABILITY, and the difference is the whole guard: every screen can
// paste by virtue of embedding Base, so "can paste" would be true on every tab and ctrl+v would never
// reach the composer again. EditingDetail is the accurate question —
// "is a form actually up?".
func (m *App) hasPasteTarget() bool {
	if len(m.formPasteTargets()) > 0 {
		return true
	}
	// A CARD'S FREE-TEXT ROW IS A TARGET TOO. Without this the guard reports "no field to paste into"
	// while a row is plainly on screen accepting keystrokes, and ctrl+v goes to the composer instead —
	// the operator's report exactly.
	if m.cardInputOpen() {
		return true
	}
	s, ok := m.screens[m.active]
	if !ok || s == nil {
		return false
	}
	ed, ok := s.(interface{ EditingDetail() bool })
	return ok && ed.EditingDetail()
}

// onClipboardText is the shell's answer to a clipboard read that ctrl+v started in a form.
func (m *App) onClipboardText(msg formClipboardTextMsg) tea.Cmd {
	if msg.err != "" {
		// SAID OUT LOUD, because the operator pressed a key and nothing else happened — silence would read
		// as "that key does not work here", which is the report being fixed.
		m.dock.SetError("paste: " + msg.err)
		return nil
	}
	if m.pasteIntoForms(msg.text) {
		return nil
	}
	// FORMS FIRST, CARDS SECOND: the two are never up together (a card row and an open form are different
	// modes of the same pane), so the order is only about which is reported when neither can take it.
	if landed, cmd := m.pasteIntoCardInput(msg.text); landed {
		return cmd
	}
	m.dock.SetError("paste: no open field to paste into")
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
//     that reads the clipboard.
//   - bracketed     the TERMINAL's own paste, which arrives as runes and would otherwise be inserted by the
//     form's ordinary rune path — losing the newline-and-whitespace sanitising PasteText does.
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
	if !m.hasPasteTarget() {
		return nil, false
	}
	if k.String() == "ctrl+v" {
		return pasteFromClipboardCmd(), true
	}
	if k.Paste {
		// It reached a form or a card row, so it is not the composer's: insert it and consume it. Routed
		// the same way ctrl+v is, so the two shapes of paste cannot land in different places.
		if m.pasteIntoForms(string(k.Runes)) {
			return nil, true
		}
		if landed, cmd := m.pasteIntoCardInput(string(k.Runes)); landed {
			return cmd, true
		}
		return nil, false
	}
	return nil, false
}
