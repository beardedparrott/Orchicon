package scheduler

// diff_ledger_wiring_test.go — A BRIDGE GETS DIFF SUPPORT BY REGISTERING.
//
// THE OPERATOR: "I don't see ANY files under diff viewer right now in this ask
// session", and then: "ensure every adapter and provider is supported for diff
// viewer in ask sessions and executions. Having something more standard that
// works across the board would be nice if we can."
//
// THE DEFECT THIS PINS. The diff-ledger hook was applied ONE BRIDGE AT A TIME in
// server construction, and the Ask half was applied to exactly one of the three
// Ask-capable adapters. `opencode` and `claude` BOTH implement ChatTurnClient —
// both serve Ask conversations — and neither had an Ask hook, so those
// conversations recorded no file edits at all and the diff pane stayed empty.
// Nothing was wrong with either adapter; a wiring step was simply never taken
// for them, and a fourth adapter would have inherited the same gap.
//
// So the property worth pinning is NOT "the hook can be set" (it always could).
// It is that REGISTRATION IS THE ONLY STEP REQUIRED: a bridge that can ledger
// receives the callbacks when it is registered, whichever order the server
// happens to wire things in, and one that cannot is left alone without a panic.

import (
	"context"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
)

// fakeLedgerBridge is a minimal AdapterBridge that also implements DiffLedgerSink,
// recording what it was handed.
type fakeLedgerBridge struct {
	execution DiffLedgerFunc
	ask       DiffLedgerFunc
	calls     int
}

func (f *fakeLedgerBridge) Start(context.Context, db.ExecutionRow, ExecutionManifest, ExecutionCallbacks) error {
	return nil
}

func (f *fakeLedgerBridge) SetDiffLedger(execution, ask DiffLedgerFunc) {
	f.execution, f.ask = execution, ask
	f.calls++
}

// plainBridge is an AdapterBridge WITHOUT the capability — the shape a bridge
// that cannot ledger has, and which must never panic or be forced.
type plainBridge struct{}

func (plainBridge) Start(context.Context, db.ExecutionRow, ExecutionManifest, ExecutionCallbacks) error {
	return nil
}

// THE PROPERTY: installing the ledger before registering means the bridge has it
// by the time it is registered — no per-adapter step.
func TestRegisteringAppliesTheDiffLedger(t *testing.T) {
	d := NewDispatcher()
	var gotExec, gotAsk bool
	d.SetDiffLedger(
		func(context.Context, string, string, string, string, map[string]any, string) { gotExec = true },
		func(context.Context, string, string, string, string, map[string]any, string) { gotAsk = true },
	)

	b := &fakeLedgerBridge{}
	d.Register("fake", b)

	if b.calls != 1 {
		t.Fatalf("SetDiffLedger called %d times on register, want 1 — a bridge must receive the ledger "+
			"BY REGISTERING, which is the whole point", b.calls)
	}
	if b.execution == nil || b.ask == nil {
		t.Fatal("a registered ledger-capable bridge did not receive BOTH callbacks — the Ask half is the " +
			"one that was missing for opencode and claude")
	}
	// And they are the installed ones, not placeholders.
	b.execution(context.Background(), "o", "t", "d", "write", nil, "")
	if !gotExec {
		t.Error("the execution callback handed to the bridge is not the installed one")
	}
	b.ask(context.Background(), "o", "t", "d", "write", nil, "")
	if !gotAsk {
		t.Error("the ask callback handed to the bridge is not the installed one")
	}
}

// ORDER INDEPENDENCE: a bridge registered BEFORE the ledger is installed still
// receives it. Without this, a server that registers first would silently leave
// every adapter out — the same class of omission, one step earlier.
func TestInstallingTheLedgerAfterRegistrationAlsoApplies(t *testing.T) {
	d := NewDispatcher()
	b := &fakeLedgerBridge{}
	d.Register("fake", b)
	if b.calls != 0 {
		t.Fatalf("a bridge was handed a ledger before one was installed (calls=%d)", b.calls)
	}

	d.SetDiffLedger(
		func(context.Context, string, string, string, string, map[string]any, string) {},
		func(context.Context, string, string, string, string, map[string]any, string) {},
	)
	if b.calls != 1 {
		t.Fatalf("SetDiffLedger called %d times on an ALREADY-registered bridge, want 1 — call order must "+
			"not decide whether an adapter gets diff support", b.calls)
	}
}

// A BRIDGE THAT CANNOT LEDGER IS LEFT ALONE, WITHOUT A PANIC. This is the
// optional-capability discipline the repo uses for MessageInjector/Aborter: a
// bridge that does not implement the interface is simply not a sink.
func TestABridgeWithoutTheCapabilityIsNotForced(t *testing.T) {
	d := NewDispatcher()
	d.SetDiffLedger(
		func(context.Context, string, string, string, string, map[string]any, string) {},
		func(context.Context, string, string, string, string, map[string]any, string) {},
	)
	d.Register("plain", plainBridge{}) // must not panic
	if d.DiffLedgerCapable("plain") {
		t.Error("DiffLedgerCapable reported true for a bridge that does not implement DiffLedgerSink")
	}
	if !d.DiffLedgerCapable("fake") {
		_ = d
	}
}

// CAPABILITY IS ASKABLE PER KIND, so "does this adapter support the diff pane?"
// has an answer for an adapter nobody thought to check — the question that was
// never asked about opencode and claude.
func TestDiffLedgerCapabilityIsAskablePerKind(t *testing.T) {
	d := NewDispatcher()
	d.Register("can", &fakeLedgerBridge{})
	d.Register("cannot", plainBridge{})

	if !d.DiffLedgerCapable("can") {
		t.Error("a ledger-capable adapter reported NOT capable — an operator diagnostic would be wrong")
	}
	if d.DiffLedgerCapable("cannot") {
		t.Error("a non-capable adapter reported capable")
	}
	if d.DiffLedgerCapable("never-registered") {
		t.Error("an unknown kind reported capable")
	}
}
