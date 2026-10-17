package runtime

// The run-start serve gate and the nil-client contract (lifecycle.go
// EnsureServing / EnsureForRun / ReapForRun / Adopt).
//
// The AC 3 property that must never regress: an all-native (serve-less) run
// and a legacy `no-serve` sentinel row must NOT create a container and must
// NOT be gated on a serve probe. That is what keeps local/native mode cheap.
// The way these tests prove "no container" is by handing the Lifecycle a
// client whose daemon socket does not exist: any attempt to reach the daemon
// would fail with "runtime daemon not reachable" instead of returning nil.
//
// The AC 9 contract: a nil client (headless `orchicon serve`, no daemon)
// makes EnsureServing/ReapForRun/Adopt no-ops and EnsureForRun a LOUD error.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/db"
)

// absentClient is a real client pointed at a socket nothing listens on: any
// daemon round-trip fails, so a nil return proves the call short-circuited
// before touching the engine.
func absentClient() *Client {
	return NewClient("/nonexistent/orchicon-runtime-test/runtime.sock", "dev")
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestEnsureServingServeLessRunCreatesNoContainer(t *testing.T) {
	lc := NewLifecycle(absentClient(), nil, discardLogger(), nil, nil)

	t.Run("native-only run is not gated and not containerised", func(t *testing.T) {
		run := db.WorkflowRunRow{ID: "run_native", RuntimeImage: "ghcr.io/beardedparrott/orchicon-runtime:latest"}
		if err := lc.EnsureServing(context.Background(), run, false); err != nil {
			t.Fatalf("serve-less run must not be gated on a serve (no container to probe): %v", err)
		}
	})

	t.Run("legacy no-serve sentinel row short-circuits", func(t *testing.T) {
		run := db.WorkflowRunRow{ID: "run_sentinel", RuntimeImage: runtimeNoServeImage}
		if err := lc.EnsureServing(context.Background(), run, true); err != nil {
			t.Fatalf("a %q row has no container to probe and must return nil: %v", runtimeNoServeImage, err)
		}
	})
}

func TestEnsureServingNilClientIsNoop(t *testing.T) {
	// Headless serve with no daemon: the probe has nothing to talk to and
	// must not fail the run (local mode is the explicit opt-out; this path is
	// only reached when a daemon is configured but absent).
	lc := NewLifecycle(nil, nil, discardLogger(), nil, nil)
	run := db.WorkflowRunRow{ID: "run_headless", RuntimeImage: "ghcr.io/beardedparrott/orchicon-runtime:latest"}
	if err := lc.EnsureServing(context.Background(), run, true); err != nil {
		t.Fatalf("EnsureServing with a nil client must be a no-op, got %v", err)
	}
	if err := lc.ReapForRun(context.Background(), run.ID); err != nil {
		t.Fatalf("ReapForRun with a nil client must be a no-op, got %v", err)
	}
	if err := lc.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt with a nil client must be a no-op, got %v", err)
	}
}

func TestEnsureForRunNilClientIsLoud(t *testing.T) {
	// Silent in-process degrade in runtime mode is gone: without a daemon a
	// runtime-mode run cannot get a container, and pretending it did would
	// dispatch executions onto the host.
	lc := NewLifecycle(nil, nil, discardLogger(), nil, nil)
	run := db.WorkflowRunRow{ID: "run_loud", RuntimeImage: "ghcr.io/beardedparrott/orchicon-runtime:latest"}
	err := lc.EnsureForRun(context.Background(), run)
	if err == nil {
		t.Fatal("EnsureForRun with a nil client must fail loudly, not degrade in-process")
	}
	msg := err.Error()
	for _, want := range []string{"runtime", "no container can be created", "execution_mode"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must name %q so the operator knows the fix", msg, want)
		}
	}
}

func TestEnsureServingAttemptsTheDaemonWhenItIsNeeded(t *testing.T) {
	// Positive control for the tests above: with a serve-dependent run and a
	// client whose daemon is absent, the gate DOES try the daemon and fails.
	// Without this, the nil returns above could be vacuous.
	lc := NewLifecycle(absentClient(), nil, discardLogger(), nil, nil)
	run := db.WorkflowRunRow{ID: "run_opencode", RuntimeImage: "ghcr.io/beardedparrott/orchicon-runtime:latest"}
	err := lc.EnsureServing(context.Background(), run, true)
	if err == nil {
		t.Fatal("a serve-dependent run with an unreachable daemon must fail the gate, not pass it")
	}
	if !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("error = %q, want the daemon-unreachable message", err)
	}
}

func TestServeReadyTimeout(t *testing.T) {
	t.Run("default is the 120s run-start window", func(t *testing.T) {
		t.Setenv("ORCHICON_RUNTIME_SERVE_READY_TIMEOUT", "")
		if got := serveReadyTimeout(); got != 120*time.Second {
			t.Fatalf("serveReadyTimeout() = %s, want 120s", got)
		}
	})
	t.Run("env override", func(t *testing.T) {
		t.Setenv("ORCHICON_RUNTIME_SERVE_READY_TIMEOUT", "5s")
		if got := serveReadyTimeout(); got != 5*time.Second {
			t.Fatalf("serveReadyTimeout() = %s, want 5s", got)
		}
	})
	t.Run("an unusable value cannot shorten the window", func(t *testing.T) {
		for _, bad := range []string{"nonsense", "0s", "-3s"} {
			t.Setenv("ORCHICON_RUNTIME_SERVE_READY_TIMEOUT", bad)
			if got := serveReadyTimeout(); got != 120*time.Second {
				t.Fatalf("serveReadyTimeout(%q) = %s, want the 120s default", bad, got)
			}
		}
	})
}
