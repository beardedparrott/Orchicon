package orchicon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

var errContainerGone = errors.New("no runtime container leased for run")

// TestContainerBashRouting pins always-container native exec: a HostTools
// wired with a container exec fn routes `bash` through it (cwd = the
// in-container worktree, sandbox DSN env passed through) and maps a
// non-zero exit to a RESULT (nil error), while a transport failure is an
// error (fail LOUD, never silent host fallback).
func TestContainerBashRouting(t *testing.T) {
	var gotCmd, gotCwd string
	var gotEnv []string
	h := NewContainerHostTools("/proj/.orchicon-worktrees/run1", "/proj",
		func(ctx context.Context, command string, env []string, cwd string) (string, string, int, error) {
			gotCmd, gotCwd, gotEnv = command, cwd, env
			return "out-bytes", "err-bytes", 0, nil
		})
	out, err := h.Execute(context.Background(), "bash", `{"command":"pwd && go version"}`)
	if err != nil {
		t.Fatalf("container bash: %v", err)
	}
	if gotCmd != "pwd && go version" {
		t.Errorf("command = %q, want the worker command verbatim", gotCmd)
	}
	if gotCwd != "/proj/.orchicon-worktrees/run1" {
		t.Errorf("cwd = %q, want the in-container worktree path", gotCwd)
	}
	joined := strings.Join(gotEnv, " ")
	if !strings.Contains(joined, "ORCHICON_TEST_DSN=") || !strings.Contains(joined, "localhost:5432") {
		t.Errorf("env must carry the sandbox DSN, got %q", joined)
	}
	if !strings.Contains(out, "out-bytes") || !strings.Contains(out, "err-bytes") {
		t.Errorf("output must merge stdout+stderr, got %q", out)
	}
}

// TestContainerBashNonZeroExitIsResult pins the exit-code contract: a
// failing command inside the container returns its output with a nil
// error (the model course-corrects), matching the in-process path.
func TestContainerBashNonZeroExitIsResult(t *testing.T) {
	h := NewContainerHostTools(t.TempDir(), "",
		func(ctx context.Context, command string, env []string, cwd string) (string, string, int, error) {
			return "partial-out", "boom", 1, nil
		})
	out, err := h.Execute(context.Background(), "bash", `{"command":"exit 1"}`)
	if err != nil {
		t.Fatalf("non-zero container exit must be a result, got error: %v", err)
	}
	if !strings.Contains(out, "partial-out") || !strings.Contains(out, "boom") {
		t.Errorf("non-zero result must carry output, got %q", out)
	}
}

// TestContainerBashTransportErrorFailsLoud pins the fail-LOUD contract: a
// broken exec channel (no lease, daemon down) surfaces an error — the
// reconciler fails the execution, never silently runs on the host.
func TestContainerBashTransportErrorFailsLoud(t *testing.T) {
	h := NewContainerHostTools(t.TempDir(), "",
		func(ctx context.Context, command string, env []string, cwd string) (string, string, int, error) {
			return "", "", 0, errContainerGone
		})
	if _, err := h.Execute(context.Background(), "bash", `{"command":"pwd"}`); err == nil {
		t.Fatal("transport failure must error (fail LOUD), got nil")
	}
}

// TestInProcessBashUnchanged pins the local-mode path: without a container
// fn, bash runs in-process exactly as before.
func TestInProcessBashUnchanged(t *testing.T) {
	h := NewHostTools(t.TempDir(), "")
	out, err := h.Execute(context.Background(), "bash", `{"command":"echo hello-native"}`)
	if err != nil {
		t.Fatalf("in-process bash: %v", err)
	}
	if !strings.Contains(out, "hello-native") {
		t.Errorf("in-process bash output = %q, want hello-native", out)
	}
}
