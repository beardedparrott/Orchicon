package claude

import (
	"path/filepath"
	"testing"

	"github.com/beardedparrott/orchicon/internal/db"
	"github.com/beardedparrott/orchicon/internal/runtime"
	"github.com/beardedparrott/orchicon/internal/scheduler"
)

// THE REPORTED FAILURE, PINNED. Every claude container dispatch on Windows died in ~400ms with 0
// tokens and "claude session exited without a terminal result message (exit code 1); the CLI emitted
// no stderr at all before exiting".
//
// The cause was argv[0]. Claude was the only adapter using the daemon's duplex STDIO transport, and it
// sent ClaudeBinaryPath() — a HOST path. On Windows that is C:\Users\...\claude.exe, which is not a
// path that exists inside a Linux container. The supervisor refuses anything whose basename is not in
// runtimeBinAllowlist (internal/runtime/agent.go runStdio), and on Linux filepath.Base splits on "/"
// ONLY — so the basename of a Windows path is the WHOLE string and the request is refused before any
// child is spawned. No child means no stderr, which is why the failure read as a silent CLI exit.
//
// These tests assert the CONTRACT rather than the symptom: whatever the plane sends as argv[0] for a
// container session must survive the supervisor's basename rule.
const reportedWindowsClaudePath = `C:\Users\justi\AppData\Local\Microsoft\WinGet\Packages\Anthropic.ClaudeCode\claude.exe`

// supervisorAllowlist mirrors internal/runtime's runtimeBinAllowlist. It is duplicated ON PURPOSE:
// the point of the test is that the claude adapter emits a name the DAEMON accepts, and importing the
// daemon's set would make the assertion true by construction even if the two drifted apart.
var supervisorAllowlist = map[string]bool{
	"opencode": true,
	"claude":   true,
	"orchicon": true,
	"bash":     true,
	"sh":       true,
}

// acceptedBySupervisor applies the supervisor's exact rule (internal/runtime/agent.go runStdio) to an
// argv, with the container's own path semantics — always forward slashes, whichever host runs the test.
func acceptedBySupervisor(argv0 string) (string, bool) {
	base := filepath.Base(filepath.ToSlash(argv0))
	return base, supervisorAllowlist[base]
}

// containerSession builds a session whose transport is the runtime container. It uses the REAL
// isContainer predicate (Bridge + rt + a run id + a non-local execution mode), not a stub, so a change
// to that predicate cannot silently stop these tests from covering the container case.
func containerSession(t *testing.T, projectDir string) *session {
	t.Helper()
	b := New(quietLogger())
	// A real *runtime.Client, never dialled: isContainer only asks whether the transport EXISTS.
	b.rt = runtime.NewClient(filepath.Join(t.TempDir(), "agent.sock"), "argv-test")
	m := scheduler.ExecutionManifest{
		ExecutionID:       "exec-argv",
		Goal:              "do the thing",
		ModelRef:          "claude/anthropic/claude-sonnet-5",
		ProjectDir:        projectDir,
		RuntimeWorkflowID: "run-argv",
		ExecutionMode:     db.ExecutionModeRuntime,
	}
	s := newSession(b, "exec-argv", "t1", m, &recordingCallbacks{})
	if !s.inContainer() {
		t.Fatal("test setup: session is not on the container transport — the assertions below would be vacuous")
	}
	return s
}

// hostSession builds a session on the LOCAL transport (the Ask/host shape).
func hostSession(t *testing.T, projectDir string) *session {
	t.Helper()
	b := New(quietLogger())
	m := scheduler.ExecutionManifest{
		ExecutionID:   "exec-host",
		Goal:          "do the thing",
		ModelRef:      "claude/anthropic/claude-sonnet-5",
		ProjectDir:    projectDir,
		ExecutionMode: db.ExecutionModeLocal,
	}
	s := newSession(b, "exec-host", "t1", m, &recordingCallbacks{})
	if s.inContainer() {
		t.Fatal("test setup: session is on the container transport but should be local")
	}
	return s
}

// The regression itself: a container session must not send a host path.
func TestContainerSessionSendsAGuestBinaryNameNotAHostPath(t *testing.T) {
	// A WINDOWS install path is forced regardless of the host running this test. On Linux the fallback
	// for a missing install is the bare name "claude", which the supervisor happens to accept — so
	// without this the test would pass on Linux while Windows stayed broken.
	t.Setenv(ClaudeBinEnv, reportedWindowsClaudePath)

	s := containerSession(t, t.TempDir())
	got := s.argv()[0]

	hostPath := ClaudeBinaryPath()
	if hostPath != reportedWindowsClaudePath {
		t.Fatalf("test setup: ClaudeBinaryPath() = %q, want the forced override %q", hostPath, reportedWindowsClaudePath)
	}

	if got == hostPath {
		t.Fatalf("container argv[0] = %q — that is the HOST install path. The supervisor refuses it by "+
			"basename (filepath.Base on the container's Linux side splits on \"/\" only, so the basename is "+
			"the whole string) and never spawns a child: the reported 0-token, 400ms, no-stderr failure.",
			got)
	}
	base, ok := acceptedBySupervisor(got)
	if !ok {
		t.Fatalf("container argv[0] = %q (basename %q) is NOT in the supervisor's allowlist — the daemon "+
			"will answer {event:error} and spawn nothing", got, base)
	}
	if base != ClaudeBinaryName {
		t.Errorf("container argv[0] basename = %q, want %q — the image installs the CLI under that name "+
			"(deploy/runtime/Dockerfile.dev: npm install -g @anthropic-ai/claude-code)", base, ClaudeBinaryName)
	}
}

// The host transport must be untouched: on the operator's machine the resolved install IS the right
// binary (it is the one they authenticated and the one whose version matches the catalog).
func TestHostSessionStillUsesTheResolvedInstallPath(t *testing.T) {
	t.Setenv(ClaudeBinEnv, reportedWindowsClaudePath)
	s := hostSession(t, t.TempDir())
	if got := s.argv()[0]; got != reportedWindowsClaudePath {
		t.Fatalf("host argv[0] = %q, want the resolved install path %q", got, reportedWindowsClaudePath)
	}
}

// ClaudeBinaryFor is the seam both transports go through. Pinned directly so a later edit to argv()
// cannot quietly reintroduce the host path, and so the host behaviour cannot drift either.
func TestClaudeBinaryForSelectsByTransport(t *testing.T) {
	t.Setenv(ClaudeBinEnv, reportedWindowsClaudePath)

	if got := ClaudeBinaryFor(false); got != reportedWindowsClaudePath {
		t.Errorf("ClaudeBinaryFor(host) = %q, want the resolved install %q", got, reportedWindowsClaudePath)
	}
	got := ClaudeBinaryFor(true)
	if got != ClaudeBinaryName {
		t.Errorf("ClaudeBinaryFor(container) = %q, want %q — the image's PATH binary", got, ClaudeBinaryName)
	}
	// A container request must survive the daemon's rule; a host path must be RECOGNISED as unacceptable
	// so this test documents why the two transports need different answers.
	if _, ok := acceptedBySupervisor(got); !ok {
		t.Errorf("ClaudeBinaryFor(container) = %q is not accepted by the supervisor", got)
	}
	if _, ok := acceptedBySupervisor(reportedWindowsClaudePath); ok {
		t.Errorf("a Windows host path is accepted by the supervisor's basename rule — this test's premise "+
			"(%q cannot cross the boundary) no longer holds, and the defect may be unreachable for another reason",
			reportedWindowsClaudePath)
	}
}
