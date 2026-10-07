package tui

// ask_turn_notice_test.go — the activity line ("Orchicon is thinking…", and its watchdog age) must show
// whenever a turn is IN FLIGHT, not only while THIS client happens to hold a live stream for it.
//
// The operator: "I am no longer seeing the 'Orchicon is thinking...' and the watchdog countdown in the
// TUI. The conversation text is going to the bottom."
//
// BOTH SYMPTOMS, ONE CAUSE. The notice was keyed to the client's own stream slot alone
// (chat.IsStreaming), while a turn's real state lives on the PLANE and reaches the shell on the
// conversation row (Conversation.TurnInFly, from turn_in_flight) — the same field the rail already uses
// to mark the row as running (rightrail.go) and the same one re-attach reads (reattachRunningTurn). So
// whenever the slot was not armed in THIS client — a turn started in the GUI, a re-attach that ran
// before the row landed, a slot cleared by a superseded stream — the pane showed no line at all while
// the rail beside it said the conversation was running.
//
// "THE TEXT IS GOING TO THE BOTTOM" IS THE SAME BUG, SEEN FROM THE OTHER SIDE. The notice takes a row
// from the body (kit2.Stream.bodyRows), so an EMPTY notice hands the transcript the whole pane: the text
// runs one row further down, to the pane's bottom edge — which is where the operator saw it land.

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/beardedparrott/orchicon/internal/tui/chat"
)

// THE LINE SHOWS FOR A TURN THE SERVER REPORTS, even when this client streams nothing.
func TestActivityLineShowsForAServerReportedTurn(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	// The server says a turn is in flight (the rail row carries it, and the rail marks it). This client
	// holds NO live stream slot — never sent, or the slot was lost and not re-attached.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: this client must hold no stream slot for the turn")
	}
	m.onChatWake()

	if got := m.askStatusLine(); !strings.Contains(got, "Orchicon is") {
		t.Errorf("no activity line while the PLANE reports a turn in flight (footer = %q). The rail "+
			"marks this conversation as running, so the pane and the rail contradict each other — and "+
			"an empty status hands the transcript the footer's rows, which is the operator's "+
			"\"the conversation text is going to the bottom\"", got)
	}
	if frame := m.View(); !strings.Contains(frame, "Orchicon is") {
		t.Errorf("the activity line is not in the PAINTED frame:\n%s", tailOf(frame, 1200))
	}
}

// AND IT GOES AWAY WHEN THE PLANE SAYS THE TURN IS OVER — the fallback must not become a permanent
// claim that something is happening, which would be worse than the silence it fixes.
func TestActivityLineClearsWhenTheTurnEnds(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()
	if m.askStatusLine() == "" {
		t.Fatal("fixture: the line should be showing")
	}

	// The turn finishes: the conversation row no longer reports it.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.onChatWake()
	if got := m.askStatusLine(); got != "" {
		t.Errorf("the activity line outlived the turn it describes: footer = %q", got)
	}
}

// THE LINE'S ROWS ARE TAKEN OUT OF THE BODY, so the transcript can never run to the pane's bottom
// edge and the line can never be squeezed out.
//
// This is the whole reason the line is a PANE FOOTER rather than a stream notice. As a notice it claimed
// a body row INSIDE the stream (kit2.Stream.bodyRows), which meant the transcript got the whole pane
// whenever the notice was empty — the operator's "the model's text is reach the bottom of the
// conversation pane and this should never happen" — and the notice was drawn last, so any sizing
// mistake clipped it. As a footer the PANE budgets it (screenkit.Detail.footerRows) and subtracts it
// BEFORE the stream is measured, which is asserted here from both directions.
func TestTheStatusLineIsBudgetedOutOfTheBody(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	for i := 0; i < 60; i++ {
		m.chatStore.append("c1", chat.ChatItem{
			Kind: chat.KindUser, Text: fmt.Sprintf("history-%02d", i), Key: fmt.Sprintf("h%d", i), At: int64(i),
		})
	}

	// A turn in flight: the line is up, and the body is smaller than the pane by the footer's rows.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()
	if m.askStatusLine() == "" {
		t.Fatal("fixture: the status line must be showing")
	}
	withLine := m.TranscriptStream("c1").Height

	// The turn ends: the footer clears and the body gets those rows back.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.onChatWake()
	if m.askStatusLine() != "" {
		t.Fatalf("the status line outlived the turn: %q", m.askStatusLine())
	}
	without := m.TranscriptStream("c1").Height
	if without <= withLine {
		t.Errorf("the footer's rows were not returned to the body: %d with the line up, %d without",
			withLine, without)
	}

	// AND THE OPERATOR'S INVARIANT, on the painted frame: with a turn running, the line is on screen AND
	// the transcript has not reached the pane's last row.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}
	m.onChatWake()
	frame := stripANSI(m.View())
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("the status line is not in the painted frame:\n%s", tailOf(frame, 1200))
	}
	rows := strings.Split(frame, "\n")
	bottom := -1
	for i, r := range rows {
		if strings.Contains(r, "└") {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		t.Fatal("could not find the pane's bottom border")
	}
	if last := rows[bottom-1]; !strings.Contains(last, "│") || strings.Contains(last, "history-") {
		t.Errorf("the pane's last row is not the status band: %q", strings.TrimRight(last, " "))
	}
}

// SENDING PUTS BOTH THE ECHO AND THE ACTIVITY LINE IN THE FRAME, immediately — the operator's "the use
// message doesn't append at the bottom right away anymore".
//
// Driven through the REAL funnel (sendFromComposer and the commands it returns, applied the way the
// bubbletea runtime applies them), not by appending to the store and calling onChatWake by hand. That
// distinction is the whole test: a funnel that appends but forgets to WAKE renders the message only when
// some later event happens to repaint, which reads to the operator as a lag — and a hand-wired test
// cannot see it.
func TestSendingAppendsTheEchoAndTheLineImmediately(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")

	cmd := m.sendFromComposer("hello from the funnel")
	if cmd == nil {
		t.Fatal("sendFromComposer produced no command")
	}
	// Apply the returned commands as the runtime does. The RPC inside is best-effort here (the stub
	// plane answers nothing for it); what matters is the wake.
	applyCmds(m, cmd)

	frame := stripANSI(m.View())
	if !strings.Contains(frame, "hello from the funnel") {
		t.Errorf("the operator's own message is not in the painted frame straight after sending — the "+
			"echo was written but nothing repainted, which is the reported lag:\n%s", tailOf(frame, 1200))
	}
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("no activity line in the painted frame straight after sending:\n%s", tailOf(frame, 1200))
	}
}

// applyCmds evaluates a (possibly batched) command and feeds every message it produces back into the
// shell, which is what the runtime does with a returned cmd.
func applyCmds(m *App, cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		switch msg := next().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case chatWakeMsg:
			// The shell's own repaint poke. Feed it through the router's handler so the pane is rebuilt
			// exactly as it is in the running program.
			_, c := m.Update(msg)
			if c != nil {
				queue = append(queue, c)
			}
		}
	}
}

// A DRIFTED DETAIL ID MUST NOT SILENCE THE PANE.
//
// The guard in onChatWake (app.go) used to return nil whenever the Ask pane's detail id did not equal
// the open conversation — painting NOTHING: no transcript update and no activity line, together, until
// something incidental restored the id. The operator's report is exactly that shape: "I have to click
// away and back again to see updates. No 'Orchicon is thinking...' block."
//
// The id does drift: kit2.Base stamps it on EVERY detail landing for a source it owns, and the rail's
// row selection loads that row's detail — so a rail reload (the tick does one every 5s) can stamp the
// id of whichever row the cursor is on, which need not be the open conversation. The Ask screen has ONE
// source and a HOST-OWNED pane, so the pane is always the open conversation's transcript and the shell
// re-asserts that rather than refusing to paint.
func TestADriftedDetailIDDoesNotSilenceThePane(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}

	// The drift: the screen's detail id now names a DIFFERENT row (a rail row the cursor landed on).
	s, ok := m.screens[TabAsk].(interface{ SetDetailID(string) })
	if !ok {
		t.Fatal("fixture: the Ask screen no longer exposes SetDetailID")
	}
	s.SetDetailID("01SOMEOTHERROW")

	// A live wake arrives — the thing that must repaint the transcript and its activity line.
	m.Update(chatWakeMsg{})

	frame := stripANSI(m.View())
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("a drifted detail id silenced the pane — no activity line:\n%s", tailOf(frame, 1200))
	}
	// And the id is corrected, so every later consumer agrees the pane shows this conversation.
	if got := m.detailIDOfAskScreen(); got != "c1" {
		t.Errorf("the detail id was not re-asserted: got %q, want the open conversation", got)
	}
}

// detailIDOfAskScreen reads the Ask pane's detail id through the same interface the shell uses.
func (m *App) detailIDOfAskScreen() string {
	if dr, ok := m.screens[TabAsk].(interface{ DetailID() string }); ok {
		return dr.DetailID()
	}
	return ""
}

// THE OPERATOR'S OWN SEQUENCE, end to end: the echo appears immediately with the line, the line survives
// EVERY kind of chunk the model produces, and it clears only when the turn is truly done.
//
// The operator's spec, verbatim: "A typical conversation should go: User sends message, message shows up
// on the right side immediately, Orchicon is thinking... and a time of the last message from the model
// (which resets automatically) should show up in the bottom left corner of the conversation pane. The
// model's response should show up on the left (reasoning, thinking, and actual prose/work) ... The status
// at the bottom indicating that actual thinking is occurring and how long since the last thought should
// NEVER go away unless a turn is truly done."
//
// THE CHUNK LOOP IS THE POINT. The report this guards against is "After the initial 'Orchicon is
// thinking...', streaming started and the 'Orchicon is thinking...' went away and never came back" — so a
// single assertion after one chunk would not catch it. Each kind is streamed in turn, and the line is
// checked after every one: reasoning, prose, a tool call, and prose again.
func TestTheStatusLineSurvivesTheWholeTurn(t *testing.T) {
	m, _ := askWithTranscript(t, "c1")
	for i := 0; i < 40; i++ {
		m.chatStore.append("c1", chat.ChatItem{
			Kind: chat.KindUser, Text: fmt.Sprintf("old-%02d", i), Key: fmt.Sprintf("h%d", i), At: int64(i),
		})
	}
	// The PLANE's view of the turn, which is what a real session has (verified against the live plane) and
	// what the status fetch keeps fresh.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat", TurnInFly: true, PendingReplyID: "m1"}}

	// 1. SEND: the echo is on screen immediately, with the line under it.
	applyCmds(m, m.sendFromComposer("MY NEW MESSAGE"))
	frame := stripANSI(m.View())
	if !strings.Contains(frame, "MY NEW MESSAGE") {
		t.Errorf("the operator's own message is not on screen straight after sending:\n%s", tailOf(frame, 1200))
	}
	if !strings.Contains(frame, "Orchicon is") {
		t.Errorf("no status line straight after sending:\n%s", tailOf(frame, 1200))
	}

	// 2. THE TURN'S CHUNKS, one kind at a time.
	chunks := []chat.ChatItem{
		{Kind: chat.KindReasoning, Text: "thinking about it", Key: "r1", Live: true, At: 2},
		{Kind: chat.KindText, Text: "Here is my answer as it streams in.", Key: "m2", Live: true, At: 5},
		{Kind: chat.KindTool, Key: "t1", Tool: &chat.ParsedTool{ID: "t1", ToolName: "bash", Output: "ok"}, At: 8},
		{Kind: chat.KindText, Text: "And more prose after the tool.", Key: "m3", Live: true, At: 9},
	}
	for i, c := range chunks {
		m.chatStore.append("c1", c)
		m.onChatWake()
		if got := stripANSI(m.View()); !strings.Contains(got, "Orchicon is") {
			t.Fatalf("the status line went away mid-turn after chunk %d (%v) — the operator's "+
				"\"streaming started and the 'Orchicon is thinking...' went away and never came back\":"+
				"\n%s", i+1, c.Kind, tailOf(got, 1200))
		}
	}

	// 3. THE TURN IS DONE, and only then does it clear.
	m.conversations = []chat.Conversation{{ID: "c1", Title: "a chat"}}
	m.onChatWake()
	if got := stripANSI(m.View()); strings.Contains(got, "Orchicon is") {
		t.Errorf("the status line outlived the turn — a line that never clears is as untrue as one that "+
			"never shows:\n%s", tailOf(got, 1200))
	}
}
