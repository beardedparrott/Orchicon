package tui

// connection_loss_test.go — THE TUI TELLS YOU WHEN THE CONNECTION DIES.
//
// The operator: "If a connection dies, the gui tells you, but the TUI conversation does not. We need
// similar notifications."
//
// TWO SURFACES, because there were two gaps and only one of them was a rendering problem.
//
// THE FOOTER said "connected" on the Ask tab regardless of the plane's health. streamStatus() aggregated
// only over the statuses the ACTIVE SCREEN declares, and Ask declares NONE — its conversation list is the
// shell's rail and it subscribes to no stream of its own — so the worst-wins loop started and ended at
// "open". Measured before the fix: a dead plane reported "reconnecting" on the Work tab and "open" on
// Ask. The connection is not a property of the tab the operator happens to be looking at.
//
// THE CONVERSATION PANE said nothing at all, and the footer is easy to miss while reading a transcript —
// it is the TRANSCRIPT that looks broken when a reply cannot arrive. Its notice slot carried only
// "reconnecting…", which is set when a TURN loses its socket; a dead PLANE is a different failure and had
// no banner anywhere near the conversation.

import (
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/screens/work"
)

// deadPlane puts the registry into the state a lost connection produces: every subscription failing.
func deadPlane(m *App) {
	m.reg.ReportStatusForTest("project-events", "error")
	m.reg.ReportStatusForTest("execution-events", "reconnecting")
	m.reg.ReportStatusForTest("workflow-events", "closed")
}

// healthyPlane puts it back.
func healthyPlane(m *App) {
	m.reg.ReportStatusForTest("project-events", "open")
	m.reg.ReportStatusForTest("execution-events", "open")
	m.reg.ReportStatusForTest("workflow-events", "open")
}

// THE FOOTER REPORTS THE DEAD PLANE FROM ANY TAB — including the one with no streams of its own.
func TestFooterReportsADeadPlaneOnEveryTab(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.RegisterScreen(TabWork, work.New(m.clients, m.reg, ""))
	deadPlane(m)

	for _, tab := range []TabID{TabAsk, TabWork} {
		m.active = tab
		got := m.streamStatus()
		if got == openStatus {
			t.Errorf("on %s the footer reports %q with a DEAD plane — this is the operator's \"if a "+
				"connection dies, the GUI tells you, but the TUI conversation does not\"", tab, got)
		}
	}
}

// AND IT STILL SAYS CONNECTED WHEN THE PLANE IS HEALTHY — the fix must not turn a working session into an
// alarm. This is the half a naive "always warn" change gets wrong.
func TestFooterStillReportsConnectedWhenHealthy(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	healthyPlane(m)
	m.active = TabAsk

	if got := m.streamStatus(); got != openStatus {
		t.Errorf("a healthy plane reported %q, want %q — a connection banner on a working session is "+
			"noise the operator learns to ignore", got, openStatus)
	}
}

// A STREAM THAT HAS NOT REPORTED YET IS NOT A FAILURE. Startup arms subscriptions as tabs are visited, so
// an absent status must read as connected rather than flashing a banner before anything connects.
func TestUnreportedStreamsDoNotReadAsDisconnected(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.active = TabAsk
	if m.planeUnreachable() {
		t.Error("a session whose streams have never reported was treated as disconnected")
	}
	if got := m.streamStatus(); got == "" {
		t.Error("the footer has no status at all before anything reports")
	}
}

// A STREAM STILL DIALING IS NOT A DEAD PLANE either: a freshly-visited tab arms its streams as it opens,
// and "connecting" is what a healthy open looks like for a moment.
func TestConnectingIsNotTreatedAsDead(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.reg.ReportStatusForTest("project-events", "connecting")
	m.active = TabAsk
	if m.planeUnreachable() {
		t.Error("a subscription that is still CONNECTING was treated as a dead plane")
	}
}

// THE CONVERSATION PANE ITSELF SAYS THE CONNECTION IS DOWN.
//
// This is the operator's actual ask: the notification has to be where the conversation is, not only in the
// footer. Asserted on the transcript's notice — the one row the pane reserves for exactly this.
func TestConversationPaneWarnsWhenThePlaneIsUnreachable(t *testing.T) {
	m, _ := newAskApp(t)
	scr := ask.New(m.clients, m.reg)
	m.RegisterScreen(TabAsk, scr)
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	m.active = TabAsk

	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})
	healthyPlane(m)
	m.onChatWake()
	if str := m.TranscriptStream("c1"); str != nil && strings.Contains(str.Notice, "disconnected") {
		t.Fatalf("a HEALTHY plane showed a disconnection banner: %q", str.Notice)
	}

	deadPlane(m)
	m.onChatWake()
	str := m.TranscriptStream("c1")
	if str == nil {
		t.Fatal("no transcript stream")
	}
	if !strings.Contains(str.Notice, "disconnected") {
		t.Errorf("the conversation pane says %q with a dead plane — the operator's \"the TUI conversation "+
			"does not\" tell you", str.Notice)
	}
	// And the notice must be PAINTED, not merely stored: the failure this class of bug ships with is a
	// value set on the widget that the host's row budget clips away.
	if !strings.Contains(m.View(), "disconnected") {
		t.Errorf("the disconnection notice never reached the painted frame:\n%s", tailOf(m.View(), 800))
	}
}

// AND IT CLEARS WHEN THE CONNECTION RETURNS, without wiping an unrelated notice.
func TestConnectionBannerClearsOnRecovery(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.active = TabAsk
	m.OpenAskConversation("c1")
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})

	deadPlane(m)
	m.onChatWake()
	if !isConnBanner(m.dock.Notice) {
		t.Fatalf("the dock did not show a connection banner: %q", m.dock.Notice)
	}

	healthyPlane(m)
	m.onChatWake()
	if isConnBanner(m.dock.Notice) {
		t.Errorf("the connection banner outlived the connection: %q", m.dock.Notice)
	}
	if m.dock.Notice != "" {
		t.Errorf("recovery left %q in the strip, want it cleared — the banner owned that slot", m.dock.Notice)
	}
}

// THE BANNER OUTRANKS A TRANSIENT NOTICE WHILE THE PLANE IS DOWN, and the strip returns to EMPTY when it
// recovers rather than restoring a stale message.
//
// The one-line strip holds ONE thing, and the priority is deliberate: a send ack or a context-injection
// notice is informational, while "the connection is gone" is the reason nothing the operator does will
// work. So the banner takes the slot while it applies — and when the connection returns the slot is
// cleared, not re-filled with a notice from minutes ago that has long since stopped being true.
func TestConnectionBannerOutranksATransientNotice(t *testing.T) {
	m, _ := newAskApp(t)
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.active = TabAsk
	m.OpenAskConversation("c1")
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "hello", Key: "u1", At: 1})

	m.dock.SetNotice("context injected: [some file]")
	deadPlane(m)
	m.onChatWake()
	if !isConnBanner(m.dock.Notice) {
		t.Fatalf("the disconnected banner did not take the strip: %q", m.dock.Notice)
	}

	healthyPlane(m)
	m.onChatWake()
	if m.dock.Notice != "" {
		t.Errorf("the strip was left holding %q after recovery, want it empty", m.dock.Notice)
	}
}
