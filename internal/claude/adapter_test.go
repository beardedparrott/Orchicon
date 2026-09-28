package claude

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time capability set: Start + the optional capabilities claude
// implements. ChatTurnClient is deliberately ABSENT (Ask chat on claude is
// out of scope).
var (
	_ scheduler.AdapterBridge    = (*Bridge)(nil)
	_ scheduler.MessageInjector  = (*Bridge)(nil)
	_ scheduler.Aborter          = (*Bridge)(nil)
	_ scheduler.LivenessReporter = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind = (*Bridge)(nil)
)

// capability is the anonymous interface matching scheduler.ConfigurableBridge's
// usage/session-store setters (its SetRuntimeClient takes the scheduler's
// RuntimeClient interface, which opencode's concrete setter also does not
// satisfy — so we assert the two setters that DO match).
type usageRecorderSetter interface {
	SetUsageRecorder(scheduler.UsageRecorderFunc)
}
type sessionStoreSetter interface {
	SetSessionStore(scheduler.SessionStoreFunc)
}

var (
	_ usageRecorderSetter = (*Bridge)(nil)
	_ sessionStoreSetter  = (*Bridge)(nil)
)

// The concrete runtime setter mirrors opencode's (takes *runtime.Client so
// the streaming method need not be added to scheduler.RuntimeClient).
var _ = func(b *Bridge, rt *runtime.Client) { b.SetRuntimeClient(rt) }

func TestBridgeDoesNotImplementChatTurnClient(t *testing.T) {
	var b any = New(nil)
	if _, ok := b.(scheduler.ChatTurnClient); ok {
		t.Fatal("claude Bridge must NOT implement ChatTurnClient (Ask chat on claude is out of scope)")
	}
}

func TestSessionOwnerKindIsClaude(t *testing.T) {
	if got := New(nil).SessionOwnerKind(); got != "claude" {
		t.Fatalf("SessionOwnerKind() = %q, want claude", got)
	}
}

// TestDispatcherChatKindsOmitsClaude pins the contract: registering the
// claude bridge makes it dispatchable (Kinds) but NOT offered for Ask chat
// (ChatKinds), so the GUI/TUI show the honest "no Ask chat" state.
func TestDispatcherChatKindsOmitsClaude(t *testing.T) {
	d := scheduler.NewDispatcher()
	d.Register("claude", New(nil))
	kinds := d.Kinds()
	if len(kinds) != 1 || kinds[0] != "claude" {
		t.Fatalf("Kinds() = %v, want [claude]", kinds)
	}
	if ck := d.ChatKinds(); len(ck) != 0 {
		t.Fatalf("ChatKinds() = %v, want empty (claude has no ChatTurnClient)", ck)
	}
}

func TestSetSpawnOverrideAndLiveness(t *testing.T) {
	b := New(nil)
	if b.IsExecutionActive("nope") {
		t.Fatal("no executions must be active on a fresh bridge")
	}
	if err := b.SendExecutionMessage(context.Background(), "nope", "hi"); err == nil {
		t.Fatal("message injection with no live session must error (never panic)")
	}
	if err := b.AbortExecution(context.Background(), "nope", "cancel"); err != nil {
		t.Fatalf("abort of unknown execution must be a no-op, got %v", err)
	}
}
