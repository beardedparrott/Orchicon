package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
)

func newWireApp() *App {
	m := NewApp(&client.Clients{}, &config.Profile{Name: "default", URL: "https://x.example.com"}, "v9.9.9")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, &stubScreen{id: "ask", chip: "Conversations: conv-1"})
	for _, tab := range []TabID{TabWork, TabExecution, TabAutomation, TabEnforcement, TabControl} {
		m.RegisterScreen(tab, &stubScreen{id: string(tab)})
	}
	m.SwitchTo(TabAsk)
	return m
}

// Live chunks flow through chatStore + the wake channel; the
// reconnecting banner surfaces on the tea loop (onChatWake).
func TestChatStoreRoundTripAndBanner(t *testing.T) {
	m := newWireApp()
	m.chatConvID = "c1"
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "chunk", Key: "st-1", Live: true})
	if got := len(m.chatStore.snapshot("c1")); got != 1 {
		t.Fatalf("snapshot = %d items, want 1", got)
	}
	m.chatStore.setReconnecting("c1", true)
	m.onChatWake()
	if !strings.Contains(m.dock.Notice, "re-attaching") {
		t.Fatalf("notice = %q, want reconnect banner", m.dock.Notice)
	}
	m.chatStore.setReconnecting("c1", false)
	m.onChatWake()
	if m.dock.Notice != "" {
		t.Fatalf("notice = %q, want cleared", m.dock.Notice)
	}
}

// StreamDone ends the turn and schedules the completion poll (which
// will REPLACE the live buffer — no duplicated reply after reconnects).
func TestOnStreamDoneEndsTurnAndPolls(t *testing.T) {
	m := newWireApp()
	m.chatConvID = "c1"
	m.chat.SetActive("c1")
	cmd := m.onStreamDone(chat.StreamDoneMsg{ConvID: "c1"})
	if cmd == nil {
		t.Fatal("onStreamDone must schedule the completion poll")
	}
	if m.chat.IsStreaming("c1") {
		t.Fatal("stream must not be streaming after StreamDone")
	}
}

// ctrl+c always escapes the composer (hard quit).
func TestCtrlCFromComposerQuits(t *testing.T) {
	m := newWireApp()
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m2 := nm.(*App)
	if m2.chatFocus != focusComposer {
		t.Fatal("precondition: composer focused")
	}
	nm, cmd := m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c while composing must return a quit cmd")
	}
	if !nm.(*App).quitting {
		t.Fatal("ctrl+c while composing must set quitting")
	}
}

// Enter in the focused composer appends the optimistic user row and
// dispatches the send.
func TestComposerSendAppendsOptimisticUserRow(t *testing.T) {
	m := newWireApp()
	m.chatConvID = "c1"
	m.chat.SetActive("c1")
	m.setFocus(focusComposer)
	m.dock.SetValue("hello from the dock")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	items := m.chatStore.snapshot("c1")
	if len(items) == 0 {
		t.Fatal("optimistic user row missing")
	}
	last := items[len(items)-1]
	if last.Kind != chat.KindUser || last.Text != "hello from the dock" {
		t.Fatalf("last item = %+v, want optimistic user row", last)
	}
}

// The transcript replaces (not merges with) the live buffer once the
// turn is over — the no-duplication-after-reconnect guarantee.
func TestTranscriptReplacesWhenIdleMergesWhenStreaming(t *testing.T) {
	m := newWireApp()
	m.chatConvID = "c1"
	m.chat.SetActive("c1")

	// Streaming: history merges under live chunks. (Send marks the turn
	// in flight without executing the stream Cmd — cmds are values.)
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "live", Key: "st-1", Live: true, Phase: "p-0"})
	_ = m.chat.Send("c1", "hi", "")
	m.Update(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{{Kind: chat.KindUser, Text: "hi", Key: "m-1"}}})
	if got := m.chatStore.snapshot("c1"); len(got) != 2 {
		t.Fatalf("streaming merge: %d items, want 2 (history+live)", len(got))
	}

	// Idle: the transcript replaces the live buffer.
	m.chat.EndStream("c1")
	m.Update(chat.TranscriptMsg{ConvID: "c1", Items: []chat.ChatItem{
		{Kind: chat.KindUser, Text: "hi", Key: "m-1"},
		{Kind: chat.KindText, Text: "reply", Key: "m-2"},
	}})
	if got := m.chatStore.snapshot("c1"); len(got) != 2 || got[1].Text != "reply" {
		t.Fatalf("idle replace: got %+v, want persisted transcript only", got)
	}
}
