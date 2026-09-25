package chat

// turn_gate_send_test.go — A SEND THE SERVER REFUSES BECAUSE A TURN IS RUNNING IS DELIVERED, NOT BOUNCED.
//
// The service allows ONE turn per conversation and answers a second ChatStream with FailedPrecondition
// ("a reply is still in progress for this conversation — wait for it to complete or stop it first":
// internal/askorchicon/chat.go, turnRegistry.register) — and it names the remedy in the same breath: the
// client interjects (supersedes) instead. That is already this client's documented rule ("sending while a
// reply streams interjects (supersedes the turn) instead" — internal/tui/help.go) and the whole point of
// the `interject` decision in SendWithAttachments.
//
// That decision is GUESSED FROM THE CLIENT'S OWN SLOT (`interject := st.streaming`), and the slot goes
// stale across a leave-and-return — the re-attach reads the rail row (app.reattachRunningTurn), so a row
// that predates the turn leaves the slot saying "idle" while the server says "busy". The send is then
// refused with nothing on the wire, and the operator: "if I leave a conversation in the TUI and come back,
// it doesn't actually send my message and I have to send it twice."
//
// The refusal is diagnosed in dropStream (it arrives ON the server-streaming call, not at the call site),
// where the turn is re-issued on the supersede path — once — and a failure the supersede path cannot fix
// is REPORTED instead of retried.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	tea "github.com/charmbracelet/bubbletea"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
)

// turnGateAsk models the server's one-turn-per-conversation gate.
type turnGateAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler

	mu       sync.Mutex
	inFlight map[string]bool
	// sends and interjects record what each path RECEIVED, so a test can tell "the message reached the
	// server" from "the client only thought it did".
	sends     []string
	interject []string
}

func newTurnGateAsk() *turnGateAsk {
	return &turnGateAsk{inFlight: map[string]bool{}}
}

func (s *turnGateAsk) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	return connect.NewResponse(&apiv1.ListConversationsResponse{
		Conversations: []*apiv1.Conversation{
			// TurnInFlight is FALSE on purpose: this is the STALE RAIL ROW the shell re-attaches from
			// (app.reattachRunningTurn), so the client's slot stays idle while the server is busy.
			{Id: "c1", Title: "Alpha", MessageCount: 1},
		},
	}), nil
}

func (s *turnGateAsk) ChatStream(_ context.Context, req *connect.Request[apiv1.ChatStreamRequest], _ *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	s.mu.Lock()
	busy := s.inFlight[req.Msg.GetConversationId()]
	s.sends = append(s.sends, req.Msg.GetMessage())
	s.mu.Unlock()
	if busy {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("a reply is still in progress for this conversation — wait for it to complete or stop it first"))
	}
	return nil
}

func (s *turnGateAsk) InterjectConversationTurn(_ context.Context, req *connect.Request[apiv1.InterjectConversationTurnRequest], _ *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	s.mu.Lock()
	// The supersede path cancels the running turn, exactly as the server does before dispatching.
	delete(s.inFlight, req.Msg.GetConversationId())
	s.interject = append(s.interject, req.Msg.GetMessage())
	s.mu.Unlock()
	return nil
}

func (s *turnGateAsk) counts() (sends, interjects []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.sends...), append([]string{}, s.interject...)
}

func (s *turnGateAsk) setInFlight(convID string) {
	s.mu.Lock()
	s.inFlight[convID] = true
	s.mu.Unlock()
}

// runPostedChatCommands pumps the controller's command channel the way the SHELL does (chatCmdMsg →
// Update), until stop() reports the outcome or the budget runs out. It returns every message the posted
// commands produced.
func runPostedChatCommands(t *testing.T, cmds chan tea.Cmd, stop func() bool) []tea.Msg {
	t.Helper()
	var out []tea.Msg
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !stop() {
		select {
		case c := <-cmds:
			if c != nil {
				if msg := c(); msg != nil {
					out = append(out, msg)
				}
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	return out
}

func TestSendRefusedByTheTurnGateIsDeliveredAsAnInterjection(t *testing.T) {
	stub := newTurnGateAsk()
	stub.setInFlight("c1") // the server is running a reply; the client's slot does not know it
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	cmds := make(chan tea.Cmd, 16)
	c.Bind(nil, cmds)

	// A send whose Cmd RUNS: this is the operator pressing Enter.
	cmd := c.Send("c1", "the message that must not vanish", "")
	if cmd == nil {
		t.Fatal("a send must produce a command")
	}
	if msg := cmd(); msg != nil {
		if e, ok := msg.(ErrMsg); ok {
			t.Fatalf("the send bounced with %v at the transport — the message never reached the server", e.Err)
		}
	}

	// The refusal is diagnosed on the STREAM (a server-streaming call reports the server's error through
	// the stream, not from the call), so the re-issue arrives on the command channel the shell drains.
	runPostedChatCommands(t, cmds, func() bool {
		_, interjects := stub.counts()
		return len(interjects) > 0
	})

	sends, interjects := stub.counts()
	if len(sends) != 1 {
		t.Fatalf("ChatStream attempts = %d, want 1 (the refused send)", len(sends))
	}
	if len(interjects) != 1 {
		t.Fatalf("InterjectConversationTurn calls = %d, want exactly 1 — the refused send must be "+
			"re-issued on the supersede path the TUI documents (\"sending while a reply streams "+
			"interjects\")", len(interjects))
	}
	if interjects[0] != "the message that must not vanish" {
		t.Fatalf("the supersede carried %q, want the operator's message verbatim", interjects[0])
	}
	if !c.IsStreaming("c1") {
		t.Error("the slot must stay in flight after a delivered send — the retry owns it")
	}
}

// A REFUSAL THIS FIX CANNOT CURE MUST STILL FAIL LOUDLY. The retry can itself fail (an unresolvable
// adapter also answers FailedPrecondition), and then the operator is TOLD — the shell's dock error strip
// and the draft put back in the composer, the same handling every failed send gets — rather than the turn
// vanishing or the retry looping.
func TestSendThatCannotBeDeliveredReportsTheFailure(t *testing.T) {
	stub := &failingInterjectAsk{turnGateAsk: newTurnGateAsk()}
	stub.setInFlight("c1")
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	cmds := make(chan tea.Cmd, 16)
	c.Bind(nil, cmds)

	c.Send("c1", "still undeliverable", "")()

	msgs := runPostedChatCommands(t, cmds, func() bool {
		sends, _ := stub.counts()
		return len(sends) > 0 && stub.interjectCalls() > 0 && !c.IsStreaming("c1")
	})

	var reported bool
	for _, msg := range msgs {
		if _, ok := msg.(ErrMsg); ok {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("a send whose supersede retry also fails must report an error for the dock, got %#v", msgs)
	}
	if c.IsStreaming("c1") {
		t.Error("a turn that no path could deliver must not leave the slot streaming")
	}
	// ONE RETRY, NEVER A LOOP: the supersede path was tried exactly once.
	if n := stub.interjectCalls(); n != 1 {
		t.Errorf("InterjectConversationTurn calls = %d, want exactly 1 — the re-issue must not loop", n)
	}
}

// failingInterjectAsk refuses BOTH paths, so the retry's own failure is what surfaces.
type failingInterjectAsk struct {
	*turnGateAsk
	mu    sync.Mutex
	calls int
}

func (s *failingInterjectAsk) InterjectConversationTurn(context.Context, *connect.Request[apiv1.InterjectConversationTurnRequest], *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("Ask Orchicon could not resolve an adapter for this conversation"))
}

func (s *failingInterjectAsk) interjectCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
