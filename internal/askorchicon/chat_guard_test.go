package askorchicon

// Ask-capability guard + stub-kind pluggability tests (ADR-0004 D1).
//
// These prove the two halves of the "Ask chat on any adapter" contract:
//  1. A NEW adapter kind needs only Dispatcher registration + the
//     ChatTurnClient interface — zero chat.go changes. A stub kind registered
//     on a Dispatcher resolves through the shared substrate and drives a turn
//     via the adapter-neutral ChatTurnClient / SessionEvent / SessionBus shapes.
//  2. A kind that registers but does NOT implement ChatTurnClient is rejected
//     at conversation creation (and tenant-default save) BEFORE the first
//     message send — the guard, not a dispatch-time surprise.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// stubChatBridge is a fake adapter kind that implements BOTH the
// AdapterBridge.Start surface (so it can be registered on a Dispatcher) and
// the ChatTurnClient capability (so it is Ask-capable). It embeds
// fakeSessionClient for the ChatTurnClient surface and adds a no-op Start.
// This is the "stub kind" the pluggability test registers — proving a new
// adapter needs only registration + the interface, no chat.go changes.
type stubChatBridge struct {
	*fakeSessionClient
}

func (s *stubChatBridge) Start(context.Context, db.ExecutionRow, scheduler.ExecutionManifest, scheduler.ExecutionCallbacks) error {
	return nil
}

// stubPlainBridge is a fake adapter kind that implements ONLY AdapterBridge.Start
// (worker-execution dispatchable) but NOT ChatTurnClient — the fixture for the
// guard: it is registered (so Kinds() offers it) but not Ask-capable.
type stubPlainBridge struct{}

func (s *stubPlainBridge) Start(context.Context, db.ExecutionRow, scheduler.ExecutionManifest, scheduler.ExecutionCallbacks) error {
	return nil
}

// TestStubKindPluggability proves a new adapter kind needs only Dispatcher
// registration + the ChatTurnClient interface: a stub kind registered on a
// Dispatcher resolves through the shared substrate and drives one Ask turn
// via the adapter-neutral shapes, with zero chat.go changes.
func TestStubKindPluggability(t *testing.T) {
	// Register a stub "claude" kind that implements ChatTurnClient.
	client := &fakeSessionClient{}
	d := scheduler.NewDispatcher()
	d.Register("claude", &stubChatBridge{client})

	s := &Service{log: slog.Default(), turns: newTurnRegistry()}
	s.SetDispatcher(d)

	// Resolve the stub kind through the Dispatcher by its model_ref kind —
	// the exact path resolveChatClient uses (no chat.go branch).
	got, err := s.resolveChatClient("conv-1", "claude/acme/model-1")
	if err != nil {
		t.Fatalf("resolveChatClient(claude): %v", err)
	}
	if got == nil {
		t.Fatal("resolveChatClient(claude) returned nil client")
	}
	if _, ok := got.(*stubChatBridge); !ok {
		t.Fatalf("resolved client is %T, want *stubChatBridge", got)
	}

	// Drive one Ask turn through the adapter-neutral ChatTurnClient surface.
	// The collector calls CreateConversationSession → Subscribe →
	// SendTurnMessage → drains SessionEvents. The stub's SendTurnMessage
	// returns nil immediately (accepted); the collector finalizes on an
	// `idle` SessionEvent (the adapter-neutral completion signal). Feed the
	// idle after the send is accepted so the sent guard is deterministic.
	opts := turnCollectOpts{
		client:      got,
		tenantID:    "tnt_test",
		convID:      "conv-1",
		token:       1,
		modelRef:    "claude/acme/model-1",
		seedSystem:  "system",
		reuseSystem: "system",
		userMsg:     "hello",
	}
	done := make(chan struct{})
	var reply string
	var collectErr error
	go func() {
		defer close(done)
		reply, _, _, collectErr = s.collectConversationReply(context.Background(), opts)
	}()
	// Wait for the stub's SendTurnMessage to be accepted, then feed the idle
	// that completes the turn (the adapter-neutral completion signal).
	waitForSend(t, client, 1)
	client.mu.Lock()
	sub := client.sub
	client.mu.Unlock()
	if sub == nil {
		t.Fatal("stub Subscribe did not create a bus sub")
	}
	sub.feed(busIdle("ses_1"))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("collectConversationReply over stub kind did not return within 10s")
	}
	if collectErr != nil {
		t.Fatalf("collectConversationReply over stub kind: %v", collectErr)
	}
	// The stub emits no text parts, so the collected reply is empty — the
	// turn was accepted and dispatched through the stub kind.
	if reply != "" {
		t.Fatalf("stub turn reply = %q, want empty (stub emits no text)", reply)
	}
	// The stub's SendTurnMessage was actually invoked.
	client.mu.Lock()
	sent := len(client.sendCalls)
	client.mu.Unlock()
	if sent != 1 {
		t.Fatalf("stub SendTurnMessage called %d times, want 1", sent)
	}
}

// TestCreateConversationRejectsNonAskCapableKind proves the guard: a
// conversation whose model_ref adapter kind is registered but does NOT
// implement ChatTurnClient is rejected at creation with a FailedPrecondition
// BEFORE the first message send — the unimplemented kind trips the guard and
// never reaches dispatch.
func TestCreateConversationRejectsNonAskCapableKind(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})

	// Register a "claude" kind that is dispatchable for worker executions
	// (AdapterBridge.Start) but NOT Ask-capable (no ChatTurnClient).
	d := scheduler.NewDispatcher()
	d.Register("claude", &stubPlainBridge{})
	s.SetDispatcher(d)
	s.SetChatKinds(d.ChatKinds)

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	_, err := s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
		ModelRef: "claude/acme/model-1",
	}))
	if err == nil {
		t.Fatal("CreateConversation with non-Ask-capable kind succeeded; want FailedPrecondition")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("CreateConversation error = %v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "does not support Ask chat") {
		t.Fatalf("CreateConversation error %q does not name the Ask-capability guard", err)
	}
}

// TestCreateConversationAllowsAskCapableKind proves the guard does NOT block
// a kind that implements ChatTurnClient (the implemented path must pass).
func TestCreateConversationAllowsAskCapableKind(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})

	d := scheduler.NewDispatcher()
	d.Register("claude", &stubChatBridge{&fakeSessionClient{}})
	s.SetDispatcher(d)
	s.SetChatKinds(d.ChatKinds)

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	resp, err := s.CreateConversation(ctx, connect.NewRequest(&apiv1.CreateConversationRequest{
		ModelRef: "claude/acme/model-1",
	}))
	if err != nil {
		t.Fatalf("CreateConversation with Ask-capable kind failed: %v", err)
	}
	if resp.Msg.Conversation.ModelRef != "claude/acme/model-1" {
		t.Fatalf("created conversation model_ref = %q, want claude/acme/model-1", resp.Msg.Conversation.ModelRef)
	}
}

// TestValidateModelRefRejectsNonAskCapableKind proves the tenant-default save
// guard: a ref whose kind is registered but not Ask-capable is rejected by
// validateModelRef so the update_settings write path never persists a ref
// that would fail at first message.
func TestValidateModelRefRejectsNonAskCapableKind(t *testing.T) {
	s := &Service{log: slog.Default()}
	d := scheduler.NewDispatcher()
	d.Register("claude", &stubPlainBridge{})
	s.SetDispatcher(d)
	s.SetChatKinds(d.ChatKinds)

	err := s.validateModelRef("claude/acme/model-1")
	if err == nil {
		t.Fatal("validateModelRef with non-Ask-capable kind succeeded; want error")
	}
	if !strings.Contains(err.Error(), "does not support Ask chat") {
		t.Fatalf("validateModelRef error %q does not name the Ask-capability guard", err)
	}
}

// TestValidateModelRefAllowsAskCapableKind proves the tenant-default save
// guard does NOT block an Ask-capable kind.
func TestValidateModelRefAllowsAskCapableKind(t *testing.T) {
	s := &Service{log: slog.Default()}
	d := scheduler.NewDispatcher()
	d.Register("claude", &stubChatBridge{&fakeSessionClient{}})
	s.SetDispatcher(d)
	s.SetChatKinds(d.ChatKinds)

	if err := s.validateModelRef("claude/acme/model-1"); err != nil {
		t.Fatalf("validateModelRef with Ask-capable kind failed: %v", err)
	}
}
