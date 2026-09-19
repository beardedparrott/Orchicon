package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// ask_render_stop_test.go — the two Ask behaviours that have to be judged at the RENDERED FRAME rather
// than at the widget, plus the Stop control.

// askWithTranscript builds the REAL Ask screen on the stub-backed App, opens a conversation, and leaves
// the pane showing that conversation's transcript. It returns the stub too, so a test can assert which
// RPCs were issued.
func askWithTranscript(t *testing.T, convID string) (*App, *stubAskParity) {
	t.Helper()
	m, stub := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation(convID)
	return m, stub
}

// A FULL TRANSCRIPT MUST STILL PAINT ITS NEWEST LINE AND THE THINKING NOTICE.
//
// The operator, twice:
//
//	"I do see user messages being printed before the model message immediately but only until the
//	 screen reaches the bottom. Once we hit the bottom pane, I no longer see my messages popping up
//	 right away. (i.e. I don't think it is auto scrolling up for user messages)"
//	"I still don't see the 'Orchicon is thinking...' being printed when the model is responding."
//
// BOTH SYMPTOMS, ONE CAUSE. The shell sized the transcript Stream from m.contentHeight() — the whole
// content region — while the detail pane spends rows on its title, its fields and its scroll label. So
// the stream rendered MORE rows than the pane draws, and the pane (a kit2 Panel) CLIPS THE TAIL. The rows
// it lost were the newest ones: the operator's own just-sent message, and the notice line, which is
// emitted after every transcript row. While the transcript was SHORTER than the pane everything fitted
// and it looked correct — which is exactly why both symptoms only appeared once the pane filled up.
//
// THIS TEST ASSERTS ON THE PAINTED FRAME, and that is the whole point. ask_parity_test.go already asserts
// str.Notice and str.Lines — the WIDGET's own fields — and both of those assertions PASS while the pane
// clips them away. That is how this shipped twice: the coverage certified the layer above the bug.
// m.View() is the only layer at which the operator's report is true or false.
func TestAskFullTranscriptPaintsTheNewestLineAndTheThinkingNotice(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")

	// A transcript far taller than the pane: 80 marks of scrollback.
	for i := 0; i < 80; i++ {
		m.chatStore.append("c1", chat.ChatItem{
			Kind: chat.KindUser, Text: fmt.Sprintf("history-%02d", i), Key: fmt.Sprintf("h%d", i),
			At: int64(i),
		})
	}
	// The operator's newest message, appended exactly as the send path does (the optimistic echo).
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindUser, Text: "NEWEST-USER-LINE", Key: "draft-newest", At: 1000, Live: true,
	})
	// A turn is in flight and has produced no prose yet. chat.Send flips the slot SYNCHRONOUSLY, so the
	// state is real; the returned Cmd is deliberately dropped, because what the pane paints needs no
	// network.
	m.chat.Send("c1", "NEWEST-USER-LINE", "")
	m.onChatWake()

	frame := m.View()
	if !strings.Contains(frame, "NEWEST-USER-LINE") {
		t.Errorf("the newest message is not in the painted frame — the pane is clipping the tail of a "+
			"transcript taller than itself, which is the operator's \"once we hit the bottom pane, I no "+
			"longer see my messages popping up right away\"\n--- frame tail ---\n%s", tailOf(frame, 2000))
	}
	if !strings.Contains(frame, "Orchicon is thinking…") {
		t.Errorf("the thinking indicator is not in the painted frame — the notice is emitted AFTER the "+
			"transcript rows, so an over-tall stream clips it away (the operator: \"I still don't see the "+
			"'Orchicon is thinking...' being printed when the model is responding\")\n--- frame tail ---\n%s",
			tailOf(frame, 2000))
	}
}

// THE STOP CONTROL: ctrl+y interrupts the in-flight reply.
//
// The operator: "I realized there is no stop button like in the GUI. We need a stop button/control key +
// hint in composer that can interrupt the agent mid flight."
//
// Driven through the REAL dispatch, because the failure mode this class of feature ships with is a chord
// that is bound but UNREACHABLE: the composer's textarea consumes every key it is handed and the composer
// branch RETURNS on a consumed key, so a global route never sees a chord the composer was allowed to eat.
// That is exactly why ctrl+y is in composerBypassKeys, and this test fails if it is removed from there.
func TestStopChordAbortsTheInFlightTurn(t *testing.T) {
	m, stub := askWithTranscript(t, "c1")

	m.chat.Send("c1", "hello", "") // a turn is in flight
	if !m.chat.IsStreaming("c1") {
		t.Fatal("precondition: the turn must be streaming")
	}

	// The affordance is advertised WHILE the turn runs — the operator asked for the hint explicitly.
	m.refreshComposerHint()
	if h := m.dock.Hint(); !strings.Contains(h, "ctrl+y") {
		t.Fatalf("the composer does not offer the stop chord while a reply streams: %q", h)
	}

	_, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlY})
	if cmd == nil {
		t.Fatal("ctrl+y must issue the abort — a stop chord that produces no Cmd is bound but dead")
	}
	msg, ok := cmd().(chat.AbortTurnMsg)
	if !ok {
		t.Fatalf("ctrl+y produced %T, want chat.AbortTurnMsg", cmd())
	}
	if msg.Err != "" {
		t.Fatalf("abort failed: %s", msg.Err)
	}
	if stub.abortedID != "c1" {
		t.Errorf("AbortConversationTurn got %q, want c1 — Stop must abort the OPEN conversation", stub.abortedID)
	}
	if m.chat.IsStreaming("c1") {
		t.Error("the turn slot must be cleared the moment the abort lands, so the UI recovers at once " +
			"instead of waiting for the socket (the GUI clears its slot for the same reason)")
	}

	// The shell's own handler settles the surfaces: the stop affordance goes, and the transcript's
	// thinking notice goes with it.
	nm, _ := m.dispatch(msg)
	m = nm
	if h := m.dock.Hint(); strings.Contains(h, "ctrl+y") {
		t.Errorf("the stop affordance outlived the turn it stops: %q", h)
	}
	if !strings.Contains(m.dock.Notice, "stopped") {
		t.Errorf("the stop must be acknowledged in the composer strip, notice = %q", m.dock.Notice)
	}
	if str := m.TranscriptStream("c1"); str != nil && str.Notice != "" {
		t.Errorf("the thinking indicator must clear when the turn stops, notice = %q", str.Notice)
	}
}

// AN IDLE ctrl+y SAYS WHY IT DID NOTHING. A chord that silently does nothing is indistinguishable from a
// chord that is not bound — the failure class the composer's "sending …" ack exists for.
func TestStopChordIsHonestWithNothingToStop(t *testing.T) {
	m, stub := askWithTranscript(t, "c1")
	_, cmd := m.dispatch(tea.KeyMsg{Type: tea.KeyCtrlY})
	if cmd != nil {
		t.Fatalf("an idle ctrl+y must not issue an RPC, got a Cmd of %T", cmd())
	}
	if stub.abortedID != "" {
		t.Errorf("no abort may be sent when nothing is in flight, got %q", stub.abortedID)
	}
	if !strings.Contains(m.dock.Notice, "nothing to stop") {
		t.Fatalf("an idle stop must name its reason rather than being inert: notice = %q", m.dock.Notice)
	}
}

// THE STOP HINT EXISTS ONLY WHILE A REPLY IS IN FLIGHT, so the affordance row can never advertise a dead
// key. The state is derived from the controller (not a second flag that could drift).
func TestComposerStopHintAppearsOnlyWhileAReplyStreams(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.refreshComposerHint()
	if h := m.dock.Hint(); strings.Contains(h, "ctrl+y") {
		t.Fatalf("the stop affordance must not be offered with no reply in flight: %q", h)
	}

	m.chat.Send("c1", "x", "")
	m.refreshComposerHint()
	if h := m.dock.Hint(); !strings.Contains(h, "ctrl+y") {
		t.Fatalf("the stop affordance must appear while a reply streams: %q", h)
	}

	// The normal end of a turn clears the slot, and the affordance goes with it. The generation is the
	// slot's own, which is what consume() carries into StreamDoneMsg — a StreamDoneMsg with the wrong
	// generation is a stream that was SUPERSEDED, and must leave this slot alone.
	m.onStreamDone(chat.StreamDoneMsg{ConvID: "c1", Gen: m.chat.CurrentGen("c1")})
	if h := m.dock.Hint(); strings.Contains(h, "ctrl+y") {
		t.Fatalf("the stop affordance must disappear when the reply ends: %q", h)
	}
}
