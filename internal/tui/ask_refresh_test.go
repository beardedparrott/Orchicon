package tui

// ask_refresh_test.go — THE ROLLING WINDOW REPAINTS THE ASK TRANSCRIPT.
//
// The operator: "The conversations in the TUI are NOT truly live. I have to click away and back again to
// see updates. No 'Orchicon is thinking...' block. No reasoning block. No live thinking text block or live
// update when responses happen."
//
// THE MECHANISM THIS COVERS. Ask liveness rested entirely on one chain: a stream chunk →
// AppendLiveItem → a NON-BLOCKING poke into a CAP-1 channel → the waiter → appMsg → onChatWake. Every
// link is load-bearing and every one of them fails SILENTLY:
//
//   - a poke into a full channel is dropped (the non-blocking send),
//   - a waiter that was never re-armed on some path is simply not there to drain it.
//
// So a single miss does not degrade the pane — it stops it, for good. The only thing left that repaints is
// a full reload, which is precisely "click away and back again".
//
// The rolling refresh window exists for exactly this class of failure (its own header: "the safety net
// that makes 'the data is there but the screen does not know' the operator's problem no longer") — but it
// dispatches through the screen's Refresher hook, and Ask had no such hook, because its transcript is a
// CACHE MERGE rather than a server read. So the one surface the operator named was the one surface the
// net did not cover.
//
// The tick now repaints it. What is asserted here is the RECOVERY PROPERTY, not the tick: a lost poke must
// no longer be permanent — and each of the operator's four symptoms must arrive without one.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// askRelaunched builds a real Ask screen with a conversation open. A stub plane is enough: nothing here
// needs a server, because the whole point is that the pane refreshes from what the shell ALREADY holds.
func askRelaunched(t *testing.T, convID string) *App {
	t.Helper()
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation(convID)
	m.active = TabAsk
	return m
}

// THE LOST POKE IS NO LONGER PERMANENT. A chunk lands with NO wake delivered — the dropped-poke case —
// and one turn of the rolling window still puts it on screen.
//
// This is the operator's report in one test: before the fix, the pane stayed stale until they clicked away
// and back; the assertion is that a tick is enough.
func TestRollingTickRepaintsTheAskTranscriptWithoutAPoke(t *testing.T) {
	m := askRelaunched(t, "c1")

	// The durable transcript and a LIVE chunk arrive. Deliberately NO poke: this is the missed-wake case,
	// and the pane must recover on the timer alone.
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "LIVE-REPLY-TOKEN", Key: "t1", Live: true, At: 2})

	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.View(), "LIVE-REPLY-TOKEN") {
		t.Fatal("fixture: the pane already shows the chunk, so this test would measure nothing")
	}

	// ONE TURN OF THE WINDOW. onChatWake paints SYNCHRONOUSLY (it merges the cache into the pane and
	// returns nil — there is nothing to await), so the call itself is the repaint; any command it does
	// produce is run for completeness.
	cmd := m.refreshActiveView()
	if cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}

	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("the tick never created the transcript stream — a lost poke leaves the transcript frozen " +
			"for the rest of the session, which is the operator's \"I have to click away and back again\"")
	}
	if !strings.Contains(str.View(), "LIVE-REPLY-TOKEN") {
		t.Errorf("the tick did not paint the live chunk:\n%s", str.View())
	}
}

// AND THE THINKING NOTICE ARRIVES ON THE TICK, with no poke at all — symptom 2 on its own.
func TestRollingTickPaintsTheThinkingNotice(t *testing.T) {
	m := askRelaunched(t, "c1")

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	_ = m.chat.Send("c1", "hello", "") // a turn is genuinely in flight; the Cmd is dropped

	if cmd := m.refreshActiveView(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if !strings.Contains(m.View(), "Orchicon is thinking") {
		t.Errorf("the tick did not produce the thinking indicator — the operator's \"No 'Orchicon is "+
			"thinking...' block\"\n%s", m.View())
	}
}

// AND A REASONING CHUNK PAINTS ON THE TICK — symptom 3, the live thinking text.
func TestRollingTickPaintsLiveReasoning(t *testing.T) {
	m := askRelaunched(t, "c1")

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{
		Kind: chat.KindReasoning, Text: "weighing the two options", Key: "r1", Live: true, At: 2,
	})

	if cmd := m.refreshActiveView(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("the tick never created the transcript stream")
	}
	if !strings.Contains(str.View(), "weighing the two options") {
		t.Errorf("live reasoning did not reach the pane on the tick — the operator's \"No reasoning block\"\n%s",
			str.View())
	}
}

// THE TICK DOES NOT LITTER ON A SCREEN THAT IS NOT ASK. The Ask branch is chosen by tab, so a tick while
// another tab is active must not repaint a hidden conversation — it must reach that screen's own refresher
// (or nothing), exactly as before.
func TestRollingTickOnAnotherTabDoesNotRepaintTheTranscript(t *testing.T) {
	m := askRelaunched(t, "c1")
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "HIDDEN", Key: "t1", Live: true, At: 1})

	// Move to a tab with no refresher and no Ask screen: the tick must produce nothing.
	m.active = TabOverview
	if cmd := m.refreshActiveView(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.View(), "HIDDEN") {
		t.Error("a tick on another tab repainted the Ask transcript behind it")
	}
}
