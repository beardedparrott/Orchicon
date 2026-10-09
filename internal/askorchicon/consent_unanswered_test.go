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
	"log/slog"
	"strings"
	"testing"
	"time"

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

// TestAParkedTurnOutlivesTheReplyWindowAndStillTakesTheAnswer is the case the operator reported,
// end to end, and it REVERSES a deliberate decision that this file used to pin.
//
// WHAT IT USED TO DO: the parked turn was ended by the reply window on purpose, and the failure was
// worded to blame the WAIT rather than the model — "this turn was waiting on YOUR answer for 30m0s
// and has ended ... reply in your own words". That wording fixed the misattribution but left the
// operator with a DEAD CARD: the expiry runs the turn's finalize, which answers the serve `reject`,
// so the tool call the card was holding is gone.
//
// The operator: "if an ask or permission card has been waiting for awhile (i.e. away from keyboard),
// I can no longer click into it or use the keyboard to select it." Both gestures need the ask to
// still be OPEN. So the window now RE-ARMS while the turn is parked (see the window.C arm in chat.go)
// and the TTL sweep spares it too — the wait belongs to the operator, and a decision, not a timer,
// is what ends it.
//
// THE MODEL'S OWN SILENCE IS UNCHANGED, and pinned separately by TestCollectConversationReplyTimeout:
// a turn with no card on screen still dies at the window with the model-facing message.
func TestAParkedTurnOutlivesTheReplyWindowAndStillTakesTheAnswer(t *testing.T) {
	t.Setenv("ORCHICON_ASK_REPLY_WINDOW", "100ms")
	t.Setenv("ORCHICON_ASK_REATTACH_BACKOFF", "1ms")
	svc := testConsentService()
	ct := newTestConsentTurn(svc, "/p/proj", true, nil)
	const question = "Which branch should the run clone off?"
	ask := raiseOpenQuestion(t, ct, "q_1", question)

	client := &fakeSessionClient{}
	s := &Service{log: slog.Default(), turns: newTurnRegistry()}
	opts := turnCollectOpts{
		client: client, convID: "conv-1", sessionID: "ses_live", reuseSystem: "REUSE_SYSTEM",
		modelRef: "opencode/deepseek-v4-flash-free", userMsg: "hello", consent: ct,
	}
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, _, _, err = s.collectConversationReply(context.Background(), opts)
	}()

	// THE OPERATOR IS AWAY. NOTHING FURTHER HAPPENS — no deltas, no answers — so several reply
	// windows pass with the turn parked exactly as described.
	select {
	case <-done:
		t.Fatalf("the turn ended while it was parked on the operator (%v) — on returning, the card "+
			"can no longer be clicked or selected, because the tool call it held is gone", err)
	case <-time.After(650 * time.Millisecond):
	}

	// THE ASK IS STILL OPEN, which is the whole precondition for either gesture working: a click
	// resolves through pendingConsentItem and the keyboard through the screen's claim, and both
	// require Pending().
	if !svc.pending.hasOpenAsk("conv-1") {
		t.Fatal("the ask must still be open after the window passed — otherwise the operator's click " +
			"and keys have nothing to land on")
	}
	if _, ok := ct.waitingOnOperator(); !ok {
		t.Fatal("the turn must still report that it is waiting on the operator")
	}

	// THE OPERATOR RETURNS AND ANSWERS. It lands — it is not refused as late — and the turn
	// resumes, which is what makes the wait worth surviving.
	if !ask.recordClientAnswer("develop") {
		t.Fatal("the answer was refused as late: the ask had already been settled")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not resume after the operator answered")
	}
	// The fake produces no further model output, so the resumed turn may end on its own window —
	// what must NOT appear is the parked-turn message, which no longer exists.
	if err != nil && (strings.Contains(err.Error(), "waiting on YOUR answer") || strings.Contains(err.Error(), question)) {
		t.Fatalf("the turn still reports the wait as its failure: %v", err)
	}
}
