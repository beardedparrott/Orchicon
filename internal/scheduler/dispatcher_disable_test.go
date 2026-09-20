package scheduler

// Tests for per-adapter enable/disable (AC 3): disabling a kind must fail
// dispatches LOUDLY with a reason, and the caller must be able to tell a
// permanent routing failure (retrying cannot help) from a transient one.

import (
	"errors"
	"strings"
	"testing"
)

func TestDispatcherDisableFailsResolveLoudly(t *testing.T) {
	d := NewDispatcher()
	d.Register("opencode", &fakeBridge{name: "opencode"})
	d.Register("orchicon", &fakeBridge{name: "orchicon"})

	// Baseline: enabled kinds resolve.
	if _, err := d.Resolve("orchicon"); err != nil {
		t.Fatalf("Resolve(orchicon) before Disable = %v, want nil", err)
	}

	d.Disable("orchicon")
	b, err := d.Resolve("orchicon")
	if err == nil {
		t.Fatal("Resolve of a disabled kind succeeded, want an error")
	}
	if b != nil {
		t.Error("Resolve of a disabled kind returned a bridge, want nil")
	}
	if !errors.Is(err, ErrAdapterDisabled) {
		t.Errorf("Resolve error = %v, want ErrAdapterDisabled (permanent: the reconciler blocks instead of requeueing)", err)
	}
	// The reason is operator-facing: it must name the kind and the switch.
	if !strings.Contains(err.Error(), "orchicon") || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("Resolve error %q must name the kind and say it is disabled", err)
	}
	// Other kinds are untouched.
	if _, err := d.Resolve("opencode"); err != nil {
		t.Errorf("Resolve(opencode) after disabling orchicon = %v, want nil", err)
	}
	if got := d.DisabledKinds(); len(got) != 1 || got[0] != "orchicon" {
		t.Errorf("DisabledKinds() = %v, want [orchicon]", got)
	}
}

// TestDispatcherDisableIsIdempotentAndIgnoresEmpty pins the seeding safety:
// the server seeds from a comma-separated env var, so an empty entry (a
// trailing comma, an unset var) must be a no-op rather than disabling the
// empty kind.
func TestDispatcherDisableIsIdempotentAndIgnoresEmpty(t *testing.T) {
	d := NewDispatcher()
	d.Disable("")
	d.Disable("claude")
	d.Disable("claude")
	if got := d.DisabledKinds(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("DisabledKinds() = %v, want [claude]", got)
	}
	if got := NewDispatcher().DisabledKinds(); got == nil || len(got) != 0 {
		t.Errorf("fresh Dispatcher DisabledKinds() = %v, want empty non-nil", got)
	}
}

// TestDispatcherUnregisteredKindIsPermanent pins the declared-but-unavailable
// case ("claude" is a DECLARED kind in internal/adapter/providers.go but is not
// dispatcher-registered): it must be identifiable as a PERMANENT routing
// failure so the reconciler blocks the work item instead of requeueing
// forever, and it must never silently fall back to another adapter.
func TestDispatcherUnregisteredKindIsPermanent(t *testing.T) {
	d := NewDispatcher()
	d.Register("orchicon", &fakeBridge{name: "orchicon"})
	b, err := d.Resolve("claude")
	if err == nil || b != nil {
		t.Fatalf("Resolve(claude) = (%v, %v), want (nil, error) — no silent fallback", b, err)
	}
	if !errors.Is(err, ErrAdapterKindUnregistered) {
		t.Errorf("Resolve(claude) error = %v, want ErrAdapterKindUnregistered", err)
	}
	// The message an operator sees is unchanged in substance: it names the
	// kind and the registered kinds.
	if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "orchicon") {
		t.Errorf("Resolve error %q must name the missing kind and the registered kinds", err)
	}
}
