package opencode

// Tests for the LAZY host serve (AC 1/AC 2/AC 4): the serve is demand-keyed — it starts on the
// first opencode demand and never as a boot side effect.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func lazyServeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEnsureStartedKillSwitchFailsFast: with ORCHICON_OPCODE_SESSION_TRANSPORT=0 there is no serve
// and no silent degradation — the caller gets the loud reason (AC 4).
func TestEnsureStartedKillSwitchFailsFast(t *testing.T) {
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", "0")
	h := NewHostServe(lazyServeLogger(), t.TempDir(), t.TempDir())

	err := h.EnsureStarted(context.Background())
	if err == nil {
		t.Fatal("EnsureStarted with the kill-switch set returned nil, want the disabled error (fail-fast, not silent degradation)")
	}
	if !strings.Contains(err.Error(), "ORCHICON_OPCODE_SESSION_TRANSPORT=0") {
		t.Errorf("EnsureStarted error %q must name the kill-switch", err)
	}
	if got := h.StartError(); got == nil || got.Error() != err.Error() {
		t.Errorf("StartError() = %v, want the same loud reason %v", got, err)
	}
}

// TestEnsureStartedIsIdempotentWhenAlreadyUp: the first demand starts the serve; every later demand
// takes the fast path — no second process, no re-probe.
func TestEnsureStartedIsIdempotentWhenAlreadyUp(t *testing.T) {
	h := NewHostServe(lazyServeLogger(), t.TempDir(), t.TempDir())
	// Pretend a first demand already brought the serve up.
	h.mu.Lock()
	h.started = true
	h.client = NewSessionClient("http://127.0.0.1:1", "pw", "")
	h.mu.Unlock()

	for i := 0; i < 3; i++ {
		if err := h.EnsureStarted(context.Background()); err != nil {
			t.Fatalf("EnsureStarted call %d on a live serve = %v, want nil (fast path)", i+1, err)
		}
	}
}

// TestStopCancelsSupervisionAndClearsState: shutdown must leave nothing running — the supervision
// cancel is invoked and the serve state is cleared so a later demand starts fresh rather.
func TestStopCancelsSupervisionAndClearsState(t *testing.T) {
	h := NewHostServe(lazyServeLogger(), t.TempDir(), t.TempDir())
	cancelled := false
	h.mu.Lock()
	h.started = true
	h.client = NewSessionClient("http://127.0.0.1:1", "pw", "")
	h.superviseCancel = func() { cancelled = true }
	h.mu.Unlock()

	h.Stop()
	if !cancelled {
		t.Error("Stop did not cancel supervision — the watchdog goroutine would outlive the plane")
	}
	if h.ready() {
		t.Error("Stop left the serve marked ready")
	}
	if h.Client() != nil {
		t.Error("Stop left a session client behind")
	}
}

// TestMarkStartFailedClearsPartialState: a FAILED lazy start must not leave the serve marked ready.
func TestMarkStartFailedClearsPartialState(t *testing.T) {
	h := NewHostServe(lazyServeLogger(), t.TempDir(), t.TempDir())
	// Simulate a readiness timeout: the process was armed, then killed.
	h.mu.Lock()
	h.started = true
	h.client = NewSessionClient("http://127.0.0.1:1", "pw", "")
	h.mu.Unlock()

	boom := errors.New("host serve did not become ready within 90s")
	h.markStartFailed(boom)

	if h.ready() {
		t.Error("markStartFailed left the serve marked ready — the next demand would skip the start")
	}
	if h.Client() != nil {
		t.Error("markStartFailed left a session client behind")
	}
	if got := h.StartError(); got == nil || !strings.Contains(got.Error(), "90s") {
		t.Errorf("StartError() = %v, want the recorded start failure", got)
	}
}
