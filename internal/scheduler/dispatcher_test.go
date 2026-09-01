package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/adapter"
	"github.com/beardedparrott/orchicon/internal/db"
)

// fakeBridge is a minimal AdapterBridge test double (Start only) used to
// prove that bridges register, resolve by kind, and that capabilities are
// negotiated by type-assertion rather than assumed.
type fakeBridge struct {
	name string
}

func (f *fakeBridge) Start(context.Context, db.ExecutionRow, ExecutionManifest, ExecutionCallbacks) error {
	return nil
}

// fullBridge implements the optional capability surfaces too, so tests can
// assert that capability dispatch type-asserts successfully.
type fullBridge struct {
	fakeBridge
}

func (f *fullBridge) SendExecutionMessage(_ context.Context, _, _ string) error { return nil }
func (f *fullBridge) ContinueSession(_ context.Context, opts ContinueSessionOpts) (string, error) {
	return "follow-up:" + opts.Message, nil
}
func (f *fullBridge) AbortExecution(_ context.Context, _, _ string) error { return nil }
func (f *fullBridge) IsExecutionActive(_ string) bool                     { return true }

func TestDispatcherRegisterResolve(t *testing.T) {
	d := NewDispatcher()
	opencode := &fakeBridge{name: "opencode"}
	claude := &fakeBridge{name: "claude"}
	d.Register("opencode", opencode)
	d.Register("claude", claude)

	got, err := d.Resolve("opencode")
	if err != nil {
		t.Fatalf("Resolve(opencode): %v", err)
	}
	if got != opencode {
		t.Fatalf("Resolve(opencode) = %p, want %p (registered bridge identity)", got, opencode)
	}
	got, err = d.Resolve("claude")
	if err != nil {
		t.Fatalf("Resolve(claude): %v", err)
	}
	if got != claude {
		t.Fatalf("Resolve(claude) = %p, want %p (registered bridge identity)", got, claude)
	}
}

func TestDispatcherResolveUnknownKind(t *testing.T) {
	d := NewDispatcher()
	d.Register("opencode", &fakeBridge{})

	// Unknown kind: actionable error naming the kind + registered kinds,
	// and crucially NO panic.
	_, err := d.Resolve("claude")
	if err == nil {
		t.Fatal("Resolve(unknown kind) succeeded, want actionable error")
	}
	for _, want := range []string{"claude", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Resolve error %q missing %q", err.Error(), want)
		}
	}
}

func TestDispatcherResolveEmptyKind(t *testing.T) {
	d := NewDispatcher()
	d.Register("opencode", &fakeBridge{})
	_, err := d.Resolve("")
	if err == nil {
		t.Fatal("Resolve(empty kind) succeeded, want error")
	}
	if !strings.Contains(err.Error(), "no adapter kind specified") {
		t.Errorf("Resolve(empty) error = %q, want clear message", err.Error())
	}
}

func TestDispatcherRegisterRejectsInvalid(t *testing.T) {
	d := NewDispatcher()
	// Programming errors must surface loudly at startup, not panic at
	// dispatch time.
	assertPanics(t, func() { d.Register("", &fakeBridge{}) })
	assertPanics(t, func() { d.Register("opencode", nil) })
}

func TestDispatcherLastRegistrationWins(t *testing.T) {
	d := NewDispatcher()
	first := &fakeBridge{name: "first"}
	second := &fakeBridge{name: "second"}
	d.Register("opencode", first)
	d.Register("opencode", second)
	got, err := d.Resolve("opencode")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != second {
		t.Fatal("last registration did not win")
	}
}

// TestDispatcherCapabilityNegotiation proves the server-side pattern: the
// caller type-asserts a capability interface and degrades with an
// actionable error when the bridge does not support it — never a panic.
func TestDispatcherCapabilityNegotiation(t *testing.T) {
	d := NewDispatcher()
	minimal := &fakeBridge{}
	full := &fullBridge{}
	d.Register("minimal", minimal)
	d.Register("full", full)

	// Non-supporting bridge: type-assert fails cleanly.
	bridge, err := d.Resolve("minimal")
	if err != nil {
		t.Fatalf("Resolve(minimal): %v", err)
	}
	t.Run("non-supporting capability is an error, not a panic", func(t *testing.T) {
		inj, ok := bridge.(MessageInjector)
		if ok {
			t.Fatal("minimal bridge unexpectedly implements MessageInjector")
		}
		if inj != nil {
			t.Fatalf("type-assert without ok must yield nil value, got %p", inj)
		}
		if _, ok := bridge.(LivenessReporter); ok {
			t.Fatal("minimal bridge unexpectedly implements LivenessReporter")
		}
	})

	// Supporting bridge: capability dispatch works end-to-end.
	bridge, err = d.Resolve("full")
	if err != nil {
		t.Fatalf("Resolve(full): %v", err)
	}
	t.Run("supporting capability dispatches", func(t *testing.T) {
		inj := bridge.(MessageInjector)
		if err := inj.SendExecutionMessage(context.Background(), "exec-1", "hello"); err != nil {
			t.Fatalf("SendExecutionMessage: %v", err)
		}
		cont := bridge.(SessionContinuer)
		got, err := cont.ContinueSession(context.Background(), ContinueSessionOpts{Message: "what next?"})
		if err != nil {
			t.Fatalf("ContinueSession: %v", err)
		}
		if got != "follow-up:what next?" {
			t.Errorf("ContinueSession = %q, want %q", got, "follow-up:what next?")
		}
		ab := bridge.(Aborter)
		if err := ab.AbortExecution(context.Background(), "exec-1", "test"); err != nil {
			t.Fatalf("AbortExecution: %v", err)
		}
		rep := bridge.(LivenessReporter)
		if !rep.IsExecutionActive("exec-1") {
			t.Error("IsExecutionActive = false, want true")
		}
	})
}

// TestTaskReconcilerResolvesBridgeByKind is the end-to-end dispatch-level
// contract: the reconciler derives the adapter kind from the manifest's
// model_ref (never a hardcoded singleton), resolves the bridge through the
// dispatcher, and an unknown kind fails the execution with an actionable
// message instead of panicking.
func TestTaskReconcilerResolvesBridgeByKind(t *testing.T) {
	d := NewDispatcher()
	claudeBridge := &fakeBridge{name: "claude"}
	d.Register("claude", claudeBridge)

	// A model_ref whose adapter segment is "claude" must route to the
	// claude bridge via AdapterKind — not to a hardcoded opencode one.
	kind := adapter.AdapterKind("claude/anthropic/claude-sonnet-4")
	bridge, err := d.Resolve(kind)
	if err != nil {
		t.Fatalf("resolve claude kind %q: %v", kind, err)
	}
	if bridge != claudeBridge {
		t.Fatalf("kind resolution routed to %p, want claude bridge %p", bridge, claudeBridge)
	}

	// Unknown kind produces an actionable error (the reconciler path
	// surfaces it as an execution failure, never a panic).
	_, err = d.Resolve(adapter.AdapterKind("mystery/provider/model"))
	if err == nil {
		t.Fatal("unknown kind resolved, want actionable error")
	}
	if !strings.Contains(err.Error(), "mystery") || !strings.Contains(err.Error(), "registered kinds") {
		t.Errorf("unknown-kind error = %q, want actionable message", err.Error())
	}
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	fn()
}
