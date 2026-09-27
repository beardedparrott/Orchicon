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
	// todowrite is a WRITE (to session state, not the filesystem) and is classified
	// as mutating for completeness; it must therefore ask like any other mutating
	// host tool rather than riding the product-tool exemption.
	if !consentGatedTool("todowrite") {
		t.Error("todowrite is classified mutating, so it must ask")
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

// TestAwaitConsentPermissionDeniesOnSilence is the operator's choice, as a test:
// an unanswered ask must FAIL CLOSED. Without the bound, a session nobody is
// watching holds a turn and its runtime container forever.
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
