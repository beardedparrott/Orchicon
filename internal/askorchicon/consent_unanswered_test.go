package askorchicon

// consent_unanswered_test.go — A QUESTION NOBODY ANSWERED IS NOT A REFUSAL.
//
// The operator: "Sessions seem to be timing out on me..." and, separately, "This is another issue we
// should investigate as well - timeouts are losing context in the conversation" — pointing at
// "reply timed out after 30m0s on model ... — the model may be overloaded or unavailable. Check the
// Ask Orchicon model in Settings → Default models, then retry."
//
// They reported it from the wrong side of the terminal: "Sorry, could you please ask the question
// again, I was away from my terminal." A turn parked on a card raises no activity, so the reply
// window's SILENCE budget (see turnReplyWindow) reads an absent operator as a dead model and ends
// the turn. The bound is deliberate and stays — what was broken is what the operator was left with:
//
//   - the failure named a model that was fine, and never the question it was waiting on;
//   - the question was published as `expired`, which every client folds into a REFUSAL. The TUI
//     recorded DecisionDeny and printed "Deny · <subject>" over the tool-and-target of a permission
//     nobody asked about, and the answer the operator gave on returning was refused as late.
//
// `unanswered` says the only true thing — nobody answered it — and it names the question, which is
// the context that has to survive for a retry to resume.
//
// THE PERMISSION CASE IS UNCHANGED, and pinned elsewhere: an unanswered permission still resolves as
// `expired` (TestFinalizePublishesTheResolutionForEveryAskItSettles), because for a permission the
// serve really was answered `reject` — the call was refused, and the record says so.

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/beardedparrott/orchicon/api/gen/go/orchicon/api/v1"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// raiseOpenQuestion registers one open clarifying question on a test turn.
func raiseOpenQuestion(t *testing.T, ct *consentTurn, id, question string) *pendingAsk {
	t.Helper()
	ask, refusal := ct.raiseQuestion("ses_1", scheduler.SessionEvent{
		Kind:         "question",
		PermissionID: id,
		Question:     question,
		Options:      []string{"develop", "main"},
	})
	if ask == nil || refusal != "" {
		t.Fatalf("raiseQuestion: ask=%v refusal=%q", ask, refusal)
	}
	return ask
}

// TestWaitingOnOperatorNamesWhatTheTurnIsWaitingFor pins the helper that lets a turn which dies
// parked say WHY — and that a turn waiting on nothing still reports nothing, so the ordinary
// model-facing message is what a genuinely quiet turn still gets.
func TestWaitingOnOperatorNamesWhatTheTurnIsWaitingFor(t *testing.T) {
	isolatedPolicy(t, "")
	svc := testConsentService()

	quiet := newTestConsentTurn(svc, "/p/proj", true, nil)
	if asking, ok := quiet.waitingOnOperator(); ok {
		t.Fatalf("a turn with no ask reported waiting on %q — every silent turn would then be reported "+
			"as waiting on a human", asking)
	}

	const question = "Which branch should the run clone off?"
	asking := newTestConsentTurn(svc, "/p/proj", true, nil)
	raiseOpenQuestion(t, asking, "q_1", question)
	if got, ok := asking.waitingOnOperator(); !ok || got != question {
		t.Fatalf("waitingOnOperator = (%q, %v), want the question being waited on", got, ok)
	}

	// A PERMISSION is named by its target — the same fact its card shows. It gets its own service:
	// the registry is keyed by conversation and every test turn is "conv-1", so the question above
	// would otherwise still be the open ask this turn is waiting on.
	permSvc := testConsentService()
	perm := newTestConsentTurn(permSvc, "/p/proj", true, nil)
	if _, _, refusal := perm.decide(context.Background(), "ses_1", fileAskEvent("per_1", "write", "/p/sibling/x.md")); refusal != "" {
		t.Fatalf("decide refusal = %q", refusal)
	}
	if got, ok := perm.waitingOnOperator(); !ok || !strings.Contains(got, "x.md") {
		t.Fatalf("waitingOnOperator for a permission = (%q, %v), want the target it is waiting on", got, ok)
	}
}

// TestAQuestionThatRunsOutOfTimeIsUnansweredNotExpired is the wire half.
func TestAQuestionThatRunsOutOfTimeIsUnansweredNotExpired(t *testing.T) {
	svc := testConsentService()
	client := &consentFakeClient{}
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	raiseOpenQuestion(t, ct, "q_1", "Which branch should the run clone off?")

	var published []*apiv1.ChatStreamResponse
	ct.finalize(context.Background(), client, func(r *apiv1.ChatStreamResponse) { published = append(published, r) })

	if len(published) != 1 {
		t.Fatalf("finalize published %d resolutions for 1 settled ask", len(published))
	}
	res := published[0].GetPermissionAskResolved()
	if res == nil {
		t.Fatal("finalize published a non-resolution")
	}
	if res.GetOutcome() != "unanswered" {
		t.Fatalf("a question nobody answered resolved as %q — clients fold `expired` into a REFUSAL, "+
			"which records a decision the operator never made", res.GetOutcome())
	}
	// AND THE SERVE IS STILL ANSWERED `reject`: opencode holds the tool call, and leaving it open
	// would leave the session holding a phantom permission. That part is deliberately unchanged —
	// what the operator's CLIENTS are told is the part that was wrong.
	if got := client.got(); len(got) != 1 || got[0] != "reject" {
		t.Fatalf("decisions sent to the serve = %v, want [reject]", got)
	}
}

// TestAParkedTurnThatRunsOutOfTimeBlamesTheWaitNotTheModel is the other half, end to end: the turn
// is bounded exactly as before, but the failure says what it was waiting for.
func TestAParkedTurnThatRunsOutOfTimeBlamesTheWaitNotTheModel(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REPLY_WINDOW", "100ms")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	const question = "Which branch should the run clone off?"
	raiseOpenQuestion(t, ct, "q_1", question)

	client := &fakeSessionClient{}
	opts := turnCollectOpts{
		client: client, convID: "conv-1", sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello", consent: ct,
	}
	// NOTHING FURTHER HAPPENS: this is the operator away from the terminal with a question on
	// screen, so the deltas that would keep the window open never arrive.
	_, _, _, err := collectTurn(t, client, opts)
	if err == nil {
		t.Fatal("a parked turn must still be bounded — the decision to keep the bound is deliberate")
	}
	msg := err.Error()
	if !strings.Contains(msg, question) {
		t.Errorf("the failure must name the question it was waiting on, so the context survives.\ngot: %s", msg)
	}
	if !strings.Contains(msg, "waiting on YOUR answer") {
		t.Errorf("the failure must name the OPERATOR as what was being waited on.\ngot: %s", msg)
	}
	if strings.Contains(msg, "Default models") || strings.Contains(msg, "may be overloaded") {
		t.Errorf("a turn parked on a question must not be reported as a model fault — that is what "+
			"sent the operator to fix a model that was fine.\ngot: %s", msg)
	}
}
