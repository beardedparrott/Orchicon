package opencode

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// failFastCallbacks records nothing; used to assert Start's fail-fast
// paths never invoke OnStarted/OnResult.
type failFastCallbacks struct{}

func (failFastCallbacks) OnStarted(context.Context, string)                          {}
func (failFastCallbacks) OnHealth(context.Context, string, string)                   {}
func (failFastCallbacks) OnStall(context.Context, string, string, bool)              {}
func (failFastCallbacks) OnRecovered(context.Context, string, string)                {}
func (failFastCallbacks) OnText(context.Context, string, string)                     {}
func (failFastCallbacks) OnToolCall(context.Context, string, string, []byte, []byte) {}
func (failFastCallbacks) OnArtifact(context.Context, string, string, string, string) {}
func (failFastCallbacks) OnResult(context.Context, string, bool, string, string)     {}
func (failFastCallbacks) OnWrittenFiles(context.Context, string, []string)           {}

func failFastManifest() scheduler.ExecutionManifest {
	return scheduler.ExecutionManifest{
		Goal:      "do nothing",
		ModelRef:  "opencode/deepseek-v4-flash-free",
		ProjectID: "proj-test",
	}
}

// hermeticBinaryResolver stands in for a found adapter CLI on bare CI
// runners: hermetic CI never installs adapter CLIs (the multi-adapter
// future — claude code etc. — must not need per-adapter CI installs), so
// tests stub the resolution seam instead of provisioning the binary. The
// returned path is never executed (session transport drives the serve
// over HTTP; resolution is a fail-fast sanity check only).
func hermeticBinaryResolver() (string, error) {
	return "/tmp/orchicon-test-stub/opencode", nil
}

// stubBinaryEnv trims the session-setup retry loop: stubbed-binary runs
// fail with "no opencode serve available" every attempt, and the default
// loop (4+3 attempts, growing backoff) would stall CI. 0 repairs keeps
// the loop at 4 fast attempts; ORCHICON_SESSION_INFRA_THRESHOLD=99 keeps
// the no-runtime-workflow path out of container repair.
func stubBinaryEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ORCHICON_SESSION_REPAIR_ATTEMPTS", "0")
	t.Setenv("ORCHICON_SESSION_INFRA_THRESHOLD", "99")
}

// TestStartFailsFastWithoutServe verifies the one-shot fallback is gone:
// when no host serve AND no runtime client are configured, Start returns a
// hard error instead of silently spawning an `opencode run` subprocess.
// Binary resolution is stubbed (SetBinaryResolver seam) so the test
// reaches the serve-availability path it exists to assert on a bare CI
// runner — no adapter CLI installed, no simulation env var.
func TestStartFailsFastWithoutServe(t *testing.T) {
	orig := os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT")
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", orig)
	os.Unsetenv("ORCHICON_OPCODE_SESSION_TRANSPORT")
	stubBinaryEnv(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(log)
	a.SetBinaryResolver(hermeticBinaryResolver)

	err := a.Start(context.Background(),
		db.ExecutionRow{ID: "exec-no-serve", TenantID: "tnt_dev", ProjectID: "proj-test", TaskID: "task-test"},
		failFastManifest(),
		failFastCallbacks{},
	)
	if err == nil {
		t.Fatal("Start succeeded with no serve available — the one-shot fallback was removed and it must fail fast")
	}
	if !strings.Contains(err.Error(), "no opencode serve available") {
		t.Errorf("expected a clear 'no opencode serve available' error, got: %v", err)
	}
}

// TestStartFailsFastWhenTransportDisabled verifies the kill-switch now
// means fail-fast: with ORCHICON_OPCODE_SESSION_TRANSPORT=0 the session
// transport is disabled and Start returns an error (no one-shot fallback).
// Binary resolution is stubbed so the test reaches the kill-switch path on
// a bare CI runner.
func TestStartFailsFastWhenTransportDisabled(t *testing.T) {
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", "0")
	stubBinaryEnv(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(log)
	a.SetBinaryResolver(hermeticBinaryResolver)

	err := a.Start(context.Background(),
		db.ExecutionRow{ID: "exec-disabled", TenantID: "tnt_dev", ProjectID: "proj-test", TaskID: "task-test"},
		failFastManifest(),
		failFastCallbacks{},
	)
	if err == nil {
		t.Fatal("Start succeeded with the session transport disabled — it must fail fast")
	}
	if !strings.Contains(err.Error(), "session transport disabled") {
		t.Errorf("expected a clear 'session transport disabled' error, got: %v", err)
	}
}

// TestResolveOpenCodeBinaryFailsCleanly covers the production resolver's
// miss path: with the binary absent from both PATH and $HOME/.opencode/bin
// it returns "" + an error (Start's fail-fast guard keys on that pair).
func TestResolveOpenCodeBinaryFailsCleanly(t *testing.T) {
	t.Setenv("PATH", "/tmp/orchicon-test-empty-path")
	t.Setenv("HOME", "/tmp/orchicon-test-empty-home")

	bin, err := resolveOpenCodeBinary()
	if err == nil || bin != "" {
		t.Fatalf("expected ('', err) with no binary anywhere; got bin=%q err=%v", bin, err)
	}
	if !strings.Contains(err.Error(), "opencode binary not found") {
		t.Errorf("expected the loud 'binary not found' error, got: %v", err)
	}
}

// TestSetBinaryResolverNilRestoresDefault pins the seam contract: a nil
// override must restore the production resolver, never leave the adapter
// with a nil function (which would panic Start).
func TestSetBinaryResolverNilRestoresDefault(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(log)
	a.SetBinaryResolver(hermeticBinaryResolver)
	a.SetBinaryResolver(nil)
	if a.resolveBinary == nil {
		t.Fatal("SetBinaryResolver(nil) left the resolver nil — Start would panic")
	}
	bin, err := a.resolveBinary()
	// Either outcome is fine depending on the host (worker containers have
	// the real binary; bare CI does not) — the contract is only that the
	// default resolver is wired.
	if err != nil && !strings.Contains(err.Error(), "opencode binary not found") {
		t.Fatalf("unexpected resolver error shape: %v", err)
	}
	if bin != "" && !strings.HasSuffix(bin, "opencode") {
		t.Fatalf("unexpected resolved path: %q", bin)
	}
}
