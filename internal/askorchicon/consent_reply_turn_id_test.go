package askorchicon

// consent_reply_turn_id_test.go — THE ANSWER NAMES THE TURN IT STARTED.
//
// Answering an "Orchicon asks" card on an MCP-served ask (claude/opencode/antigravity) CONTINUES the
// conversation: the asking turn ended holding an awaiting marker, so the server starts a brand-new
// turn with the operator's words. The client never opened that turn and has no way to name it, so the
// reply was only ever found by accident — a LONG reply was still running at the next rail poll (the
// TUI re-attached and streamed it up to 5s late) while a SHORT one finished inside the gap between
// polls and was never seen at all until the operator left the conversation and came back.
//
// The reported symptom is therefore a MISSING IDENTIFIER, and `assistant_message_id` on
// ReplyPermissionAskResponse is it. This pins the server half against a real Postgres: a promoted
// (MCP-served) answer returns the assistant message id the new turn created, and a LIVE answer — the
// orchicon adapter's in-turn question, which already worked — returns none, because it starts nothing.

import (
	"context"
	"encoding/json"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
	"github.com/beardedparrott/orchicon/internal/tenant"
)

// awaitingAskLedger is the durable shape of an MCP-served ask that has been asked and NOT answered: the
// tool call carries the ask's id, and its result is the awaiting marker. findPromotedAsk matches on
// exactly that pair, and it is what makes the reply a PROMOTED one rather than a live in-turn answer.
func awaitingAskLedger(askID string) (calls, results []byte) {
	calls, _ = json.Marshal([]struct {
		ID           string `json:"id"`
		Type         string `json:"type"`
		FunctionName string `json:"function_name"`
		Arguments    string `json:"arguments"`
	}{{
		ID:           askID,
		Type:         "function",
		FunctionName: "mcp__orchicon__ask_user",
		Arguments:    `{"question":"Which branch should the run clone off?"}`,
	}})
	results, _ = json.Marshal([]struct {
		ToolCallID string `json:"tool_call_id"`
		Output     string `json:"output"`
		IsError    bool   `json:"is_error"`
	}{{
		ToolCallID: askID,
		Output:     AskAwaitingAnswerMarker,
	}})
	return calls, results
}

// seedPromotedAsk writes the assistant row that recorded an MCP-served ask, and returns the conversation
// plus the row's id.
func seedPromotedAsk(t *testing.T, pool *db.Pool, askID string) (convID, msgID string) {
	t.Helper()
	convID = createConversation(t, pool, "")
	calls, results := awaitingAskLedger(askID)
	msgID = db.NewID()
	ctx := context.Background()
	ttx, err := pool.BeginTenantTx(ctx, "tnt_dev")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer ttx.Rollback(ctx)
	meta, _ := json.Marshal(map[string]any{"model_ref": askTestModelRef})
	if _, err := db.CreateMessage(ctx, ttx.Tx, db.MessageRow{
		ID:             msgID,
		TenantID:       "tnt_dev",
		ConversationID: convID,
		Role:           "assistant",
		Content:        "Which branch should the run clone off?",
		ToolCalls:      calls,
		ToolResults:    results,
		Attachments:    []byte("[]"),
		Metadata:       meta,
		Reasoning:      []string{},
	}); err != nil {
		t.Fatalf("seed the recorded ask row: %v", err)
	}
	if err := ttx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return convID, msgID
}

// TestAPromotedAskAnswerReturnsTheNewTurnsAssistantMessageID is AC1, the server half of the fix.
//
// It answers a PROMOTED (MCP-served) ask with an UNSPECIFIED choice plus the operator's words — the
// exact call the TUI's card click makes — and requires the response to name the turn that answer
// started, equal to the assistant row the turn persists under. Without that id the client cannot
// attach, which is the operator's "the reply never appears until I leave and come back".
func TestAPromotedAskAnswerReturnsTheNewTurnsAssistantMessageID(t *testing.T) {
	pool := chatDBTestPool(t)
	client := &fakeSessionClient{}
	s := newChatService(t, pool, client)

	const askID = "call_promoted_reply_1"
	convID, msgID := seedPromotedAsk(t, pool, askID)

	resp, err := s.ReplyPermissionAsk(tenant.WithID(context.Background(), "tnt_dev"),
		connectReq(&apiv1.ReplyPermissionAskRequest{
			ConversationId: convID,
			AskId:          askID,
			// A QUESTION: content, not a permission choice.
			Choice: apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED,
			Answer: "develop",
		}))
	if err != nil {
		t.Fatalf("ReplyPermissionAsk on a promoted ask: %v", err)
	}
	if !resp.Msg.GetApplied() || resp.Msg.GetExpired() {
		t.Fatalf("the answer was not applied: %+v — the operator's words were dropped", resp.Msg)
	}
	got := resp.Msg.GetAssistantMessageId()
	if got == "" {
		t.Fatal("the reply names no turn (assistant_message_id is empty). The client has nothing to " +
			"attach to, so it can only discover this turn by accident — the operator's long-reply-works " +
			"/ short-reply-never symptom")
	}
	if got == msgID {
		t.Fatalf("the reply reused the ASKING message's id (%s) instead of the new turn's — a client " +
			"would re-attach to a turn that is already finished", got)
	}

	// AND IT IS THE ROW THE NEW TURN ACTUALLY CREATED, not merely a non-empty string. The turn acks that
	// id (TurnStarted.assistant_message_id) and persists the reply under it, so the row must appear in
	// the conversation under exactly the id the reply named.
	//
	// The row is written by the DETACHED collector, so it lands after the RPC returns — the same
	// asynchrony the client is attaching to. Feeding one text part makes the collector's partial mirror
	// write it, which is the row a client would stream into.
	waitForSend(t, client, 1)
	client.sub.feed(busText("ses_1", "the reply begins"))
	row := waitForMessage(t, pool, convID, got)
	if row.Role != "assistant" {
		t.Fatalf("the returned id %q is a %q row, want the assistant row the turn acks", got, row.Role)
	}

	// AND IT IS THE TURN'S ACKED ID, which is the same id TurnStarted carries on that turn's stream — so
	// a client attaching with it is addressing the turn the server is really running.
	entry, running := s.turns.get(convID)
	if !running {
		t.Fatal("the reply reported a turn but the turn registry holds none — the id addresses nothing")
	}
	if entry.assistantMsgID != got {
		t.Fatalf("the reply named %q but the running turn acks %q; a client would dial a turn that is "+
			"not the one producing its reply", got, entry.assistantMsgID)
	}

	// THE ANSWER IS THE TURN'S USER MESSAGE: the promoted path delivers the operator's words as the
	// next message, which is what the returned turn is producing a reply to.
	var sawAnswer bool
	for _, m := range listMessages(t, pool, convID) {
		if m.Role == "user" && m.Content == "develop" {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Error("the conversation carries no user row with the operator's answer — the turn the id names " +
			"is not the one answering them")
	}
}

// TestALiveAskAnswerReturnsNoAssistantMessageID is the OTHER HALF, and it is the no-regression guard: the
// orchicon adapter's ask is answered INSIDE the turn that asked it (the turn is still running and the
// client is already consuming its stream), so the server starts nothing and must name nothing. A client
// that re-attached here would dial a turn it is already streaming.
func TestALiveAskAnswerReturnsNoAssistantMessageID(t *testing.T) {
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	if ask, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind:         "question",
		PermissionID: "q_live_1",
		Question:     "Which branch should the run clone off?",
		Options:      []string{"develop", "main"},
	}); ask == nil || refusal != "" {
		t.Fatalf("raiseQuestion: ask=%v refusal=%q", ask, refusal)
	}

	resp, err := svc.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
		ConversationId: "conv-1",
		AskId:          "q_live_1",
		Choice:         apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED,
		Answer:         "main",
	}))
	if err != nil {
		t.Fatalf("ReplyPermissionAsk: %v", err)
	}
	if !resp.Msg.GetApplied() {
		t.Fatalf("the live answer was not applied: %+v", resp.Msg)
	}
	if got := resp.Msg.GetAssistantMessageId(); got != "" {
		t.Fatalf("a LIVE in-turn answer returned assistant_message_id=%q — it starts no turn, and a "+
			"client that attached to it would abandon the stream it is already reading", got)
	}
}
