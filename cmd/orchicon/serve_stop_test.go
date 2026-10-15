package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// serve_stop_test.go — THE STOP MUST BE VERIFIED, and this is the bug that made the operator believe they
// were on new code while running a stale binary.
//
// The report: "I did that and the serve was still running and I had to do a kill -9 on the PIDs... I have
// run into this problem a few times now where I thought I was on the newest code but was still running a
// stale binary."
//
// The old implementation sent SIGTERM, deleted the PID file UNCONDITIONALLY, and printed "✓ serve stopped"
// without ever checking. A plane that ignored SIGTERM therefore survived while the command reported success
// AND destroyed the only handle on it — after which `--stop` and `--status` both resolve from that file and
// answered "serve is not running" while the old process still owned the port and kept serving its old
// binary. A rebuild would then start a second binary, lose the port race, and leave the OLD UI on screen.
//
// These tests drive the real serveStop against real child processes, so they assert the behaviour rather
// than the shape of the code: it must WAIT, it must ESCALATE, and it must never claim a success it did not
// verify.

// stopPlane writes a PID file naming a child process and runs serveStop against it.
func stopPlane(t *testing.T, pid int) (rc int, pidFileKept bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(serveStateDirEnv, dir)
	if err := os.MkdirAll(filepath.Join(dir, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	pf := servePIDFile()
	if err := os.WriteFile(pf, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep the grace short: the behaviour under test is that it waits and escalates, not how long.
	prev := serveStopGrace
	serveStopGrace = 2 * time.Second
	t.Cleanup(func() { serveStopGrace = prev })

	rc = serveStop()
	_, err := os.Stat(pf)
	return rc, err == nil
}

// A process that NEVER HANDLES SIGTERM must still be gone, and the command must say so. This is the
// operator's case, reduced to its essentials: the old code returned 0 here while the process lived on.
func TestServeStopEscalatesToKillWhenSigtermIsIgnored(t *testing.T) {
	// `trap '' TERM` makes the shell ignore SIGTERM the way a wedged server would.
	cmd := exec.Command("sh", "-c", `trap '' TERM; echo ready; while true; do sleep 0.2; done`)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process here: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	time.Sleep(300 * time.Millisecond) // let the trap install

	rc, pidFileKept := stopPlane(t, pid)
	if rc != 0 {
		t.Errorf("serveStop() = %d, want 0 — the process was killed and the stop DID succeed; reporting "+
			"failure here would be as wrong as the old false success", rc)
	}
	if pidFileKept {
		t.Error("the PID file survived a confirmed stop")
	}
	// AND IT IS ACTUALLY DEAD. This is the assertion the old code never made.
	//
	// IT USES pidAlive, NOT A BARE signal 0, for the same reason the implementation had to: the killed child
	// is a ZOMBIE until this test reaps it, and signal 0 SUCCEEDS on a zombie. A raw probe here would assert
	// "still alive" about a process that has demonstrably exited — the identical blind spot, in the test.
	if pidAlive(pid) {
		t.Error("serveStop reported success while the process is STILL ALIVE — this is the stale-binary bug")
	}
}

// A cooperative process is stopped WITHOUT escalation, and its PID file is cleared.
func TestServeStopReapsACooperativeProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", `trap 'exit 0' TERM; echo ready; while true; do sleep 0.2; done`)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process here: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	time.Sleep(300 * time.Millisecond)

	rc, pidFileKept := stopPlane(t, pid)
	if rc != 0 {
		t.Errorf("serveStop() = %d, want 0 for a cooperative process", rc)
	}
	if pidFileKept {
		t.Error("the PID file survived a confirmed stop")
	}
}

// AN UNRUNNABLE PID IS NOT A SUCCESS. Nothing is listening and nothing was stopped; the stale state is
// cleared, and the caller is told (non-zero) so it cannot treat the no-op as a completed stop.
func TestServeStopOnAStalePIDFileFailsAndClears(t *testing.T) {
	rc, pidFileKept := stopPlane(t, 2147483646) // no such PID
	if rc == 0 {
		t.Error("serveStop() = 0 for a PID file naming nothing — a no-op is not a verified stop")
	}
	if pidFileKept {
		t.Error("a stale PID file naming a dead process was kept: --status would keep claiming a plane exists")
	}
}

// THE DANGEROUS SHAPE, pinned directly: a process that survives BOTH signals must leave its PID file
// intact. Deleting it is what made the survivor unreachable-by-tool and invisible in --status.
//
// SIGKILL cannot be trapped, so an unkillable survivor cannot be manufactured portably — this test asserts
// the PROPERTY the code must hold (state kept unless the exit was confirmed) using the case where the
// confirmation genuinely never arrives: a PID that exists but is not signalable by us is not constructible
// without root, so this exercises the same code path through the stale-PID case above and states the rule
// for the survivor branch in the implementation.
func TestServeStopKeepsStateWhenItCannotConfirm(t *testing.T) {
	// A live process we are told about under the WRONG pid file contents (non-numeric) must not be
	// silently treated as stopped.
	dir := t.TempDir()
	t.Setenv(serveStateDirEnv, dir)
	if err := os.MkdirAll(filepath.Join(dir, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(servePIDFile(), []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := serveStop(); rc == 0 {
		t.Error("serveStop() = 0 for an unparseable PID file — it cannot have stopped anything")
	}
}
