package tui

// composer_send_funnel_test.go — A KEY THAT PRODUCES A SEND MUST ALWAYS DISPATCH IT.
//
// The operator: "Sometimes on subsequent messages in Ask Orchicon conversations, I don't see the orchicon is
// thinking and I have to send another message" — and then, decisively: "my message does not appear when
// this happens."
//
// THAT SECOND DETAIL IS THE DIAGNOSIS. The optimistic echo is appended by sendFromComposer and BOTH store
// paths preserve it (mergeHistory and replace each keep a "draft-*" row the durable view has not caught up
// with), so a message that never appears cannot have been echoed — which means sendFromComposer never ran.
// The composer emptied anyway, because the DOCK clears the buffer itself. Both halves together are a
// swallowed send: the dock parks the message in a pending slot for the shell to collect, and the shell
// never collected it.
//
// The hole was a COUNT: four callers of dock.Update, one of which collected the pending send. The wiring
// test below drives the one that could carry a send — the slash palette forwards every key it does not case
// to the dock, and its cases include "enter" but NOT "ctrl+j", which is the dock's own second spelling of
// Enter (dock.enterKey: some terminals and PTY configurations send LF).

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// composerEcho reports whether the operator's text landed in the transcript store — the optimistic echo
// that sendFromComposer appends. Its absence is the operator's "my message does not appear".
func composerEcho(m *App, convID, text string) bool {
	for _, it := range m.chatStore.snapshot(convID) {
		if strings.Contains(it.Text, text) {
			return true
		}
	}
	return false
}

// THE PALETTE MUST NOT EAT A SEND. Enter arrives here as ctrl+j (the LF spelling), which the palette does
// not case, so it is forwarded to the dock — and the dock treats it as SEND.
func TestThePaletteForwardsASendInsteadOfEatingIt(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.refreshLayout()
	m.dock.Focus()

	// Open the palette the way the operator does: a leading "/" on an empty composer.
	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = nm
	if !m.palette.PaletteOpen() {
		t.Fatal("fixture: \"/\" did not open the palette")
	}

	// Give it something to send, then press Enter AS LF (ctrl+j) — the spelling the palette does not case.
	m.dock.SetValue("a message that must not vanish")
	_, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlJ})
	if cmd == nil {
		t.Error("the send produced no command at all")
	}

	if m.dock.SendRequest() != "" {
		t.Error("the pending send was not collected — this is the swallow: the dock cleared the buffer and " +
			"parked the message for a shell that never asked for it")
	}
	if !composerEcho(m, "c1", "a message that must not vanish") {
		t.Errorf("the operator's message is not in the transcript store, so nothing was sent and nothing "+
			"will appear — the reported symptom. store: %+v", m.chatStore.snapshot("c1"))
	}
}

// AND THE ORDINARY PATH STILL SENDS EXACTLY ONCE. The funnel batches the send with whatever the key
// produced, so the risk of the change is a DOUBLE send on the path that already worked.
func TestTheOrdinarySendStillHappensExactlyOnce(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.refreshLayout()
	m.dock.Focus()
	m.dock.SetValue("plain message")

	nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm

	var echoes int
	for _, it := range m.chatStore.snapshot("c1") {
		if strings.Contains(it.Text, "plain message") {
			echoes++
		}
	}
	if echoes != 1 {
		t.Errorf("the message was echoed %d time(s), want exactly 1 — the funnel must not double-send", echoes)
	}
	if m.dock.Value() != "" {
		t.Errorf("the composer still holds %q after a send", m.dock.Value())
	}
}

// A NON-SENDING KEY IS UNCHANGED: the funnel must not turn ordinary typing into a send.
func TestTypingThroughTheFunnelDoesNotSend(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)
	m.refreshLayout()
	m.dock.Focus()

	for _, r := range "hello" {
		nm, _ := m.dispatch(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm
	}
	if got := m.dock.Value(); got != "hello" {
		t.Fatalf("composer = %q, want \"hello\"", got)
	}
	if composerEcho(m, "c1", "hello") {
		t.Error("typing a character sent a message")
	}
}
