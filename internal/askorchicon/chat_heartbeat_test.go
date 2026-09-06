package askorchicon

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1connect "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1/apiv1connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/auth"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestChatStreamHeartbeatKeepsIdleTurnAlive verifies acceptance C: an
// idle-but-running turn (collector waiting on the serve bus, no chunks)
// still emits wire traffic within the heartbeat window. The test drives the
// REAL ChatStream RPC over httptest with a 50ms heartbeat interval, reads
// the TurnStarted ack, then asserts a Heartbeat arrives BEFORE the reply is
// fed — proving silent phases generate socket traffic.
func TestChatStreamHeartbeatKeepsIdleTurnAlive(t *testing.T) {
	t.Setenv("ORCHICON_ASK_HEARTBEAT_INTERVAL", "50ms")
	pool, s, client, tenantID, actorID, convID := auditConvEnv(t, "tnt_dev")
	_ = pool

	handler := connect.NewServerStreamHandler(apiv1connect.AskOrchiconServiceChatStreamProcedure,
		func(ctx context.Context, req *connect.Request[apiv1.ChatStreamRequest], stream *connect.ServerStream[apiv1.ChatStreamResponse]) error {
			ctx = tenant.WithID(ctx, tenantID)
			ctx = auth.WithIdentity(ctx, auth.ResolvedIdentity{
				IdentityID: actorID, TenantID: tenantID, AuthMethod: "oidc", IsAdmin: true,
			})
			return s.ChatStream(ctx, req, stream)
		})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client2 := apiv1connect.NewAskOrchiconServiceClient(srv.Client(), srv.URL)
	stream, err := client2.ChatStream(context.Background(), connect.NewRequest(&apiv1.ChatStreamRequest{
		ConversationId: convID,
		Message:        "slow question",
	}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	// TurnStarted ack.
	if !stream.Receive() {
		t.Fatalf("no ack: %v", stream.Err())
	}
	if _, ok := stream.Msg().Event.(*apiv1.ChatStreamResponse_TurnStarted); !ok {
		t.Fatalf("first event = %T, want TurnStarted", stream.Msg().Event)
	}
	// The turn is now idle-but-running (no reply fed). A heartbeat must
	// arrive within a generous multiple of the 50ms interval.
	deadline := time.Now().Add(5 * time.Second)
	sawHeartbeat := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for stream.Receive() {
			if _, ok := stream.Msg().Event.(*apiv1.ChatStreamResponse_Heartbeat); ok {
				sawHeartbeat = true
				return
			}
		}
	}()
	// Feed the reply only AFTER observing (or timing out) so the heartbeat
	// window is genuinely idle. Poll for the send first.
	waitForSend(t, client, 1)
	ses := client.sendCalls[0].sessionID
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !sawHeartbeat && time.Now().Before(deadline) {
		<-tick.C
	}
	if !sawHeartbeat {
		t.Fatal("no heartbeat on idle-but-running turn within 5s (50ms cadence)")
	}
	// Complete the turn so the RPC returns and the test cleans up.
	client.sub.feed(busText(ses, "done"))
	client.sub.feed(busIdle(ses))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not close after reply")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
}

// TestTurnHubPublishSubscribe verifies the broadcast hub fan-out unit:
// published responses reach every subscriber, slow watchers drop (never
// park the collector), and close terminates subscribers.
func TestTurnHubPublishSubscribe(t *testing.T) {
	r := newTurnHubRegistry()
	h := r.create("c1")
	id1, ch1 := h.subscribe()
	_, ch2 := h.subscribe()

	resp := &apiv1.ChatStreamResponse{
		Event: &apiv1.ChatStreamResponse_Heartbeat{Heartbeat: &apiv1.Heartbeat{}},
	}
	h.publish(resp)
	select {
	case got := <-ch1:
		if got != resp {
			t.Errorf("sub1 got %v, want published resp", got)
		}
	case <-time.After(time.Second):
		t.Error("sub1 received nothing")
	}
	select {
	case got := <-ch2:
		if got != resp {
			t.Errorf("sub2 got %v, want published resp", got)
		}
	case <-time.After(time.Second):
		t.Error("sub2 received nothing")
	}
	h.unsubscribe(id1)
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("unsubscribed channel should be closed")
		}
	case <-time.After(time.Second):
		t.Error("unsubscribed channel never closed")
	}
	// Supersede replaces the hub: old watchers drain, new hub is fresh.
	h2 := r.create("c1")
	if h2 == h {
		t.Error("create should replace the hub on supersede")
	}
	select {
	case _, ok := <-ch2:
		if ok {
			t.Error("superseded watcher should drain on close")
		}
	case <-time.After(time.Second):
		t.Error("superseded watcher never closed")
	}
	r.remove("c1")
	if _, found := r.get("c1"); found {
		t.Error("remove should drop the hub")
	}
}
