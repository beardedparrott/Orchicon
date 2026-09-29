package claude

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// Compile-time capability set: Start + the optional capabilities claude
// implements, INCLUDING ChatTurnClient.
//
// Ask chat on claude WAS out of scope and these assertions said so; the
// requirement changed. The interactive profile (ask.go + permissions.go) plus
// the control protocol (stream.go + control.go) is what makes an Ask turn
// possible, so the capability is now required rather than forbidden. The
// assertions are INVERTED rather than deleted: they still pin the contract, in
// the direction that now matters.
var (
	_ scheduler.AdapterBridge    = (*Bridge)(nil)
	_ scheduler.MessageInjector  = (*Bridge)(nil)
	_ scheduler.Aborter          = (*Bridge)(nil)
	_ scheduler.LivenessReporter = (*Bridge)(nil)
	_ scheduler.SessionOwnerKind = (*Bridge)(nil)
	_ scheduler.ChatTurnClient   = (*Bridge)(nil)
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

// The Ask capability is what puts claude in the Ask picker at all: the GUI/TUI
// read Dispatcher.ChatKinds(), which type-asserts exactly this interface. Without
// it claude registers but is never offered for Ask.
func TestBridgeImplementsChatTurnClient(t *testing.T) {
	var b any = New(nil)
	if _, ok := b.(scheduler.ChatTurnClient); !ok {
		t.Fatal("claude Bridge must implement ChatTurnClient — without it an Ask turn cannot be routed to claude and the pickers report it as not Ask-capable")
	}
}

func TestSessionOwnerKindIsClaude(t *testing.T) {
	if got := New(nil).SessionOwnerKind(); got != "claude" {
		t.Fatalf("SessionOwnerKind() = %q, want claude", got)
	}
}

// TestDispatcherChatKindsIncludesClaude pins the contract: registering the
// claude bridge makes it dispatchable (Kinds) AND offered for Ask chat
// (ChatKinds), because the bridge now implements ChatTurnClient. This is the
// surface both pickers read, so it is the difference between claude being
// selectable for Ask and being flagged as unsupported.
func TestDispatcherChatKindsIncludesClaude(t *testing.T) {
	d := scheduler.NewDispatcher()
	d.Register("claude", New(nil))
	kinds := d.Kinds()
	if len(kinds) != 1 || kinds[0] != "claude" {
		t.Fatalf("Kinds() = %v, want [claude]", kinds)
	}
	ck := d.ChatKinds()
	if len(ck) != 1 || ck[0] != "claude" {
		t.Fatalf("ChatKinds() = %v, want [claude] (the Ask picker would otherwise refuse claude)", ck)
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
