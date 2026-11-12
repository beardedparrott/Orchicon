package tui

// form_paste.go — PASTING INTO A FORM FIELD.

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/screens/kit2"
)

// formClipboardTextMsg carries the system clipboard's TEXT back into the shell for a FORM FIELD.
type formClipboardTextMsg struct {
	text string
	err  string
}

// TEXT ONLY, never an image: a form field can only hold text, and the gesture that has to choose
// between the two (a screenshot pasted into a prompt) belongs to the composer.
func pasteFromClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		t, err := readClipboardText()
		if err != nil {
			return formClipboardTextMsg{err: err.Error()}
		}
		return formClipboardTextMsg{text: t}
	}
}

// formPasteTargets lists the text forms the shell hosts, TOPMOST FIRST — the same layering the
// router's key chain uses.
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

// pasteIntoForms inserts text into the topmost form that can take it, reporting whether it landed
// anywhere.
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

// cardPaster is a screen whose CARD hosts a free-text input row (the ask cards' "Other" row, and
// the recorded card's draft row).
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

// pasteIntoCardInput routes text to that row, reporting whether it landed and returning its
// repaint.
func (m *App) pasteIntoCardInput(text string) (bool, tea.Cmd) {
	p, ok := m.cardPasteScreen()
	if !ok || !p.CardInputOpen() {
		return false, nil
	}
	return p.PasteIntoCardInput(text)
}

// pasteScreen is the ACTIVE screen, when it can take a paste at all.
func (m *App) pasteScreen() (interface{ PasteIntoForm(string) bool }, bool) {
	s, ok := m.screens[m.active]
	if !ok || s == nil {
		return nil, false
	}
	p, ok := s.(interface{ PasteIntoForm(string) bool })
	return p, ok
}

// hasPasteTarget reports whether ANY form is open to paste into — the shell's, or the active
// screen's.
func (m *App) hasPasteTarget() bool {
	if len(m.formPasteTargets()) > 0 {
		return true
	}
	// A CARD'S FREE-TEXT ROW IS A TARGET TOO.
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
