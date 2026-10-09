package ask

// cardinput.go — PASTING INTO A CARD'S FREE-TEXT ROW.
//
// The operator: "In the TUI when an ask card has an 'other' option, I can type in there just fine, but
// ctrl+v doesn't seem to accept pasting. I can see users doing this quite frequently if they need to
// copy and paste code, errors, etc. We need to make this work."
//
// WHY IT DID NOT WORK, and it was structural rather than a missing binding. The shell's paste path
// (internal/tui/form_paste.go) intercepts ctrl+v ABOVE the forms, because reading the clipboard shells
// out — so the text has to come back as a message rather than arriving inside a key handler. That
// interception is guarded on `hasPasteTarget`, which knew only about kit2.Forms and the detail pane's
// inline editor. A card's free-text row is NEITHER: it is a plain string on the card's own state
// (ChatItem.Consent.OtherInput / ParsedAsk.Draft). So with a row open the shell saw no paste target,
// declined the key, and it fell through to the composer's chord — which pastes into the MESSAGE, not the
// row. Typing worked because runes take the ordinary path; a clipboard read cannot.
//
// The fix gives the cards the same capability the forms have, so the ONE interception reaches them too
// rather than the shell learning about cards by name.

import tea "github.com/charmbracelet/bubbletea"

// cardInputRow names the free-text row that currently OWNS THE KEYBOARD: "consent", "draft", or "".
//
// ONE PREDICATE, because ownership is asked in two places (does the shell read the clipboard for this
// key? where does the text go?) and two copies of the rule would drift. It is the SAME deferral rule
// ClaimsKeys applies: ctrl+g hands the keyboard to the composer (see DropKeyClaim), so a deferred row is
// still on screen and still collecting nothing — and a paste into it would vanish into a row the operator
// has stepped away from, which is precisely the silence this fix exists to remove.
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
//
// It is the shell's half of the contract: with this true, ctrl+v belongs to the ROW, and with it false the
// composer's attachment chord must stay exactly as it was (see App.hasPasteTarget).
func (m *Model) CardInputOpen() bool { return m.cardInputRow() != "" }

// PasteIntoCardInput inserts PASTED text into the open row, reporting whether it landed and returning the
// repaint the row needs.
//
// THE TEXT GOES IN AS PASTED — no reflowing, no newline collapsing. That is deliberately UNLIKE
// kit2.Form.PasteText, which flattens newlines for its single-line kinds, and the difference is what the
// destination is. A form field holds a STRUCTURED value (a token, a secret name, a slug) that the operator
// sees one line of, so an invisible newline inside it is a defect. A card row holds an ANSWER: it becomes a
// message or a tool result, where line breaks are legitimate content the operator pasted on purpose — and
// pasting code and error traces is the whole point of this fix. Reformatting that silently would be the
// defect here.
//
// The Cmd is NOT optional: the row is drawn from the transcript, and the key handlers that type into it
// repaint for the same reason (see handleConsentKey). A paste that mutates the state and does not repaint
// shows the operator nothing, which is indistinguishable from the bug being reported.
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
