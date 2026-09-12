package chat

import (
	"context"
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
)

// stub ask service: ChatStream acks then emits chunks then heartbeats;
// WatchTurnStream replays chunks; ListMessages returns the transcript.
type stubAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	streams   int
	watches   int
	watchFail bool
}

func (s *stubAsk) ListConversations(context.Context, *connect.Request[apiv1.ListConversationsRequest]) (*connect.Response[apiv1.ListConversationsResponse], error) {
	return connect.NewResponse(&apiv1.ListConversationsResponse{
		Conversations: []*apiv1.Conversation{{Id: "c1", Title: "one", MessageCount: 2}},
	}), nil
}

func (s *stubAsk) ListMessages(context.Context, *connect.Request[apiv1.ListMessagesRequest]) (*connect.Response[apiv1.ListMessagesResponse], error) {
	return connect.NewResponse(&apiv1.ListMessagesResponse{
		Messages: []*apiv1.ChatMessage{
			{Id: "m1", Role: "user", Content: "hi"},
			{Id: "m2", Role: "assistant", Content: "hello"},
		},
	}), nil
}

func (s *stubAsk) ChatStream(ctx context.Context, req *connect.Request[apiv1.ChatStreamRequest], st *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	s.streams++
	_ = st.Send(&apiv1.ChatStreamResponse{Event: &apiv1.ChatStreamResponse_TurnStarted{TurnStarted: &apiv1.TurnStarted{AssistantMessageId: "a1"}}})
	_ = st.Send(&apiv1.ChatStreamResponse{Event: &apiv1.ChatStreamResponse_TextChunk{TextChunk: &apiv1.TextChunk{Content: "wor"}}})
	_ = st.Send(&apiv1.ChatStreamResponse{Event: &apiv1.ChatStreamResponse_TextChunk{TextChunk: &apiv1.TextChunk{Content: "ld"}}})
	return nil
}

func (s *stubAsk) WatchTurnStream(ctx context.Context, req *connect.Request[apiv1.WatchTurnStreamRequest], st *connect.ServerStream[apiv1.ChatStreamResponse]) error {
	s.watches++
	if s.watchFail {
		return connect.NewError(connect.CodeNotFound, nil)
	}
	_ = st.Send(&apiv1.ChatStreamResponse{Event: &apiv1.ChatStreamResponse_Reasoning{Reasoning: &apiv1.ReasoningChunk{Content: "watched"}}})
	return nil
}

type recorder struct {
	mu    sync.Mutex
	items []ChatItem
	conn  map[string]bool
}

func (r *recorder) AppendLiveItem(convID string, item ChatItem) {
	r.mu.Lock()
	r.items = append(r.items, item)
	r.mu.Unlock()
}
func (r *recorder) SetReconnecting(convID string, on bool) {
	r.mu.Lock()
	r.conn[convID] = on
	r.mu.Unlock()
}

func newTestServer(t *testing.T, h apiv1connect.AskOrchiconServiceHandler) (*client.Clients, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(apiv1connect.NewAskOrchiconServiceHandler(h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.NewWithHTTPClient(client.Options{BaseURL: srv.URL}, srv.Client()), srv
}

func drainCmds(t *testing.T, ch chan tea.Cmd, n int) []tea.Cmd {
	t.Helper()
	out := make([]tea.Cmd, 0, n)
	for i := 0; i < n; i++ {
		select {
		case cmd := <-ch:
			out = append(out, cmd)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for cmd %d", i)
		}
	}
	return out
}

// Compaction must be refused while a turn is live: it rewrites the history the
// turn is generating from, so it would corrupt an answer in progress.
func TestCanCompactRefusesWhileStreaming(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	rec := &recorder{conn: map[string]bool{}}
	cmds := make(chan tea.Cmd, 16)
	c.Bind(rec, cmds)

	if reason := c.CanCompact(""); reason == "" {
		t.Error("no conversation must refuse with a reason")
	}
	if reason := c.CanCompact("c1"); reason != "" {
		t.Errorf("an idle conversation must be compactable, got refusal %q", reason)
	}

	// A real send puts the slot in flight.
	c.Send("c1", "hi", "")
	if !c.IsStreaming("c1") {
		t.Fatal("the slot must be streaming after a send")
	}
	reason := c.CanCompact("c1")
	if reason == "" {
		t.Fatal("an in-flight turn must refuse compaction")
	}
	if !strings.Contains(reason, "turn is in flight") || !strings.Contains(reason, "/compact") {
		t.Errorf("the refusal must name the cause and the command, got %q", reason)
	}
}

func TestSendStreamsAndAcks(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	rec := &recorder{conn: map[string]bool{}}
	cmds := make(chan tea.Cmd, 16)
	c.Bind(rec, cmds)

	// initial send: no in-flight turn → ChatStream
	if c.IsStreaming("c1") {
		t.Fatal("must start idle")
	}
	msg := c.Send("c1", "hi there", "[context: ask]")
	if got := msg(); got != nil {
		if _, ok := got.(ErrMsg); ok {
			t.Fatalf("send failed: %v", got)
		}
		t.Fatalf("unexpected msg %T", got)
	}
	if !c.IsStreaming("c1") {
		t.Fatal("slot must be streaming after send")
	}
	// wait for the goroutine to deliver chunks
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.items)
		rec.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.items) != 2 || rec.items[0].Text != "wor" || rec.items[1].Text != "ld" {
		t.Fatalf("items = %+v", rec.items)
	}
	c.mu.Lock()
	st := c.state["c1"]
	if st == nil || st.pendingReplyID != "a1" || st.optimisticUser != "" {
		t.Fatalf("state = %+v", st)
	}
	c.mu.Unlock()
	// second send while streaming → interject path
	_ = c.Send("c1", "redirect", "")
	if stub.streams != 1 {
		t.Fatalf("streams = %d, want 1 (second send must interject, not re-stream)", stub.streams)
	}
	_ = c
}

func TestWatchRedialOnDrop(t *testing.T) {
	stub := &stubAsk{watchFail: false}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	rec := &recorder{conn: map[string]bool{}}
	cmds := make(chan tea.Cmd, 16)
	c.Bind(rec, cmds)

	c.mu.Lock()
	c.state["c1"] = &convState{streaming: true, pendingReplyID: "a1"}
	c.mu.Unlock()

	c.dropStream("c1", context.Canceled)
	// The shell executes drained Cmds (tea programs run what Update
	// returns) — the watch re-dial must be invoked, not just received.
	for _, cmd := range drainCmds(t, cmds, 1) {
		_ = cmd()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		n := len(rec.items)
		rec.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.items) != 1 || rec.items[0].Kind != KindReasoning || rec.items[0].Text != "watched" {
		t.Fatalf("items = %+v", rec.items)
	}
	if stub.watches != 1 {
		t.Fatalf("watches = %d", stub.watches)
	}
}

func TestUnackedFailureTearsDown(t *testing.T) {
	// Connect streams fail lazily: the dial error surfaces on the first
	// Receive, not on the constructor — exercise dropStream directly.
	bad := client.NewWithHTTPClient(client.Options{BaseURL: "http://127.0.0.1:1"}, http.DefaultClient)
	c := NewController(bad)
	c.mu.Lock()
	c.state["c1"] = &convState{streaming: true} // not yet acked
	c.mu.Unlock()
	c.dropStream("c1", context.Canceled)
	c.mu.Lock()
	st := c.state["c1"]
	if st.streaming || st.optimisticUser != "" || st.sentText != "" {
		t.Fatalf("pre-ack drop must tear down the slot: %+v", st)
	}
	c.mu.Unlock()
	if got := c.LastError(); got == "" {
		t.Fatal("sticky error must be set")
	}
}

func TestOpenConversationTranscript(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	msg := c.OpenConversation("c1")
	tm, ok := msg().(TranscriptMsg)
	if !ok {
		t.Fatalf("want TranscriptMsg, got %T", msg())
	}
	if tm.Err != "" {
		t.Fatalf("transcript err: %s", tm.Err)
	}
	if len(tm.Items) != 2 || tm.Items[0].Kind != KindUser || tm.Items[1].Kind != KindText {
		t.Fatalf("items = %+v", tm.Items)
	}
}

func TestLoadConversations(t *testing.T) {
	stub := &stubAsk{}
	cl, _ := newTestServer(t, stub)
	c := NewController(cl)
	msg := c.LoadConversations()
	cm, ok := msg().(ConversationsMsg)
	if !ok {
		t.Fatalf("want ConversationsMsg, got %T", msg())
	}
	if cm.Err != "" || len(cm.Convs) != 1 || cm.Convs[0].ID != "c1" {
		t.Fatalf("convs = %+v err=%s", cm.Convs, cm.Err)
	}
}
