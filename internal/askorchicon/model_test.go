// model_test.go — SetConversationModel: the per-conversation model_ref write
// path. It is what lets `/models` (TUI) and the GUI's model chip retarget an
// ALREADY-OPEN conversation instead of forcing a new one.
//
// DB-backed like its mode_test.go neighbours (chatDBTestPool skips without
// ORCHICON_TEST_DSN).
package askorchicon

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

const testModelRef = "orchicon/anthropic/claude-sonnet-4"

// Setting a model writes it to the conversation row and echoes it back; an
// EMPTY ref CLEARS the override (so the tenant default applies) rather than
// being rejected.
func TestSetConversationModel(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")

	resp, err := s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
		Id: convID, ModelRef: testModelRef,
	}))
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if got := resp.Msg.GetConversation().GetModelRef(); got != testModelRef {
		t.Errorf("returned model_ref = %q, want %q", got, testModelRef)
	}
	if got := resp.Msg.GetConversation().GetId(); got != convID {
		t.Errorf("returned id = %q, want %q", got, convID)
	}

	// The write is durable: read it back through GetConversation.
	got, err := s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID}))
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if ref := got.Msg.GetConversation().GetModelRef(); ref != testModelRef {
		t.Errorf("persisted model_ref = %q, want %q", ref, testModelRef)
	}

	// Empty clears the override (the conversation falls back to the tenant
	// default), and is NOT an error.
	cleared, err := s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{Id: convID}))
	if err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if ref := cleared.Msg.GetConversation().GetModelRef(); ref != "" {
		t.Errorf("cleared model_ref = %q, want empty", ref)
	}
}

// A malformed ref never reaches the row; a missing conversation is NotFound;
// and an empty id is refused before anything else.
func TestSetConversationModelValidation(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")

	var cerr *connect.Error
	_, err := s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
		Id: convID, ModelRef: "/llama3", // empty first segment
	}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("malformed ref error = %v, want InvalidArgument", err)
	}
	// The refusal did not write: the stored ref is still whatever it was.
	after, gerr := s.GetConversation(ctx, connect.NewRequest(&apiv1.GetConversationRequest{Id: convID}))
	if gerr != nil {
		t.Fatalf("get conversation: %v", gerr)
	}
	if ref := after.Msg.GetConversation().GetModelRef(); ref != "" {
		t.Errorf("a refused ref was persisted: %q", ref)
	}

	_, err = s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
		ModelRef: testModelRef,
	}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("empty id error = %v, want InvalidArgument", err)
	}

	_, err = s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
		Id: "does-not-exist", ModelRef: testModelRef,
	}))
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("missing conversation error = %v, want NotFound", err)
	}
}

// A LEGACY 1/2-segment ref is accepted (the pinned grammar infers adapter
// "opencode"), so an existing conversation is never un-settable just because
// it carries a pre-namespace ref.
func TestSetConversationModelAcceptsALegacyRef(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	ctx := tenant.WithID(context.Background(), "tnt_dev")
	convID := createConversation(t, pool, "")

	resp, err := s.SetConversationModel(ctx, connect.NewRequest(&apiv1.SetConversationModelRequest{
		Id: convID, ModelRef: "opencode-go/deepseek-v4-flash",
	}))
	if err != nil {
		t.Fatalf("legacy ref rejected: %v", err)
	}
	if got := resp.Msg.GetConversation().GetModelRef(); got != "opencode-go/deepseek-v4-flash" {
		t.Errorf("legacy ref = %q, want it stored verbatim", got)
	}
}
