package mcpclient

// mcp_degradation_test.go — WHAT A FAILING SERVER COSTS, and what it tells you.
//
// Two reported properties, both about the same incident:
//
//  1. ONE UNSTARTABLE SERVER MUST NOT TAKE THE REST WITH IT. A conversation resolved 123 aseprite tools and
//     166 unity tools; one server failed to start, the manager aborted the union, and the conversation got
//     NEITHER. 289 working tools were lost to 1 broken one, and nothing said so.
//  2. A FAILURE MUST EXPLAIN ITSELF. Every failure surfaced as the same "context deadline exceeded", so
//     "the interpreter was not found", "the import blew up", and "it started but never answered" were
//     indistinguishable — while the server answered `initialize` in 0.3s when spawned by hand.

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// deadSpec is a stdio server whose command cannot be executed. It is the "never started" shape, and it is
// deliberately NOT a fixture that fails the handshake: exec failure is the case where cmd.Process is nil.
func deadSpec(id string) ServerSpec {
	return ServerSpec{ID: id, Type: TypeStdio, Command: []string{"/nonexistent-interpreter-for-orchicon-test"}}
}

// THE REPORTED LOSS, pinned: one dead server, one healthy one, and the healthy server's tools SURVIVE.
func TestOneUnstartableServerDoesNotCostTheRestOfTheSurface(t *testing.T) {
	good := stdioSpec("healthy")
	m := NewManager(testLogger(t))

	defs, err := m.Start(context.Background(), []ServerSpec{deadSpec("broken"), good})
	if err != nil {
		t.Fatalf("Start returned an error although one server started: %v — a single bad server must not "+
			"abort the union, which is exactly how 289 working tools were lost to 1 broken one", err)
	}
	if len(defs) == 0 {
		t.Fatal("no tools were discovered from the HEALTHY server — the union was aborted")
	}
	for _, d := range defs {
		if strings.Contains(d.Name, "broken") {
			t.Fatalf("a tool (%q) was attributed to the server that never started", d.Name)
		}
	}
	// The healthy server's tools must be CALLABLE, not merely listed: a def with no connection behind it
	// would route to nothing.
	if _, err := m.Execute(context.Background(), defs[0].Name, "{}"); err != nil {
		t.Fatalf("the surviving tool %q is not callable: %v", defs[0].Name, err)
	}
	t.Cleanup(func() { _ = m.Close() })
}

// ...AND THE FAILURE IS RECORDED, because a silently absent tool is indistinguishable from a platform
// that does not have it.
func TestAFailedServerIsReportedWithItsOwnDiagnosis(t *testing.T) {
	m := NewManager(testLogger(t))
	if _, err := m.Start(context.Background(), []ServerSpec{deadSpec("broken"), stdioSpec("healthy")}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	failures := m.Failures()
	if len(failures) != 1 {
		t.Fatalf("Failures() reported %d failed servers, want exactly the 1 that could not start: %+v",
			len(failures), failures)
	}
	f := failures[0]
	if f.ServerID != "broken" {
		t.Errorf("failed server id = %q, want %q", f.ServerID, "broken")
	}
	if f.Transport != "stdio" {
		t.Errorf("failed transport = %q, want stdio", f.Transport)
	}
	// THE DIAGNOSIS MUST SAY WHICH SHAPE IT WAS. "context deadline exceeded" told nobody anything; the
	// distinction below is the whole point of stdioProcessState.
	msg := f.Error()
	if !strings.Contains(msg, "never started") && !strings.Contains(msg, "EXITED") && !strings.Contains(msg, "STILL RUNNING") {
		t.Fatalf("failure does not say what happened to the child: %q\n  wanted one of: never started / "+
			"EXITED with … / STILL RUNNING (pid N)", msg)
	}
}

// An empty surface IS an error — the one case a caller cannot continue past silently.
func TestEveryServerFailingIsStillAnError(t *testing.T) {
	m := NewManager(testLogger(t))
	t.Cleanup(func() { _ = m.Close() })

	_, err := m.Start(context.Background(), []ServerSpec{deadSpec("broken-a"), deadSpec("broken-b")})
	if err == nil {
		t.Fatal("Start returned no error when EVERY server failed — a caller would run with an MCP surface it " +
			"was promised and none of it present")
	}
	if !strings.Contains(err.Error(), "no MCP server could be started") {
		t.Errorf("error does not say the surface is empty: %v", err)
	}
	if got := len(m.Failures()); got != 2 {
		t.Errorf("Failures() has %d entries, want 2 — every failure is reported, not just the first", got)
	}
}

// A process that RAN is described differently from one that never existed, and its stderr is captured.
//
// This is the "died on import" shape: the interpreter exists, starts, and exits immediately. The old code
// reported it identically to a missing interpreter.
func TestAChildThatExitsIsReportedWithItsExitStatus(t *testing.T) {
	// `sh -c 'echo ... >&2; exit 3'` — starts, writes a diagnosis to stderr, exits non-zero. Portable on
	// any Unix; the test skips where `sh` is not available.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	m := NewManager(testLogger(t))
	t.Cleanup(func() { _ = m.Close() })

	spec := ServerSpec{ID: "exits", Type: TypeStdio, Command: []string{sh, "-c", "echo 'ImportError: no module named aseprite_mcp' >&2; exit 3"}}
	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err == nil {
		t.Fatal("Start succeeded for a child that exits immediately")
	}
	failures := m.Failures()
	if len(failures) != 1 {
		t.Fatalf("want 1 failure, got %d", len(failures))
	}
	msg := failures[0].Error()
	if !strings.Contains(msg, "EXITED") {
		t.Errorf("failure does not report that the child EXITED (it ran, then died): %q", msg)
	}
	// THE PART THAT ENDS THE INVESTIGATION. The child's own words were going to /dev/null because the
	// go-sdk sets only stdin/stdout on the command.
	if !strings.Contains(msg, "no module named aseprite_mcp") {
		t.Errorf("the child's STDERR was not captured into the failure: %q\n  a server that dies on import "+
			"reports why on stderr, and that is the diagnosis", msg)
	}
}

// The stderr capture is bounded and keeps the TAIL, because the fatal line is the last one.
func TestStderrCaptureKeepsTheTail(t *testing.T) {
	r := newProcStderr()
	for i := 0; i < stderrTailLines*3; i++ {
		_, _ = r.Write([]byte("line-" + strconv.Itoa(i) + "\n"))
	}
	tail := r.Tail()
	if !strings.Contains(tail, "line-"+strconv.Itoa(stderrTailLines*3-1)) {
		t.Fatalf("the LAST line was evicted: %q", tail)
	}
	if strings.Contains(tail, "line-0") {
		t.Fatalf("the capture kept the head instead of the tail: %q", tail)
	}
}
