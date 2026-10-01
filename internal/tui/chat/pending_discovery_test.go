package chat

// pending_discovery_test.go — a card can be DISCOVERED, not only delivered.
//
// The operator: "you sent numerous permission card requests and user ask card requests. ALL
// of them reached the GUI conversation just fine, however, they did not all reach the TUI…
// it appeared you were stalled. So I went into the GUI and lo and behold, a permissions card
// was waiting for me to click on it."
//
// The TUI learned about an ask ONLY through consume(), which runs for a conversation it is
// actively streaming. The shell only re-attached to a turn the conversation row said was in
// flight WITH a pending reply id — so a turn this client never started, or stopped tracking,
// had no path for a card at all while the SERVER kept the turn parked. That is the stall.
//
// These tests pin the property that fixes it: the pending set is a QUESTION the client can
// ask, answered from the server's own registry, independent of the client's stream state.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
)

// stubPendingAsk serves ListPendingAsks with a fixed set, and counts the calls so a test
// can tell "asked the server" from "assumed".
type stubPendingAsk struct {
	apiv1connect.UnimplementedAskOrchiconServiceHandler
	asks  []*apiv1.PermissionAsk
	calls int
	err   error
}

func (s *stubPendingAsk) ListPendingAsks(ctx context.Context, req *connect.Request[apiv1.ListPendingAsksRequest]) (*connect.Response[apiv1.ListPendingAsksResponse], error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return connect.NewResponse(&apiv1.ListPendingAsksResponse{Asks: s.asks}), nil
}

// A CARD IS RAISED FROM THE SERVER'S ANSWER, with no stream involved at all.
//
// This is the operator's failure directly: the turn is parked server-side, this client has
// no live stream for it, and the card must still appear.
func TestAnOpenAskIsDiscoveredWithoutAnyStream(t *testing.T) {
	stub := &stubPendingAsk{asks: []*apiv1.PermissionAsk{{
		AskId:          "ask-1",
		ConversationId: "conv-1",
		Tool:           "bash",
		Command:        "rm -rf build",
		Directory:      "/p/build",
	}}}
	cl, srv := newTestServer(t, stub)
	defer srv.Close()

	rec := &recorder{conn: map[string]bool{}}
	c := NewController(cl)
	c.Bind(rec, nil)

	// Drive the discovery command the shell runs on attach / re-attach.
	if cmd := c.DiscoverPendingAsks("conv-1"); cmd != nil {
		cmd() // the command is a msg producer; the STORE is written inside it
	}
	if stub.calls != 1 {
		t.Fatalf("ListPendingAsks calls = %d, want the client to ASK the server", stub.calls)
	}
	got := rec.asksSeen()
	if len(got) != 1 {
		t.Fatalf("cards raised = %d, want the discovered ask", len(got))
	}
	if got[0].ID != "ask-1" || got[0].Target != "rm -rf build" || got[0].ConvID != "conv-1" {
		t.Fatalf("discovered card = %+v, want it mapped like a streamed one", got[0])
	}
}

// THE TWO KINDS BOTH DISCOVER. A clarifying QUESTION parks the turn exactly like a
// permission ask, so discovery must cover it — the operator reported losing both.
func TestAQuestionAskIsDiscoveredToo(t *testing.T) {
	stub := &stubPendingAsk{asks: []*apiv1.PermissionAsk{{
		AskId:          "q-1",
		ConversationId: "conv-1",
		Question:       "which branch should I target?",
		Options:        []string{"develop", "main"},
	}}}
	cl, srv := newTestServer(t, stub)
	defer srv.Close()

	rec := &recorder{conn: map[string]bool{}}
	c := NewController(cl)
	c.Bind(rec, nil)

	if cmd := c.DiscoverPendingAsks("conv-1"); cmd != nil {
		cmd()
	}
	got := rec.asksSeen()
	if len(got) != 1 {
		t.Fatalf("cards raised = %d, want the discovered question", len(got))
	}
	if got[0].Kind != AskQuestion || got[0].Options == nil {
		t.Fatalf("discovered question = %+v, want the question kind and its options", got[0])
	}
}

// IT IS SAFE ON EVERY ATTACH AND POLL: a conversation with nothing pending raises nothing,
// and a failing call is harmless (best-effort, like every live signal here — it must never
// fail a turn).
func TestDiscoveryIsQuietWhenNothingIsPendingAndOnFailure(t *testing.T) {
	// Nothing pending.
	stub := &stubPendingAsk{}
	cl, srv := newTestServer(t, stub)
	defer srv.Close()
	rec := &recorder{conn: map[string]bool{}}
	c := NewController(cl)
	c.Bind(rec, nil)
	if cmd := c.DiscoverPendingAsks("conv-1"); cmd != nil {
		cmd()
	}
	if n := len(rec.asksSeen()); n != 0 {
		t.Fatalf("raised %d cards for an empty pending set", n)
	}

	// A failing call: no panic, no card, no error surfaced to the turn.
	fail := &stubPendingAsk{err: connect.NewError(connect.CodeUnavailable, nil)}
	cl2, srv2 := newTestServer(t, fail)
	defer srv2.Close()
	rec2 := &recorder{conn: map[string]bool{}}
	c2 := NewController(cl2)
	c2.Bind(rec2, nil)
	if cmd := c2.DiscoverPendingAsks("conv-1"); cmd != nil {
		cmd() // must not panic
	}
	if n := len(rec2.asksSeen()); n != 0 {
		t.Fatalf("a failed discovery raised %d cards", n)
	}
}

// AN EMPTY CONVERSATION ID ASKS NOTHING — no round trip, no card. The shell calls this
// unconditionally (including before a conversation is open), so it must be free there.
func TestDiscoveryWithNoConversationIsANoOp(t *testing.T) {
	stub := &stubPendingAsk{}
	cl, srv := newTestServer(t, stub)
	defer srv.Close()
	rec := &recorder{conn: map[string]bool{}}
	c := NewController(cl)
	c.Bind(rec, nil)

	if cmd := c.DiscoverPendingAsks(""); cmd != nil {
		cmd()
	}
	if stub.calls != 0 {
		t.Fatalf("ListPendingAsks was called %d times with no conversation", stub.calls)
	}
}
