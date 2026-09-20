package tui

// composer_sending_ack_test.go — THE SEND ACK MAY NOT OUTLIVE THE SEND.
//
// The operator: "I also notice a 'sending ...' underneath the composer even when the model is done. We
// need to investigate that."
//
// The mechanism, reproduced before the fix. The dock writes "sending …" the instant Enter fires (which is
// deliberate: a send that produces no visible reaction is indistinguishable from a dead key), and exactly
// TWO outcomes replaced it: a FAILURE (setChatError) and a NEW conversation landing (chatConvCreatedMsg).
// A SECOND message in an EXISTING conversation went through neither, so the ack stayed on screen for the
// rest of the session. The first test drives that path and asserts the frame, not the field.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/dock"
)

// sendingOnScreen reports whether the composer's strip is showing the send ack in the rendered frame.
func sendingOnScreen(m *App) bool {
	for _, r := range strings.Split(m.viewFrame(), "\n") {
		if strings.Contains(ansi.Strip(r), dock.SendingAck) {
			return true
		}
	}
	return false
}

// sendSecondMessage puts the operator through the reported sequence: an existing conversation with a
// completed exchange, then a follow-up typed and sent with Enter.
func sendSecondMessage(t *testing.T) *App {
	t.Helper()
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "first", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "first reply", Key: "t1", At: 2})
	m.onChatWake()
	m.refreshLayout()
	m.dock.Focus()
	m.dock.SetValue("second message")
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	app := nm
	if app == nil {
		t.Fatal("dispatch returned a nil app")
	}
	if !app.chat.IsStreaming("c1") {
		t.Fatalf("fixture: the turn did not start, so this test cannot tell a settled ack from an unsent one")
	}
	if !sendingOnScreen(app) {
		t.Fatalf("fixture: %q is not on screen right after Enter, so there is nothing for this test to "+
			"settle", dock.SendingAck)
	}
	return app
}

// THE SERVER'S ACK SETTLES IT — the ack means "handed over, nothing back yet", and that stops being true
// the moment the turn is accepted.
func TestTheTurnAckSettlesTheSendAck(t *testing.T) {
	m := sendSecondMessage(t)

	if _, cmd := m.dispatch(chat.TurnAckedMsg{ConvID: "c1"}); cmd == nil {
		// Not an assertion about behaviour — the router re-arms the message channel. Asserted so a future
		// refactor that drops that re-arm is caught here rather than by a hung session.
		t.Error("handling the turn ack did not re-arm the chat message channel (waitChat)")
	}
	if m.dock.Notice != "" {
		t.Errorf("the composer still says %q after the turn was acked", m.dock.Notice)
	}
	if sendingOnScreen(m) {
		t.Errorf("%q is still on screen after the turn was acked", dock.SendingAck)
	}
}

// AND SO DOES THE TURN'S END — the second settle, which covers a turn whose ack never reached the shell
// (the channel drops it under load) or one that was already acked when the shell attached.
func TestTheEndOfTheTurnSettlesTheSendAck(t *testing.T) {
	m := sendSecondMessage(t)

	// Deliberately NO TurnAckedMsg: this is the missed-ack case.
	m.dispatch(chat.StreamDoneMsg{ConvID: "c1", Gen: m.chat.CurrentGen("c1")})

	if m.dock.Notice != "" {
		t.Errorf("the composer still says %q after the turn ended, with no ack delivered", m.dock.Notice)
	}
	if sendingOnScreen(m) {
		t.Errorf("%q is still on screen after the turn ended", dock.SendingAck)
	}
}

// THE SETTLE IS GUARDED: it may clear the ACK and nothing else. A connection banner is written when a turn
// loses its socket, and a late settle erasing it would hide a dead connection behind a blank strip — the
// failure this guard exists to prevent.
func TestSettlingTheAckNeverErasesAnotherNotice(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.dock.SetNotice("connection lost — re-attaching to the running turn…")

	if m.dock.SettleSendingAck() {
		t.Error("SettleSendingAck reported clearing a notice that was not the send ack")
	}
	if !strings.Contains(m.dock.Notice, "connection lost") {
		t.Errorf("the connection banner was erased: notice = %q", m.dock.Notice)
	}
}

// A FAILED SEND STILL PUTS THE DRAFT BACK, and still settles the ack — the pre-existing behaviour this
// change must not disturb (the error strip is written AFTER the settle, so it wins).
func TestAFailedSendStillSettlesTheAck(t *testing.T) {
	m := sendSecondMessage(t)
	m.setChatError("send", errStringer("boom"))

	if m.dock.Notice != "" {
		t.Errorf("a failed send left the strip saying %q", m.dock.Notice)
	}
	if m.dock.Err == "" {
		t.Error("a failed send wrote no error strip")
	}
}

// errStringer is a minimal error for the failure-path test.
type errStringer string

func (e errStringer) Error() string { return string(e) }
