package tui

// card_paste_test.go — THE SHELL'S HALF OF PASTING INTO A CARD'S FREE-TEXT ROW.
//
// The operator: "In the TUI when an ask card has an 'other' option, I can type in there just fine, but
// ctrl+v doesn't seem to accept pasting. I can see users doing this quite frequently if they need to copy
// and paste code, errors, etc. We need to make this work."
//
// The shell owns this half because a clipboard read SHELLS OUT: ctrl+v arrives as tea.KeyCtrlV with no
// runes, so nothing in the card can insert it, and the text has to return as a message. The interception
// (pasteKey, in dispatch) therefore decides WHERE a paste goes — and that decision is what was wrong. It
// knew about forms only, so with a card row open it declined the key and ctrl+v became the composer's
// attachment chord, pasting into the MESSAGE while the row sat there accepting typing.
//
// What is pinned here is the DECISION and the DESTINATION, not the clipboard reader (which execs a helper).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// cardRowOpen returns a shell whose card's free-text row is open, driven through the REAL keyboard path
// (down to Other, Enter) rather than by poking the state — so the fixture cannot disagree with how the row
// actually opens in the product.
func cardRowOpen(t *testing.T) (*App, []chat.ChatItem) {
	t.Helper()
	m, items := otherCardPlane(t)
	// rows: develop(0), main(1), Other(2)
	for i := 0; i < 2; i++ {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(*App)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(*App)
	if !items[0].Consent.OtherMode {
		t.Fatal("fixture: the free-text row never opened — the assertions below would prove nothing")
	}
	return m, items
}

// THE REPORT: ctrl+v with a card row open must be HANDLED by the shell (so it reads the clipboard) rather
// than falling through to the composer's chord.
func TestCtrlVIsHandledWhileACardRowIsOpen(t *testing.T) {
	m, _ := cardRowOpen(t)

	cmd, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV})
	if !handled {
		t.Fatal("ctrl+v was not handled while a card's free-text row was open — this is the reported bug: " +
			"the key fell through to the composer and pasted into the message")
	}
	if cmd == nil {
		t.Fatal("ctrl+v produced no clipboard read, so the key is swallowed and nothing is pasted")
	}
}

// AND THE TEXT LANDS IN THE ROW, not in the composer — the destination is the other half of the bug.
func TestClipboardTextLandsInTheCardRow(t *testing.T) {
	m, items := cardRowOpen(t)
	before := m.dock.Value()

	text := "panic: nil map write\n\tat runner.go:118"
	// THE RETURNED Cmd IS DELIBERATELY NOT ASSERTED HERE. It is whatever the host's RepaintTranscript
	// gives, and that is onChatWake — which legitimately returns nil when there is nothing to wake. The
	// guarantee that matters is that the row ASKED for a repaint, and that is pinned where it is
	// deterministic: TestPasteLandsInTheConsentOtherRow (ask package) counts the host's repaint calls.
	m.onClipboardText(formClipboardTextMsg{text: text})
	if got := items[0].Consent.OtherInput; got != text {
		t.Fatalf("the row holds %q, want the pasted text %q", got, text)
	}
	if m.dock.Value() != before {
		t.Fatalf("the paste reached the COMPOSER (now %q) instead of the row", m.dock.Value())
	}
}

// THE TERMINAL'S OWN PASTE (bracketed) takes the same route, so the two shapes of paste cannot land in
// different places — and so a pasted newline is not inserted as a raw rune sequence by the card's ordinary
// rune path.
func TestBracketedPasteLandsInTheCardRow(t *testing.T) {
	m, items := cardRowOpen(t)

	// The Cmd is not asserted, for the reason given in TestClipboardTextLandsInTheCardRow.
	_, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("exit status 1"), Paste: true})
	if !handled {
		t.Fatal("a bracketed paste with a card row open was not handled")
	}
	if got := items[0].Consent.OtherInput; got != "exit status 1" {
		t.Fatalf("the row holds %q, want the bracketed paste", got)
	}
}

// THE GUARD IS THE CONTRACT: with no card row open, ctrl+v is still the composer's, exactly as before this
// change. Without this the interception would steal the composer's paste — an image, or a file path — for
// the whole app.
func TestCtrlVStaysTheComposersWithNoCardRowOpen(t *testing.T) {
	m, items := otherCardPlane(t)
	if items[0].Consent.OtherMode {
		t.Fatal("fixture: the row is open, so this guard is untested")
	}
	if _, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV}); handled {
		t.Fatal("ctrl+v was claimed with no card row open — the composer's own paste would never run")
	}
}

// A DEFERRED ROW YIELDS THE PASTE, for the same reason it yields the keyboard: ctrl+g hands focus to the
// composer, so a paste there belongs to the composer and must not vanish into a row the operator has
// stepped away from.
func TestACtrlGDeferredCardRowDoesNotTakeThePaste(t *testing.T) {
	m, items := cardRowOpen(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = next.(*App)

	if _, handled := m.pasteKey(tea.KeyMsg{Type: tea.KeyCtrlV}); handled {
		t.Fatal("ctrl+v was claimed for a row the operator deferred with ctrl+g — the text would land " +
			"where their typing is not going")
	}
	before := items[0].Consent.OtherInput
	m.onClipboardText(formClipboardTextMsg{text: "ignored"})
	if items[0].Consent.OtherInput != before {
		t.Fatalf("the deferred row collected %q", items[0].Consent.OtherInput)
	}
	// AND THE ROW IS STILL THERE, so the fix is a yield rather than a dismissal.
	if !strings.Contains(m.viewFrame(), chat.ConsentOther) {
		t.Error("the deferred card's Other row vanished — ctrl+g must not close it")
	}
}
