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

// TestStartFailsFastWithoutServe verifies the one-shot fallback is gone:
// when no host serve AND no runtime client are configured, Start returns a
// hard error instead of silently spawning an `opencode run` subprocess.
func TestStartFailsFastWithoutServe(t *testing.T) {
	orig := os.Getenv("ORCHICON_OPCODE_SESSION_TRANSPORT")
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", orig)
	os.Unsetenv("ORCHICON_OPCODE_SESSION_TRANSPORT")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(log)

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
func TestStartFailsFastWhenTransportDisabled(t *testing.T) {
	t.Setenv("ORCHICON_OPCODE_SESSION_TRANSPORT", "0")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(log)

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
