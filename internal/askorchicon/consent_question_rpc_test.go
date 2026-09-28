package askorchicon

import (
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestReplyPermissionAskAnswersAQuestion is the PAUSE's server contract: a
// clarifying question is answered with CONTENT, not a permission choice, and the
// answer is recorded where the drain loop reads it (so it becomes the ask_user tool
// result and the paused turn resumes).
//
// Without this branch the RPC rejected an unspecified choice outright, so a blocking
// question could never be answered at all.
func TestReplyPermissionAskAnswersAQuestion(t *testing.T) {
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	ask, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind:         "question",
		PermissionID: "q_1",
		Question:     "Which branch should the run clone off?",
		Options:      []string{"develop", "main"},
	})
	if ask == nil || refusal != "" {
		t.Fatalf("raiseQuestion: ask=%v refusal=%q", ask, refusal)
	}

	resp, err := svc.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
		ConversationId: "conv-1",
		AskId:          "q_1",
		// UNSPECIFIED: a question has no permission choice.
		Choice: apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED,
		Answer: "main",
	}))
	if err != nil {
		t.Fatalf("ReplyPermissionAsk: %v", err)
	}
	if !resp.Msg.GetApplied() || resp.Msg.GetExpired() {
		t.Fatalf("answer not applied: %+v", resp.Msg)
	}
	// The answer is READABLE by the drain loop — that is what makes it the tool result.
	got, ok := ask.clientAnswerValue()
	if !ok || got != "main" {
		t.Fatalf("clientAnswerValue = (%q, %v), want the operator's answer", got, ok)
	}
	// A question must record NO grant: it grants nothing.
	if svc.grants.Len("conv-1") != 0 {
		t.Fatalf("answering a question recorded %d grants — a question grants nothing", svc.grants.Len("conv-1"))
	}
}

// TestReplyPermissionAskRejectsAnEmptyQuestionAnswer: an empty answer would resume
// the model with a blank tool result, which reads as "the operator said nothing"
// when in fact nothing was submitted.
func TestReplyPermissionAskRejectsAnEmptyQuestionAnswer(t *testing.T) {
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	if ask, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind: "question", PermissionID: "q_2", Question: "Which?", Options: []string{"a", "b"},
	}); ask == nil || refusal != "" {
		t.Fatalf("raiseQuestion: ask=%v refusal=%q", ask, refusal)
	}
	if _, err := svc.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
		ConversationId: "conv-1", AskId: "q_2",
		Choice: apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED,
		Answer: "   ",
	})); err == nil {
		t.Fatal("an empty answer must be refused, not recorded")
	}
}

// TestReplyPermissionAskAnswerIsIdempotent: a second answer to the same question is
// reported expired rather than overwriting the first — the same rule a permission
// decision follows, and the reason the operator's click cannot double-apply.
func TestReplyPermissionAskAnswerIsIdempotent(t *testing.T) {
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	if ask, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind: "question", PermissionID: "q_3", Question: "Which?", Options: []string{"a", "b"},
	}); ask == nil || refusal != "" {
		t.Fatalf("raiseQuestion: ask=%v refusal=%q", ask, refusal)
	}
	first, err := svc.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
		ConversationId: "conv-1", AskId: "q_3", Choice: apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, Answer: "a",
	}))
	if err != nil || !first.Msg.GetApplied() {
		t.Fatalf("first answer: %+v err=%v", first.Msg, err)
	}
	second, err := svc.ReplyPermissionAsk(tenantCtx(), connectReq(&apiv1.ReplyPermissionAskRequest{
		ConversationId: "conv-1", AskId: "q_3", Choice: apiv1.PermissionChoice_PERMISSION_CHOICE_UNSPECIFIED, Answer: "b",
	}))
	if err != nil {
		t.Fatalf("second answer: %v", err)
	}
	if second.Msg.GetApplied() || !second.Msg.GetExpired() {
		t.Fatalf("second answer: applied=%v expired=%v — want expired", second.Msg.GetApplied(), second.Msg.GetExpired())
	}
}

// TestRaiseQuestionRefusesAnEmptyQuestionAndTooFewOptions pins the fail-loud rule:
// a question with no text is not answerable and must be REFUSED rather than raised
// as a card the operator cannot complete.
func TestRaiseQuestionRefusesAnEmptyQuestionAndTooFewOptions(t *testing.T) {
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)

	if _, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind: "question", PermissionID: "q_4", Question: "   ",
	}); refusal == "" {
		t.Error("an empty question must be refused")
	}
	// The turn must not be left believing an ask is pending.
	if n := len(svc.pending.list("conv-1")); n != 0 {
		t.Errorf("a refused question was still registered: %d pending", n)
	}
}
