package askorchicon

// withhold_wiring_db_test.go — the guard is WIRED, asserted through the real RPC rather than by grepping
// the source for the call.
//
// withhold_stale_resolution_test.go proves the RULE. This proves ListMessages APPLIES it: a rule that is
// implemented and never called is the exact shape of a half-done fix, and a source-level assertion would not
// catch someone replacing the call site.
//
// Skips without ORCHICON_TEST_DSN, like the rest of this package's DB tests.

import (
	"context"
	"encoding/json"
	"testing"

	"connectrpc.com/connect"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

func TestListMessagesWithholdsAResolutionForAnAskThatIsOpenNow(t *testing.T) {
	pool := chatDBTestPool(t)
	s := newChatService(t, pool, &fakeSessionClient{})
	convID := createConversation(t, pool, testInterjectModelRef)
	ctx := tenant.WithID(context.Background(), "tnt_dev")

	// A ledger row carrying BOTH histories under one id — exactly the legacy collision measured in
	// production: native-ask-1 had an ALLOW_ONCE from an earlier day and a live raise from a later one.
	calls := []map[string]any{
		{"id": "native-ask-1", "type": "function", "function_name": "permission.ask"},
		{"id": "native-ask-1", "type": "function", "function_name": "permission.user_PERMISSION_CHOICE_ALLOW_ONCE"},
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		t.Fatal(err)
	}
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	if _, err := db.CreateMessage(ctx, ttx.Tx, db.MessageRow{
		ID: db.NewID(), TenantID: "tnt_dev", ConversationID: convID,
		Role: "assistant", Content: "working", ToolCalls: raw,
		ToolResults: []byte("[]"), Attachments: []byte("[]"), Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create message: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	list := func() []string {
		resp, err := s.ListMessages(ctx, connect.NewRequest(&apiv1.ListMessagesRequest{ConversationId: convID}))
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		var out []string
		for _, m := range resp.Msg.GetMessages() {
			for _, tc := range m.GetToolCalls() {
				out = append(out, tc.GetFunctionName())
			}
		}
		return out
	}

	contains := func(xs []string, want string) bool {
		for _, x := range xs {
			if x == want {
				return true
			}
		}
		return false
	}

	// (1) NOTHING OPEN: both records reach the client, and the card settles normally.
	got := list()
	if !contains(got, "permission.user_PERMISSION_CHOICE_ALLOW_ONCE") {
		t.Fatalf("with nothing open, the resolution must be delivered (the card settles); got %v", got)
	}

	// (2) THE ASK IS OPEN NOW: the stale resolution must be withheld, and the raise must survive so a
	// client attaching now still learns the card exists.
	s.pending.put(convID, &pendingAsk{AskID: "native-ask-1", ConversationID: convID, state: askOpen})
	got = list()
	if contains(got, "permission.user_PERMISSION_CHOICE_ALLOW_ONCE") {
		t.Fatalf("a resolution was delivered for an ask the server is holding OPEN — the client settles the "+
			"live card from it and re-arms on the next poll: the flicker. got %v", got)
	}
	if !contains(got, "permission.ask") {
		t.Errorf("the raise was withheld too, so a client attaching now would not know the card exists; got %v", got)
	}
}
