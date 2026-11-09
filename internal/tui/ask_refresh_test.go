package tui

// ask_refresh_test.go — THE ROLLING WINDOW REPAINTS THE ASK TRANSCRIPT.

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

// THE LOST POKE IS NO LONGER PERMANENT.
func TestRollingTickRepaintsTheAskTranscriptWithoutAPoke(t *testing.T) {
	m := askRelaunched(t, "c1")

	// The durable transcript and a LIVE chunk arrive. Deliberately NO poke: this is the missed-wake case,
	// and the pane must recover on the timer alone.
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "LIVE-REPLY-TOKEN", Key: "t1", Live: true, At: 2})

	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.View(), "LIVE-REPLY-TOKEN") {
		t.Fatal("fixture: the pane already shows the chunk, so this test would measure nothing")
	}

	// ONE TURN OF THE WINDOW.
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

// THE PLANE MUST BE HEALTHY, and that is a real precondition rather than test plumbing: the notice
// slot has a PRECEDENCE, and "disconnected" outranks "thinking".
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
	line := m.askStatusLine()
	if !containsStr(line, "Orchicon is ") || !activityWordInList(line) {
		t.Errorf("the tick did not set the activity line, or set a word outside the reviewed rotation — the "+
			"operator's \"No 'Orchicon is thinking...' block\". footer=%q", line)
	}
	// AND IT IS IN WHAT THE OPERATOR ACTUALLY SEES.
	if frame := stripANSI(m.View()); !strings.Contains(frame, "Orchicon is ") {
		t.Errorf("the status line is set but missing from the painted frame:\n%s", tailOf(frame, 1200))
	}
}

// THE TWO NOTICES DO NOT FIGHT: a dead plane takes the slot, and the indicator comes back on
// recovery.
func TestThinkingYieldsToTheConnectionBanner(t *testing.T) {
	m := askRelaunched(t, "c1")

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	_ = m.chat.Send("c1", "hello", "") // a turn is in flight throughout

	// Healthy first: the indicator is what the operator sees while waiting.
	healthyPlane(m)
	m.onChatWake()
	m.onChatWake()
	if got := m.askStatusLine(); !containsStr(got, "Orchicon is ") || !activityWordInList(got) {
		t.Fatalf("with a healthy plane the status line should be the activity indicator (a reviewed "+
			"rotation word), got %q", got)
	}

	// The plane dies mid-turn: the banner takes the slot.
	deadPlane(m)
	m.onChatWake()
	if got := m.askStatusLine(); !strings.Contains(got, "disconnected") {
		t.Errorf("a dead plane did not take the status slot from the activity indicator — the operator "+
			"would be told the model is thinking while no reply can arrive. footer=%q", got)
	}

	// And it recovers, rather than sticking like an alarm. Same invariant as the healthy leg: the
	// activity line is back, and its word is one the rotation names.
	healthyPlane(m)
	m.onChatWake()
	if got := m.askStatusLine(); !containsStr(got, "Orchicon is ") || !activityWordInList(got) {
		t.Errorf("the indicator did not return after recovery: footer=%q", got)
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

// THE TICK DOES NOT LITTER ON A SCREEN THAT IS NOT ASK.
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
