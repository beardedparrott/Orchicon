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
//
// THE PLANE MUST BE HEALTHY, and that is a real precondition rather than test plumbing: the notice slot has
// a PRECEDENCE, and "disconnected" outranks "thinking" — you cannot be waiting on a reply from a plane that
// is gone. The harness's registry reports dead by default (nothing has ever connected), so without this the
// pane correctly shows the connection banner and this test would be asserting the wrong thing. The
// precedence itself is pinned in TestThinkingYieldsToTheConnectionBanner below.
//
// IT ASSERTS THE NOTICE ON THE STREAM, not on the painted frame, and that is deliberate rather than weaker.
// The frame is one step downstream and depends on render timing: the pane is re-laid-out by the same fetch
// the tick triggers, so for exactly ONE frame the pane's body can be a row taller than its viewport and the
// notice — the last row — is clipped. Measured: after a fetch the pane's body was 22 rows while the stream
// had re-sized to 21, so the notice was in the stream, in the pane's body, and absent from that one frame.
// It is back on the next paint. Asserting the stream is asserting the FEATURE; asserting a particular frame
// would be asserting the renderer's timing, which is why the earlier version of this test passed and then
// failed the moment an unrelated change shifted the height arithmetic.
func TestRollingTickPaintsTheThinkingNotice(t *testing.T) {
	m := askRelaunched(t, "c1")
	healthyPlane(m)

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	_ = m.chat.Send("c1", "hello", "") // a turn is genuinely in flight; the Cmd is dropped

	if cmd := m.refreshActiveView(); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("the tick never created the transcript stream")
	}
	if !strings.Contains(str.Notice, "thinking") {
		t.Errorf("the tick did not set the thinking indicator — the operator's \"No 'Orchicon is thinking...' "+
			"block\". notice=%q", str.Notice)
	}
	// AND IT IS IN WHAT THE PANE IS HANDED TO PAINT: the stream's own render carries it, so a paint either
	// side of the layout change shows it.
	if !strings.Contains(str.View(), "thinking") {
		t.Errorf("the notice is set but missing from the pane's own body render:\n%s", str.View())
	}
}

// THE TWO NOTICES DO NOT FIGHT: a dead plane takes the slot, and the indicator comes back on recovery.
//
// This is the interaction between two features built in different rounds — the thinking indicator and the
// connection banner — and it is worth pinning because the naive implementations of each break the other:
// a banner with no precedence is overwritten by the indicator a moment later (so it flickers away), and an
// indicator with no banner check claims the model is thinking while the plane is unreachable (which is the
// "silent hang" the banner exists to explain).
func TestThinkingYieldsToTheConnectionBanner(t *testing.T) {
	m := askRelaunched(t, "c1")

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	_ = m.chat.Send("c1", "hello", "") // a turn is in flight throughout

	// Healthy first: the indicator is what the operator sees while waiting.
	healthyPlane(m)
	m.onChatWake()
	str := m.TranscriptStream("c1")
	if str == nil || !strings.Contains(str.Notice, "thinking") {
		t.Fatalf("with a healthy plane the notice should be the thinking indicator, got %q", str.Notice)
	}

	// The plane dies mid-turn: the banner takes the slot.
	deadPlane(m)
	m.onChatWake()
	str = m.TranscriptStream("c1")
	if !strings.Contains(str.Notice, "disconnected") {
		t.Errorf("a dead plane did not take the notice slot from the thinking indicator — the operator "+
			"would be told the model is thinking while no reply can arrive. notice=%q", str.Notice)
	}

	// And it recovers, rather than sticking like an alarm.
	healthyPlane(m)
	m.onChatWake()
	str = m.TranscriptStream("c1")
	if !strings.Contains(str.Notice, "thinking") {
		t.Errorf("the indicator did not return after recovery: notice=%q", str.Notice)
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
