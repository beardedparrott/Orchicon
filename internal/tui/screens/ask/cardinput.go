package ask

// cardinput.go — PASTING INTO A CARD'S FREE-TEXT ROW.

import tea "github.com/charmbracelet/bubbletea"

// cardInputRow names the free-text row that currently OWNS THE KEYBOARD: "consent", "draft", or "".
func (m *Model) cardInputRow() string {
	if m.consent != nil && m.consent.OtherMode && m.consent.Ask.ID != m.consentDeferred {
		return "consent"
	}
	if m.draft != nil && m.draft.Drafting && m.draftKey != m.draftDeferred {
		return "draft"
	}
	return ""
}

// CardInputOpen reports whether a card's free-text row is up AND owns the keyboard.
func (m *Model) CardInputOpen() bool { return m.cardInputRow() != "" }

// PasteIntoCardInput inserts PASTED text into the open row, reporting whether it landed and
// returning the repaint the row needs.
func (m *Model) PasteIntoCardInput(text string) (bool, tea.Cmd) {
	if text == "" {
		return false, nil
	}
	switch m.cardInputRow() {
	case "consent":
		m.consent.OtherInput += text
		return true, m.repaint()
	case "draft":
		m.draft.Draft += text
		return true, m.repaint()
	}
	return false, nil
}

// CardRowInput exposes which row is open and what it holds, for tests and for any surface that needs to
// render the row's state without reaching into the fields.
func (m *Model) CardRowInput() (string, string) {
	switch m.cardInputRow() {
	case "consent":
		return "consent", m.consent.OtherInput
	case "draft":
		return "draft", m.draft.Draft
	}
	return "", ""
}
