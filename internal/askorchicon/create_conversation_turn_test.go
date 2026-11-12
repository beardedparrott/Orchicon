package askorchicon

// create_conversation_turn_test.go — AN INITIAL MESSAGE MUST START A TURN.
//
// REPORTED from a live Ask session: the first question was persisted and then sat unanswered until a LATER
// message was sent by hand, at which point the assistant answered the FIRST one. CreateConversation inserted
// the user row and returned without dispatching anything, so "start the conversation immediately" — the
// RPC's own documented contract — was not what it did.
//
// Invisible to every client that sends InitialMessage and expects an answer: a harness, a script, or the
// GUI's new-conversation-with-a-prompt path. The turn only began when some later ChatStream dispatched it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// TestCreateConversationWithAnInitialMessageStartsATurn is the reported bug.
func TestCreateConversationWithAnInitialMessageStartsATurn(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	const question = "what should a team verify first when building a small web app?"
	resp, err := s.CreateConversation(tenant.WithID(context.Background(), "tnt_dev"), connect.NewRequest(&apiv1.CreateConversationRequest{
		Mode:           apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
		InitialMessage: question,
	}))
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	convID := resp.Msg.GetConversation().GetId()
	if strings.TrimSpace(convID) == "" {
		t.Fatal("CreateConversation returned no conversation id")
	}

	// A DISPATCH IS THE OBSERVABLE PROOF A TURN STARTED. The fake records every send the collector makes;
	// before the fix there were none, which is exactly why nothing answered.
	waitForSend(t, client, 1)

	// And the question is in the conversation exactly ONCE — the turn persists the user row itself, so a
	// fix that also pre-inserted it would duplicate the first message.
	n := 0
	for _, m := range listMessages(t, pool, convID) {
		if strings.EqualFold(m.Role, "user") && strings.Contains(m.Content, "verify first") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the initial message appears %d times in the conversation, want exactly 1 — a duplicate "+
			"means the row is inserted twice (once by CreateConversation, once by the turn)", n)
	}
}

// THE CONTROL: a conversation created WITHOUT an initial message sends nothing, so the fix cannot be
// "always dispatch something".
func TestCreateConversationWithoutAMessageStartsNoTurn(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	resp, err := s.CreateConversation(tenant.WithID(context.Background(), "tnt_dev"), connect.NewRequest(&apiv1.CreateConversationRequest{
		Mode: apiv1.ConversationMode_CONVERSATION_MODE_BRAINSTORM,
	}))
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if strings.TrimSpace(resp.Msg.GetConversation().GetId()) == "" {
		t.Fatal("CreateConversation returned no conversation id")
	}

	// Give a wrong implementation time to dispatch before asserting it did not.
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
		client.mu.Lock()
		n := len(client.sendCalls)
		client.mu.Unlock()
		if n > 0 {
			t.Fatalf("creating an EMPTY conversation dispatched %d send(s) — nothing was asked", n)
		}
	}
}
