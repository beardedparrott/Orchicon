package orchicon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestConsentDenialErrorDistinguishesTheThreeOutcomes is the operator's question as
// a test: "if I let a permission ask time out and then I asked for the prompt again,
// would it now be accepted, or is it deny forever at that point?"
//
// It is NOT deny-forever — a timeout writes no grant, no deny entry and no
// once-target, so a retried call is a new call which asks again. But the model has
// to be TOLD that, and it used to receive identical text for "the operator refused
// you" and "nobody answered", so it had no reason to ask again. These messages are
// read by the model, and they must not be interchangeable.
func TestConsentDenialErrorDistinguishesTheThreeOutcomes(t *testing.T) {
	expired := consentDenialError("write", consentExpired).Error()
	cancelled := consentDenialError("write", consentCancelled).Error()
	denied := consentDenialError("write", "reject").Error()

	// A timeout must INVITE a retry, because that is the whole point: nothing is
	// permanently denied and the same call will ask again.
	if !strings.Contains(expired, "retry") || !strings.Contains(expired, "ask again") {
		t.Errorf("the expired message must tell the model a retry will ask again, got %q", expired)
	}
	// A refusal must NOT.
	if strings.Contains(denied, "retry the call") {
		t.Errorf("the denied message must not invite a retry, got %q", denied)
	}
	if !strings.Contains(denied, "denied") {
		t.Errorf("the denied message must say the operator refused, got %q", denied)
	}
	// And the three must not be interchangeable.
	if expired == denied || expired == cancelled || denied == cancelled {
		t.Fatalf("the outcomes are not distinguishable:\n  expired=%q\n  denied=%q\n  cancelled=%q", expired, denied, cancelled)
	}
}

// TestConsentReadOnlyToolsNeverAsk is the rule at the heart of the gate: a read
// cannot change anything, so it never asks.
func TestConsentReadOnlyToolsNeverAsk(t *testing.T) {
	for _, name := range []string{"write", "edit", "batch_write", "bash"} {
		if !consentGatedTool(name) {
			t.Errorf("%q must be gated — it changes the world", name)
		}
	}
	for _, name := range []string{"read", "batch_read", "grep", "glob", "list", "ask_user", "ask_file_root"} {
		if consentGatedTool(name) {
			t.Errorf("%q must NOT be gated — reads never ask", name)
		}
	}
	// todowrite writes SESSION state, not the filesystem, so it must NOT ask: a card for
	// a change that cannot leave the session is how a gate becomes something to click
	// through. (It was briefly classed as mutating "for completeness"; using the feature
	// showed that was wrong.)
	if consentGatedTool("todowrite") {
		t.Error("todowrite must not ask — it cannot change anything outside the session")
	}
}

// TestOpaqueMCPToolsAreNeverConsentGated pins the CONSENT half for MCP, which now ALLOWS the class.
//
// It used to be its own inverse (`AreAlwaysGated`): an operator's MCP tool was treated as a
// third-party action the platform cannot classify, gated in every mode. The operator overruled it —
// the server is in the conversation because they attached it, so consent was spent at configuration
// time. The MODE table was already the governor for the platform's own surface and remains it for
// everyone's.
func TestOpaqueMCPToolsAreNeverConsentGated(t *testing.T) {
	// THE REQUIREMENT FLIPPED, and the assertion flipped with it rather than being deleted. An MCP
	// server is in this conversation only because the operator put it there, so the approval already
	// happened at configuration time; a per-call card asks them to re-decide a standing decision.
	// The operator: "If it's added in scope it should just have access tbh. I don't want cards for
	// that."
	//
	// WHAT STILL GOVERNS ONE is the MODE boundary, which is a separate layer and is NOT asserted from
	// here — see internal/askmode's MayExecute (an opaque MCP tool takes its mode's MayAct, so
	// Brainstorm and Quick Work still refuse it) and native_tools.go, which enforces that at both the
	// OFFER and the EXECUTE end. Pinning it THERE is what keeps this test honest: this one claims only
	// that consent is not the thing doing the governing.
	for _, name := range []string{
		"mcp__github__create_issue", "mcp__sentry__list_issues", "mcp__notes__append",
	} {
		if consentGatedTool(name) {
			t.Errorf("%q must NOT be consent-gated — the server is in scope because the operator attached it, "+
				"so a card here asks them to re-decide a decision they already made", name)
		}
	}
	for _, name := range []string{
		"mcp__orchicon__create_work_item", "mcp__orchicon__get_current_conversation", "orchicon_list_projects",
	} {
		if consentGatedTool(name) {
			t.Errorf("%q must NOT be gated by consent — the mode table governs the platform's own surface", name)
		}
	}
	// AND THE HOST SUITE IS UNAFFECTED by that change: a write or a command still asks, which is the
	// half of this gate that the operator has never disputed.
	for _, name := range []string{"write", "edit", "batch_write", "bash"} {
		if !consentGatedTool(name) {
			t.Errorf("%q must STILL be gated — allowing MCP must not loosen the file/shell suite", name)
		}
	}
}

// pendingPermIDs returns the ask ids currently parked, for a test that needs to
// answer one.
func pendingPermIDs(b *NativeBridge) []string {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	out := make([]string, 0, len(b.permWaits))
	for id := range b.permWaits {
		out = append(out, id)
	}
	return out
}

// TestAwaitConsentPermissionProceedsOnOnce: a "once" decision from the collector
// lets the call run. This is the whole path the permission card drives.
func TestAwaitConsentPermissionProceedsOnOnce(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "5s")
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- b.awaitConsentPermission(context.Background(), bus,
			ToolCall{ToolCallID: "tc-1", Name: "bash", ArgsJSON: `{"command":"ls"}`}, `{"command":"ls"}`)
	}()

	id := waitForAsk(t, b)
	// The ask must have reached the bus as a TYPED permission event, carrying the
	// action — a card cannot be drawn from an ask id alone.
	evt := waitForBusEvent(t, bus, "permission")
	if evt.PermissionID != id {
		t.Errorf("bus ask id = %q, want the parked id %q", evt.PermissionID, id)
	}
	if evt.Tool != "bash" || evt.InputJSON != `{"command":"ls"}` {
		t.Errorf("the ask carried %+v, want the tool and its arguments", evt)
	}

	if err := b.ReplyPermissionDecision(context.Background(), "sess", id, "once"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	select {
	case d := <-done:
		if d != "once" {
			t.Fatalf("decision = %q, want \"once\"", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a delivered decision did not release the parked call")
	}
}

// TestAwaitConsentPermissionDeniesOnSilence pins the CONFIGURED leash: an operator who
// sets ORCHICON_ASK_CONSENT_WAIT gets the old fail-closed expiry, and silence still
// resolves the call as "expired" rather than as an approval.
//
// IT IS NO LONGER THE DEFAULT. The default is now no bound at all (see
// nativeConsentWaitDefault): the operator, away from the keyboard, was losing cards to
// exactly this expiry — "if an ask or permission card has been waiting for awhile (i.e.
// away from keyboard), I can no longer click into it or use the keyboard to select it" —
// and decided that a card waits for the user no matter what. This test therefore guards
// the SETTING rather than the behaviour every card now gets.
func TestAwaitConsentPermissionDeniesOnSilence(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "80ms")
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	start := time.Now()
	d := b.awaitConsentPermission(context.Background(), bus,
		ToolCall{ToolCallID: "tc-1", Name: "write", ArgsJSON: `{"filePath":"/tmp/x"}`}, `{"filePath":"/tmp/x"}`)
	// "expired", not "reject": the operator did not refuse, and the model is told
	// that a retry will ask again. What must NEVER happen is "once".
	if d == "once" {
		t.Fatalf("silence resolved to %q — silence must never be approval", d)
	}
	if d != consentExpired {
		t.Fatalf("decision = %q, want %q", d, consentExpired)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the wait took %s; it must honour the configured bound", elapsed)
	}
	// The waiter must be gone, so a late decision is inert rather than a leak.
	if ids := pendingPermIDs(b); len(ids) != 0 {
		t.Fatalf("the timed-out ask is still parked: %v", ids)
	}
}

// TestAwaitConsentPermissionWaitsIndefinitelyByDefault is the operator's rule as a test: with
// nothing configured, a card is NEVER taken away from them.
//
// The whole bug was the expiry: when the wait ran out the call was refused, the ask was settled,
// and the card the operator came back to could no longer be clicked or selected. So the
// assertions here are (1) there is no bound to run out, (2) an absent operator leaves the ask
// PARKED, and (3) their eventual answer still lands and releases the call.
func TestAwaitConsentPermissionWaitsIndefinitelyByDefault(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "") // the default: unset
	if got := nativeConsentWait(); got != 0 {
		t.Fatalf("nativeConsentWait() = %s with nothing configured, want no bound — a card must not "+
			"expire on an operator who has stepped away", got)
	}
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	type result struct{ decision string }
	done := make(chan result, 1)
	go func() {
		done <- result{b.awaitConsentPermission(context.Background(), bus,
			ToolCall{ToolCallID: "tc-1", Name: "write", ArgsJSON: `{"filePath":"/tmp/x"}`}, `{"filePath":"/tmp/x"}`)}
	}()

	id := waitForAsk(t, b)
	// THE OPERATOR IS AWAY. Nothing is answered, and the wait must simply hold — well past any
	// configured test-scale bound, so a timer that existed would have fired.
	select {
	case r := <-done:
		t.Fatalf("the parked call resolved as %q while nobody had answered — the operator's card is "+
			"being taken away again", r.decision)
	case <-time.After(600 * time.Millisecond):
	}
	if !askStillParked(b, id) {
		t.Fatal("the ask is no longer parked: the card would be settled and unanswerable")
	}

	// THEY RETURN AND ANSWER. It lands — not refused as late, which was the other half of the
	// report — and the call is released with their decision.
	if err := b.ReplyPermissionDecision(context.Background(), "sess", id, "once"); err != nil {
		t.Fatalf("reply: %v", err)
	}
	select {
	case r := <-done:
		if r.decision != "once" {
			t.Fatalf("decision = %q, want \"once\" — a late answer must still be the operator's answer", r.decision)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a delivered decision did not release the call — the wait outlived its own ask")
	}
}

// askStillParked reports whether the named ask is still in the bridge's wait registry.
func askStillParked(b *NativeBridge, id string) bool {
	b.permMu.Lock()
	defer b.permMu.Unlock()
	_, ok := b.permWaits[id]
	return ok
}

// TestAwaitConsentPermissionDeniesOnCancelledTurn: a Stop / supersede must
// release the wait at once, not hold the goroutine for the full window.
func TestAwaitConsentPermissionDeniesOnCancelledTurn(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "10m")
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	ctx, cancelTurn := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		done <- b.awaitConsentPermission(ctx, bus,
			ToolCall{ToolCallID: "tc-1", Name: "bash", ArgsJSON: `{}`}, `{}`)
	}()
	waitForAsk(t, b)
	cancelTurn()

	select {
	case d := <-done:
		if d == "once" {
			t.Fatalf("a cancelled turn resolved to %q — it must never be an approval", d)
		}
		if d != consentCancelled {
			t.Fatalf("decision = %q, want %q for a cancelled turn", d, consentCancelled)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled turn did not release the consent wait")
	}
}

// TestReplyPermissionDecisionForAMootAskIsNotAnError: a decision for an ask
// nobody is waiting on (expired, or the turn already ended) must not poison the
// caller — there is nothing it could do about it.
func TestReplyPermissionDecisionForAMootAskIsNotAnError(t *testing.T) {
	b, _, cancel := newConsentTestBridge(t)
	defer cancel()
	if err := b.ReplyPermissionDecision(context.Background(), "sess", "native-ask-999", "once"); err != nil {
		t.Fatalf("a decision for an unknown ask returned %v, want nil", err)
	}
}

// TestReplyPermissionAutoApprovesOnlyTheNoConsentFallback: ReplyPermission is the
// collector's fallback for a turn with no consent core at all. It must release a
// parked call rather than leaving it to time out (the old stub returned an error,
// which would have hung every such turn until the window expired).
func TestReplyPermissionAutoApprovesOnlyTheNoConsentFallback(t *testing.T) {
	t.Setenv("ORCHICON_ASK_CONSENT_WAIT", "10m")
	b, bus, cancel := newConsentTestBridge(t)
	defer cancel()

	done := make(chan string, 1)
	go func() {
		done <- b.awaitConsentPermission(context.Background(), bus,
			ToolCall{ToolCallID: "tc-1", Name: "write", ArgsJSON: `{}`}, `{}`)
	}()
	id := waitForAsk(t, b)
	if err := b.ReplyPermission(context.Background(), "sess", id); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	select {
	case d := <-done:
		if d != "once" {
			t.Fatalf("decision = %q, want \"once\" from the fallback", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fallback did not release the parked call")
	}
}

// --- helpers ---------------------------------------------------------------

// newConsentTestBridge builds a bridge with no provider: the consent path needs
// only the bus and the wait registry, so no turn is started.
func newConsentTestBridge(t *testing.T) (*NativeBridge, *chatBus, context.CancelFunc) {
	t.Helper()
	b := newChatBridge(t, &chatTestProvider{})
	bus := newChatBus()
	ctx, cancel := context.WithCancel(context.Background())
	_ = ctx
	return b, bus, cancel
}

// waitForAsk polls until the bridge has parked an ask, and returns its id.
func waitForAsk(t *testing.T, b *NativeBridge) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ids := pendingPermIDs(b); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no allow was parked")
	return ""
}

// waitForBusEvent reads until an event of the given kind arrives, so the test
// asserts on what the collector would actually receive.
func waitForBusEvent(t *testing.T, bus *chatBus, kind string) scheduler.SessionEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-bus.Events():
			if evt.Kind == kind {
				return evt
			}
		case <-deadline:
			t.Fatalf("no %q event reached the bus", kind)
			return scheduler.SessionEvent{}
		}
	}
}

// TestConsentDenialErrorDoesNotBlameTheOperatorForARule is the OTHER half of the
// distinction above, and the one that was missing.
//
// The consent layer refuses a call for reasons that are not operator decisions at all: a
// DENY entry (a decision the POLICY made), the never-allow binary class, or a policy that
// could not be read. In every one of those cases the operator never saw the call — no card
// was raised — and the layer already computed a precise reason naming the rule. But the
// collector sent the bridge a bare "reject" and logged the reason, so the model was told
// "the operator denied ... do not retry it; ask them what they would prefer instead" about
// a refusal the operator had nothing to do with.
//
// That is the exact failure this repo has been corrected on before (a malformed policy was
// reported as an operator denial), and a model that believes the operator refused something
// they never saw will ask them to lift a rule they did not know existed.
func TestConsentDenialErrorDoesNotBlameTheOperatorForARule(t *testing.T) {
	reason := "denied by the permission list (deny entry \"~/.secrets/**\")"
	got := consentDenialError("read_file", ConsentRefusedPrefix+reason).Error()

	if !strings.Contains(got, reason) {
		t.Errorf("the refusal must CARRY the reason the layer computed, got %q", got)
	}
	if !strings.Contains(got, "did not refuse") || !strings.Contains(got, "never asked") {
		t.Errorf("the message must say the operator was not involved, got %q", got)
	}
	// And it must not send the model to the operator for permission: there is no
	// permission to give. The route out is a different approach or fixing the rule.
	if strings.Contains(got, "ask them what they would prefer") {
		t.Errorf("a rule refusal must not be attributed to the operator, got %q", got)
	}
	// A GENUINE operator decision keeps the old wording — including the instruction not
	// to retry, because the operator saying no is an answer rather than an accident.
	if denied := consentDenialError("read_file", "reject").Error(); !strings.Contains(denied, "the operator denied") {
		t.Errorf("a real operator denial must still say so, got %q", denied)
	}
	// THE PREFIX MUST NOT MATCH BY ACCIDENT. Only a prefix-carrying decision may claim the
	// operator was not involved; every other value is either an operator decision or a
	// non-refusal, and neither may borrow that wording.
	for _, plain := range []string{"reject", consentExpired, consentCancelled, "once"} {
		if strings.Contains(consentDenialError("t", plain).Error(), "did not refuse") {
			t.Errorf("decision %q claimed the operator was not involved", plain)
		}
	}
}
