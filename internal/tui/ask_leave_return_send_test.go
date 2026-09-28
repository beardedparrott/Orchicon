package tui

// ask_leave_return_send_test.go — THE LEAVE-AND-RETURN SEND.
//
// The operator: "There is also a weird lingering issue where if I leave a conversation in the TUI and
// come back, it doesn't actually send my message and I have to send it twice."
//
// THE MESSAGE IS DISPATCHED AND THE SERVER REFUSES IT — which is why the operator sees it "not send"
// and has to press Enter again. The service allows ONE turn per conversation and answers a second
// ChatStream with FailedPrecondition ("a reply is still in progress for this conversation — wait for
// it to complete or stop it first": internal/askorchicon/chat.go, turnRegistry.register). The TUI
// decides between ChatStream and the supersede RPC from ITS OWN slot — `interject := st.streaming`
// (internal/tui/chat/controller.go, SendWithAttachments) — and that slot is exactly what a
// leave-and-return loses and only re-syncs from the RAIL row (app.reattachRunningTurn; the operator's
// earlier report, quoted in OpenAskConversation: "When I leave an chat and go back into it, it loses
// the 'orchicon is thinking...' and watchdog"). A rail row that predates the turn therefore leaves the
// client saying "idle" while the server is busy — and the operator's next message is bounced, with the
// draft put back in the composer. Pressing Enter again (by which time the turn has moved on) sends it.
//
// The test drives the REAL keys for the leave, the return, the focus and the send, and asserts on what
// the SERVER received. It fails before the fix with "the server received no turn at all".

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	"github.com/beardedparrott/orchicon/internal/tui/client"
	"github.com/beardedparrott/orchicon/internal/tui/config"
	"github.com/beardedparrott/orchicon/internal/tui/screens/ask"
)

// busyTurnPlane serves a conversation whose reply is STILL RUNNING server-side while its rail row
// reports no turn in flight. That pair is the desynchronised state a leave-and-return leaves behind:
// the shell re-attaches from the row (reattachRunningTurn), finds nothing to re-attach to, and
// believes the conversation is idle.
type busyTurnPlane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	mu        sync.Mutex
	delivered []string // every message that actually reached the server as a turn
	refused   int      // sends the one-turn gate bounced
}

func (p *busyTurnPlane) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	return connect.NewResponse(&apiv1.ListConversationsResponse{
		Conversations: []*apiv1.Conversation{
			// turn_in_flight is FALSE — the stale row the shell depends on.
			{Id: "c1", Title: "Alpha", MessageCount: 1},
		},
	}), nil
}

func (p *busyTurnPlane) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	return connect.NewResponse(&apiv1.ListMessagesResponse{}), nil
}

func (p *busyTurnPlane) GetConversation(_ context.Context, req *connect.Request[apiv1.GetConversationRequest]) (*connect.Response[apiv1.GetConversationResponse], error) {
	return connect.NewResponse(&apiv1.GetConversationResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId(), Title: "Alpha"},
	}), nil
}

// ChatStream is the gate: while the conversation's reply is running, a second turn is refused with
// FailedPrecondition — the server's own code and wording.
func (p *busyTurnPlane) ChatStream(context.Context, *connect.Request[apiv1.ChatStreamRequest], *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	p.mu.Lock()
	p.refused++
	p.mu.Unlock()
	return connect.NewError(connect.CodeFailedPrecondition,
		errors.New("a reply is still in progress for this conversation — wait for it to complete or stop it first"))
}

// InterjectConversationTurn is the supersede path — how a message is delivered while a reply runs
// (the documented rule in the composer's own help: "sending while a reply streams interjects
// (supersedes the turn) instead"). It records what the server actually received.
func (p *busyTurnPlane) InterjectConversationTurn(_ context.Context, req *connect.Request[apiv1.InterjectConversationTurnRequest], _ *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	p.mu.Lock()
	p.delivered = append(p.delivered, req.Msg.GetMessage())
	p.mu.Unlock()
	return nil
}

func (p *busyTurnPlane) deliveredMessages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.delivered...)
}

func (p *busyTurnPlane) refusals() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

// leaveAndReturnApp builds a shell on the Ask tab with c1 open, wired to the busy-reply plane.
func leaveAndReturnApp(t *testing.T, h apiv1connect.AskOrchiconServiceHandler) *App {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cl := client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client())
	m := NewApp(cl, &config.Profile{Name: "default", URL: srv.URL}, "v0")
	m.width, m.height = 120, 40
	m.RegisterScreen(TabAsk, ask.New(m.clients, m.reg))
	m.SwitchTo(TabAsk)
	m.askMode = askConversations
	m.rightRailOpen = true
	m.OpenAskConversation("c1")
	return m
}

// THE REPORTED SYMPTOM: leave the conversation, come back, ONE Enter must deliver the message.
func TestSendAfterLeavingAndReturningDispatchesOnce(t *testing.T) {
	plane := &busyTurnPlane{}
	m := leaveAndReturnApp(t, plane)
	if m.chatConvID != "c1" {
		t.Fatalf("fixture: chatConvID = %q, want c1", m.chatConvID)
	}
	// THE DESYNCHRONISATION THIS TEST IS ABOUT, asserted rather than assumed: the shell believes the
	// conversation is idle, so it will send with a plain ChatStream — which the server refuses.
	if m.chat.IsStreaming("c1") {
		t.Fatal("fixture: the client must NOT think the conversation is streaming — the stale turn is " +
			"exactly what a leave-and-return fails to re-attach")
	}

	// LEAVE: another area (the tab chord every operator uses).
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m = nm.(*App)
	m = runCmd(t, m, cmd)
	if m.active != TabWork {
		t.Fatalf("fixture: F3 did not leave the Ask tab (active=%v)", m.active)
	}
	// COME BACK: the Ask tab chord again — a real key, landing on the tab bar with Ask's submenu down.
	nm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyF1})
	m = nm.(*App)
	m = runCmd(t, m, cmd)
	if m.active != TabAsk || m.chatConvID != "c1" {
		t.Fatalf("fixture: F1 did not return to c1 (active=%v conv=%q)", m.active, m.chatConvID)
	}
	// BACK TO TYPING — the chord the footer and /help both name for exactly this round trip.
	nm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	m = nm.(*App)
	m = runCmd(t, m, cmd)
	if m.chatFocus != focusComposer {
		t.Fatalf("fixture: ctrl+g did not focus the composer (chatFocus=%v)", m.chatFocus)
	}

	m = typeInto(t, m, "the message that must not vanish")
	if got := m.dock.Value(); got != "the message that must not vanish" {
		t.Fatalf("fixture: composer = %q, want the typed message", got)
	}

	// THE SEND: a real Enter through the shell.
	nm, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	if cmd == nil {
		t.Fatal("enter produced no command at all")
	}
	m = runCmd(t, m, cmd)

	// THE RE-ISSUE ARRIVES FROM THE STREAM'S OWN GOROUTINE, exactly as the runtime delivers it: the
	// controller posts it on the shell's chat-command channel and the shell runs it (chatCmdMsg →
	// Update). Pump that channel the way the runtime does until the server has received the turn.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(plane.deliveredMessages()) == 0 {
		select {
		case c := <-m.chatCmds:
			m = runCmd(t, m, c)
		case <-time.After(20 * time.Millisecond):
		}
	}

	got := plane.deliveredMessages()
	if len(got) != 1 || got[0] != "the message that must not vanish" {
		t.Fatalf("the server received %q (%d refusal(s) by the one-turn gate), want exactly one turn "+
			"carrying the operator's message — this is the operator's \"it doesn't actually send my "+
			"message and I have to send it twice\": the send was refused and never re-issued",
			got, plane.refusals())
	}
	if plane.refusals() == 0 {
		t.Error("the fixture never exercised the gate: no send was refused, so this test would pass for " +
			"the wrong reason")
	}
	// AND THE OPERATOR CAN SEE IT: the echoed turn is in the transcript and the box let it go.
	if !strings.Contains(transcriptText(m), "the message that must not vanish") {
		t.Errorf("the message is not in the transcript store after a delivered send: %+v", m.chatStore.snapshot("c1"))
	}
	if m.dock.Value() != "" {
		t.Errorf("the composer still holds %q after a delivered send", m.dock.Value())
	}
}

// THE ADJACENT BEHAVIOUR STILL HOLDS: an ordinary send in an idle conversation is a plain ChatStream
// and is NOT turned into an interjection — the retry is scoped to the gate's refusal.
func TestAnIdleConversationStillSendsWithChatStream(t *testing.T) {
	plane := &idlePlane{}
	m := leaveAndReturnApp(t, plane)
	m.setFocus(focusComposer)
	m = typeInto(t, m, "plain message")

	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*App)
	m = runCmd(t, m, cmd)

	if len(plane.streams) != 1 {
		t.Fatalf("ChatStream calls = %d, want 1 for an idle conversation", len(plane.streams))
	}
	if len(plane.interjects) != 0 {
		t.Fatalf("the idle path must NOT interject (got %v)", plane.interjects)
	}
}

// idlePlane accepts a turn normally and records which path it came in on.
type idlePlane struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	streams    []string
	interjects []string
}

func (p *idlePlane) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	return connect.NewResponse(&apiv1.ListConversationsResponse{
		Conversations: []*apiv1.Conversation{{Id: "c1", Title: "Alpha"}},
	}), nil
}

func (p *idlePlane) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	return connect.NewResponse(&apiv1.ListMessagesResponse{}), nil
}

func (p *idlePlane) GetConversation(_ context.Context, req *connect.Request[apiv1.GetConversationRequest]) (*connect.Response[apiv1.GetConversationResponse], error) {
	return connect.NewResponse(&apiv1.GetConversationResponse{
		Conversation: &apiv1.Conversation{Id: req.Msg.GetId(), Title: "Alpha"},
	}), nil
}

func (p *idlePlane) ChatStream(_ context.Context, req *connect.Request[apiv1.ChatStreamRequest], _ *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	p.streams = append(p.streams, req.Msg.GetMessage())
	return nil
}

func (p *idlePlane) InterjectConversationTurn(_ context.Context, req *connect.Request[apiv1.InterjectConversationTurnRequest], _ *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	p.interjects = append(p.interjects, req.Msg.GetMessage())
	return nil
}

// transcriptText flattens the conversation's stored items for a readable assertion.
func transcriptText(m *App) string {
	var b strings.Builder
	for _, it := range m.chatStore.snapshot("c1") {
		b.WriteString(it.Text)
		b.WriteString("\n")
	}
	return b.String()
}
