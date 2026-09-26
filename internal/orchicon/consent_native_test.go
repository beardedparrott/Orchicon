package orchicon

import (
	"context"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// TestConsentGatedToolGatesWritesAndExecutionsOnly pins the settled rule: a write
// or an execution is approved before it runs, a READ never is. Prompting for a
// read would train the operator to approve without looking, which is the failure
// the gate exists to prevent.
func TestConsentGatedToolGatesWritesAndExecutionsOnly(t *testing.T) {
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
	if d != "reject" {
		t.Fatalf("decision = %q, want \"reject\" — silence must never be approval", d)
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
		if d != "reject" {
			t.Fatalf("decision = %q, want \"reject\" for a cancelled turn", d)
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
