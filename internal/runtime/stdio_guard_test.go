package runtime

// stdio_guard_test.go — the container-side half of the claude worker's OS-level
// guard proof.
//
// runStdio is the transport a claude worker's container execution uses: the
// claude adapter drives ONE long-lived child (internal/claude/proc.go,
// runtime.Client.Stdio) and the supervisor runs it here, with the execution
// guard built (guard.MakeGuard) and prepended to its PATH. That is what makes a
// destructive command impossible INSIDE the run's container, and it is the only
// guard a container execution gets — the claude adapter deliberately ships no
// host-built shim into the container (see
// internal/claude/guard_env_test.go:TestContainerTransportDoesNotShipAHostShimPath).
//
// The claim "runStdio applies the guard" was previously only a COMMENT (no test
// drove the handler), so deleting those two lines would have left every
// permission/parity suite green while every container worker — claude and
// opencode alike — ran unguarded. This test drives the REAL handler with a REAL
// child and asserts the refusal comes from the shim this path installed.
//
// Targets are TEMP SPACE (the rule internal/guard/guard_test.go documents): a
// test whose safety depends on the verdict of the thing it tests fails open, so
// the failure mode of a bad verdict is a directory this test owns.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shimFreePath is the ambient PATH with every Orchicon guard shim directory
// removed. It is NOT cosmetic: the runner's own PATH in an Orchicon run already
// begins with a guard shim (the worker process is itself guarded), so a child
// that resolved `rm` through the inherited PATH would be refused by THAT shim
// even if runStdio installed none — the assertion would pass with the guard
// deleted from the code under test (verified: it did). Pinning the child's PATH
// makes the guard under test the ONLY possible source of the refusal.
func shimFreePath(t *testing.T) string {
	t.Helper()
	var kept []string
	for _, d := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if d == "" || strings.Contains(filepath.Base(d), "orchicon-guard-") {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		t.Fatal("every PATH entry is a guard shim — the child could not resolve a shell")
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// stdioEvents drives runStdio for one argv with NO follow-up frames (the decoder
// hits EOF immediately, so the child sees stdin close) and returns the decoded
// event stream. The child's PATH is pinned shim-free (see shimFreePath), so the
// shim it resolves is the one runStdio prepends and nothing else.
func stdioEvents(t *testing.T, req AgentRequest) []AgentEvent {
	t.Helper()
	req.Env = append(append([]string(nil), req.Env...), "PATH="+shimFreePath(t))
	h := newChildRegistry(slog.Default())
	var in bytes.Reader
	var buf bytes.Buffer
	h.runStdio(json.NewDecoder(&in), json.NewEncoder(&buf), req)

	var events []AgentEvent
	d := json.NewDecoder(&buf)
	for {
		var ev AgentEvent
		if err := d.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatalf("runStdio produced no events for argv %v", req.Argv)
	}
	return events
}

// stdioStreamText concatenates every stream chunk the child wrote.
func stdioStreamText(events []AgentEvent) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString(ev.Data)
	}
	return b.String()
}

// stdioExitCode returns the terminal exit event's code — the contract the plane
// reads to decide whether the tool call succeeded.
func stdioExitCode(t *testing.T, events []AgentEvent) int {
	t.Helper()
	for _, ev := range events {
		if ev.Event == "exit" {
			return ev.ExitCode
		}
	}
	t.Fatalf("runStdio emitted no terminal exit event: %#v", events)
	return -1
}

// THE PIN: a subprocess command issued through the stdio transport — the path a
// claude worker's Bash tool executes on inside the run's container — is refused
// by the execution guard, while ordinary in-project cleanup still runs.
func TestStdioChildRunsUnderTheExecutionGuard(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir() // temp space ONLY

	events := stdioEvents(t, AgentRequest{
		Cmd:        "stdio",
		ExecID:     "exec-guard-1",
		Argv:       []string{"sh", "-c", "rm -rf " + outside},
		Cwd:        project,
		ProjectDir: project,
	})
	if code := stdioExitCode(t, events); code == 0 {
		t.Fatalf("the stdio path RAN `rm -rf %s` (exit %d, output %q) — runStdio does not apply guard.MakeGuard",
			outside, code, stdioStreamText(events))
	}
	if _, err := os.Stat(outside); os.IsNotExist(err) {
		t.Fatalf("the destructive command DELETED %s — the guard is not on the stdio execution path", outside)
	}
	if out := stdioStreamText(events); !strings.Contains(strings.ToUpper(out), "ORCHICON GUARD") {
		t.Errorf("the refusal does not name the guard: %q", out)
	}

	// The guard was not made useless: in-project cleanup through the SAME path
	// still runs (the shim allows a scoped binary whose targets stay in scope).
	build := filepath.Join(project, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	events = stdioEvents(t, AgentRequest{
		Cmd:        "stdio",
		ExecID:     "exec-guard-2",
		Argv:       []string{"sh", "-c", "rm -rf build"},
		Cwd:        project,
		ProjectDir: project,
	})
	if code := stdioExitCode(t, events); code != 0 {
		t.Fatalf("the stdio path refused in-project cleanup `rm -rf build` (exit %d, output %q)",
			code, stdioStreamText(events))
	}
	if _, err := os.Stat(build); !os.IsNotExist(err) {
		t.Fatal("in-project cleanup did not happen")
	}
}

// The guard is built from the request's project dir, not the container's cwd:
// a command that stays inside the PROJECT but runs from elsewhere is allowed,
// and the shim the child resolves is the one for THIS execution.
func TestStdioGuardUsesTheRequestProjectDir(t *testing.T) {
	project := t.TempDir()
	target := filepath.Join(project, "dist")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	events := stdioEvents(t, AgentRequest{
		Cmd:        "stdio",
		ExecID:     "exec-guard-3",
		Argv:       []string{"sh", "-c", "rm -rf " + target},
		Cwd:        t.TempDir(), // the child runs OUTSIDE the project; scope is the request's
		ProjectDir: project,
	})
	if code := stdioExitCode(t, events); code != 0 {
		t.Fatalf("an in-project target was refused because the child ran from another cwd (exit %d, output %q)",
			code, stdioStreamText(events))
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("the in-project target was not removed")
	}
}
