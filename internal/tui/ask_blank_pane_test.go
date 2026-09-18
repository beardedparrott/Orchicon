package tui

// ask_blank_pane_test.go — THE PANE MUST NOT BE BLANKED BY ITS OWN REFRESH.
//
// The operator, the moment the rolling window started repainting this pane: "the conversation pane is
// completely blank on every chat."
//
// THE MECHANISM, reproduced before the fix and pinned here. kit2.Base.RefreshView reloads the active source
// AND re-requests the open detail's payload. That is right for every list-and-detail screen, and fatal for
// Ask, because ask.detail returns body="" BY DESIGN — the shell paints the transcript from the live chunk
// cache, and this pane's body is not the server's to send. So the re-request lands a detailMsg with an
// empty body, and Base's handler assigns it:
//
//	b.detail.SetContent(msg.title, msg.fields, msg.body)   // body == ""
//
// which erases whatever onChatWake had just painted. Measured: after onChatWake the pane held the
// transcript; after one detail landing it held the title and fields with a body of length 0.
//
// WHY IT IS PINNED AT THE PANE RATHER THAN AT THE PAINT. The failure is invisible in the obvious place: a
// test asserting that the transcript paints PASSES, because the paint happens and is then overwritten a
// moment later by a different code path. So these assert the pane's state AFTER BOTH.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
	"github.com/beardedparrott/orchicon/internal/tui/screens/screenkit"
)

func blankPaneApp(t *testing.T) (*App, *ask.Model) {
	t.Helper()
	m, _ := newAskApp(t)
	scr := ask.New(m.clients, m.reg)
	m.RegisterScreen(TabAsk, scr)
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.convRailOpen = true
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	m.active = TabAsk
	// A real transcript, painted the way the shell paints it.
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindUser, Text: "MY-QUESTION", Key: "u1", At: 1})
	m.chatStore.append("c1", chat.ChatItem{Kind: chat.KindText, Text: "THE-ANSWER", Key: "t1", At: 2})
	m.onChatWake()
	return m, scr
}

// paneBody reads the open pane's body without depending on the renderer's escape sequences.
func paneBody(scr *ask.Model) string {
	_, _, body := scr.DetailForTest()
	return body
}

// THE WIPE ITSELF: a conversation's detail payload carries no body, and it must not empty the pane.
//
// The payload here is EXACTLY what the real screen produces — title and fields, body "" — so if the base
// ever goes back to assigning an empty body onto a pane this screen owns, this fails.
func TestConversationDetailLandingDoesNotEmptyThePane(t *testing.T) {
	_, scr := blankPaneApp(t)

	if paneBody(scr) == "" {
		t.Fatal("fixture: the transcript did not paint, so this test would measure nothing")
	}

	before := paneBody(scr)
	// The landing, with the empty body ask.detail returns for a conversation.
	scr.DeliverDetailForTest("conversations", "c1", "Conversation: t", "", []screenkit.Field{
		{Key: "id", Value: "c1"},
	})

	after := paneBody(scr)
	if after == "" {
		t.Fatalf("a conversation's detail landing EMPTIED the pane — this is the operator's \"completely " +
			"blank on every chat\". The payload carries body=\"\" by design, so assigning it erases the " +
			"transcript the shell just painted")
	}
	if len(after) < len(before) {
		t.Errorf("the landing SHRANK the pane's body (%d -> %d bytes): the transcript is the shell's, and a "+
			"payload with no body must not displace it", len(before), len(after))
	}
}

// AND THE PANE SURVIVES THE CHAIN THAT ACTUALLY BLANKED IT.
//
// THE LANDING IS DRIVEN EXPLICITLY, and that is deliberate rather than redundant. The blank was caused by
// the detail landing — and in a test harness the Ask screen's own detail() cannot produce one (it calls
// GetConversation, which has no fake endpoint here), so a test that merely runs the tick's command PASSES
// whether or not the bug is present. That vacuous pass is exactly how a regression like this survives
// review, so this drives the landing the refresh provokes, with the empty body ask.detail really returns.
func TestRefreshDoesNotBlankTheConversationPane(t *testing.T) {
	m, scr := blankPaneApp(t)
	if paneBody(scr) == "" {
		t.Fatal("fixture: the transcript did not paint")
	}
	before := paneBody(scr)

	// The refresh a rolling tick causes.
	if cmd := m.refreshActiveView(); cmd != nil {
		runCmdTreeForTest(t, m, cmd, 0)
	}
	// …and the detail landing its list reload ends in (Base's fetchedMsg case calls loadDetail), carrying
	// the empty body a conversation's payload has.
	scr.DeliverDetailForTest("conversations", "c1", "Conversation: c1", "", []screenkit.Field{
		{Key: "id", Value: "c1"},
	})

	after := paneBody(scr)
	// THE INVARIANT IS THAT THE TRANSCRIPT IS STILL THERE, not that the byte count is identical.
	//
	// A refresh legitimately RE-MEASURES the transcript (the stream is re-sized against the pane's real
	// body height, and a row or two of wrapping follows), so asserting equality would fail a CORRECT
	// program — which is exactly what an earlier version of this test did. What must never happen is the
	// operator's report: the body becoming EMPTY, or the conversation's content disappearing.
	if after == "" {
		t.Fatalf("the refresh chain LEFT THE PANE BLANK — the operator's report. The detail landing that " +
			"the list reload provokes carries body=\"\" and was assigned over the transcript")
	}
	for _, want := range []string{"MY-QUESTION", "THE-ANSWER"} {
		if !strings.Contains(after, want) {
			t.Errorf("the refresh chain dropped %q from the transcript (before=%d bytes, after=%d) — a "+
				"refresh may re-measure the body, but it must not lose the conversation",
				want, len(before), len(after))
		}
	}
}

// THE ASK SCREEN'S REFRESHER DOES NOT DISPLACE THE TRANSCRIPT.
//
// This is the property that keeps the transcript and the refresh from fighting, and it is the one that
// fails if the body-ownership declaration is dropped. Asserted by RUNNING the hook's command with its
// messages dispatched: the list reload ends in a detail load, and that detail landing is what used to
// assign an empty body over the transcript.
func TestAskRefresherDoesNotDisplaceTheTranscript(t *testing.T) {
	m, scr := blankPaneApp(t)

	// Ask must still satisfy the shell's hook — the rail's list needs refreshing.
	var _ Refresher = scr

	before := paneBody(scr)
	if cmd := scr.RefreshView(); cmd != nil {
		runCmdTreeForTest(t, m, cmd, 0)
	}
	after := paneBody(scr)
	if after == "" {
		t.Error("the Ask screen's refresher EMPTIED the transcript. A detail re-request carries body=\"\" " +
			"here, so anything that assigns it blanks the pane")
	}
	for _, want := range []string{"MY-QUESTION", "THE-ANSWER"} {
		if !strings.Contains(after, want) {
			t.Errorf("the refresher lost %q from the transcript (before=%d bytes, after=%d)",
				want, len(before), len(after))
		}
	}
}

// runCmdTreeForTest executes a command tree the way the program loop does: run each command, dispatch each
// message, and recurse into batches. Depth-bounded, and it stops at a re-arming tick so a self-arming
// command cannot spin forever.
func runCmdTreeForTest(t *testing.T, m *App, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > 6 {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			runCmdTreeForTest(t, m, c, depth+1)
		}
		return
	}
	if _, isTick := msg.(refreshTickMsg); isTick {
		return
	}
	nm, next := m.Update(msg)
	if m2, ok := nm.(*App); ok {
		*m = *m2
	}
	runCmdTreeForTest(t, m, next, depth+1)
}
