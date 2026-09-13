package askorchicon

// DB-backed tests for the CompactConversation RPC surface.
//
//	export ORCHICON_TEST_DSN='postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable'
//	go test ./internal/askorchicon/ -run TestCompactConversation -v

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// compactingFakeClient satisfies ChatTurnClient (promoted from the embed) AND
// the optional scheduler.ChatCompactor capability, recording every call.
type compactingFakeClient struct {
	*fakeSessionClient

	calls []scheduler.CompactConversationOpts
	res   scheduler.ChatCompaction
	err   error
}

func (c *compactingFakeClient) CompactConversationSession(_ context.Context, opts scheduler.CompactConversationOpts) (scheduler.ChatCompaction, error) {
	c.calls = append(c.calls, opts)
	if c.err != nil {
		return scheduler.ChatCompaction{}, c.err
	}
	return c.res, nil
}

func TestCompactConversationRefusesWhileTurnInFlight(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &compactingFakeClient{fakeSessionClient: &fakeSessionClient{}}
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = client

	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")

	// Register a live turn, as the collector does.
	_, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if _, ok := s.turns.register(convID, "tnt_dev", "msg_live", cancel); !ok {
		t.Fatal("register turn")
	}

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	_, err := s.CompactConversation(ctx, connect.NewRequest(
		&apiv1.CompactConversationRequest{ConversationId: convID, Reason: "manual"},
	))
	if err == nil {
		t.Fatal("compaction must be refused while a turn is in flight (it would race the collector)")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if len(client.calls) != 0 {
		t.Error("the adapter must not be asked to compact while a turn is in flight")
	}
}

func TestCompactConversationWithoutCapabilityIsActionable(t *testing.T) {
	pool := chatDBTestPool(t)
	// A plain fake: supports chat, does NOT implement the compactor capability.
	s := newChatService(t, pool, &fakeSessionClient{})

	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	_, err := s.CompactConversation(ctx, connect.NewRequest(
		&apiv1.CompactConversationRequest{ConversationId: convID, Reason: "manual"},
	))
	if err == nil {
		t.Fatal("an adapter without the capability must produce an actionable error, not appear to succeed")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestCompactConversationHappyPath(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &compactingFakeClient{
		fakeSessionClient: &fakeSessionClient{},
		res: scheduler.ChatCompaction{
			Compacted: true,
			Detail:    "compacted 20 messages into 1 summary + 6 recent messages",
			Summary:   "the summary",
		},
	}
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = client

	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")
	setConversationSessionID(t, pool, convID, "orchicon-ask:"+convID)

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	res, err := s.CompactConversation(ctx, connect.NewRequest(
		&apiv1.CompactConversationRequest{ConversationId: convID, Reason: "manual"},
	))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !res.Msg.Compacted || res.Msg.Summary != "the summary" {
		t.Errorf("response = %+v, want the adapter's compaction", res.Msg)
	}
	// The conversation's resolved session id must be handed to the adapter: the
	// session-ful path cannot summarize without it.
	if len(client.calls) != 1 {
		t.Fatalf("adapter calls = %d, want 1", len(client.calls))
	}
	if client.calls[0].SessionID != "orchicon-ask:"+convID {
		t.Errorf("session id = %q, want the conversation's stored session", client.calls[0].SessionID)
	}
	if client.calls[0].Reason != "manual" {
		t.Errorf("reason = %q, want manual", client.calls[0].Reason)
	}
}

func TestCompactConversationAdapterErrorSurfaces(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &compactingFakeClient{
		fakeSessionClient: &fakeSessionClient{},
		err:               errors.New("provider still over limit"),
	}
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = client

	convID := createConversation(t, pool, "orchicon/deepseek/deepseek-flash")
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	if _, err := s.CompactConversation(ctx, connect.NewRequest(
		&apiv1.CompactConversationRequest{ConversationId: convID},
	)); err == nil {
		t.Fatal("an adapter compaction failure must surface, not be swallowed")
	}
}

func TestCompactConversationUnknownConversationNotFound(t *testing.T) {
	pool := chatDBTestPool(t)
	s := New(pool, slog.Default(), nil, nil, nil)
	s.testServeClient = &compactingFakeClient{fakeSessionClient: &fakeSessionClient{}}

	ctx := tenant.WithID(context.Background(), "tnt_dev")
	_, err := s.CompactConversation(ctx, connect.NewRequest(
		&apiv1.CompactConversationRequest{ConversationId: "01NOSUCHCONVERSATION0000000"},
	))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}
