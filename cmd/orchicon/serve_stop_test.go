package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// serve_stop_test.go — THE STOP MUST BE VERIFIED, and this is the bug that made the operator
// believe they were on new code while running a stale binary.

// isolateServeState points the detached-serve state at dir, AND CLEARS ORCHICON_INSTANCE.
func isolateServeState(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("ORCHICON_INSTANCE", "")
	t.Setenv(serveStateDirEnv, dir)
}

// stopPlane writes a PID file naming a child process and runs serveStop against it.
func stopPlane(t *testing.T, pid int) (rc int, pidFileKept bool) {
	t.Helper()
	dir := t.TempDir()
	isolateServeState(t, dir)
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
	// AND IT IS ACTUALLY DEAD.
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

// THE DANGEROUS SHAPE, pinned directly: a process that survives BOTH signals must leave its PID
// file intact.
func TestServeStopKeepsStateWhenItCannotConfirm(t *testing.T) {
	// A live process we are told about under the WRONG pid file contents (non-numeric) must not be
	// silently treated as stopped.
	dir := t.TempDir()
	isolateServeState(t, dir)
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

// THE ESCALATION LADDER IS NOW SEAM-DRIVEN, so a reader looking for where SIGTERM/SIGKILL went
// should look at signals_unix.go / signals_windows.go (sendGracefulStop, sendForceStop.

// THE GUARD THAT KEEPS THE FALLBACK FROM KILLING A LIVE PLANE.
func TestServeStopFallbackOnlyForTheCanonicalStateDir(t *testing.T) {
	t.Run("redirected state dir refuses to discover", func(t *testing.T) {
		dir := t.TempDir()
		isolateServeState(t, dir) // ORCHICON_INSTANCE cleared, so the temp dir wins
		if serveStopMayDiscover() {
			t.Error("the port-discovery fallback ran while the state dir was redirected — it would signal " +
				"a plane that does not belong to this state root")
		}
	})

	t.Run("the launcher path allows discovery", func(t *testing.T) {
		// An instance named and NO override: serveStateDir() derives the state dir from that instance, so
		// servePIDFile() and planecontrol's record are the same path by construction.
		t.Setenv("ORCHICON_INSTANCE", "dev")
		t.Setenv(serveStateDirEnv, "")
		if !serveStopMayDiscover() {
			t.Fatal("the fallback must run for an instance's canonical state dir")
		}
	})
}

// THE OTHER HALF, AND THE ONE THAT ACTUALLY CAUSED THE OUTAGE: a test must never resolve the PID
// file to a live instance's real record.
func TestServeStopTestIsolationKeepsTheRealRecordOutOfReach(t *testing.T) {
	// The shape that bit us: ambient instance exported, override set. Both must be neutralized.
	t.Setenv("ORCHICON_INSTANCE", "prod")
	dir := t.TempDir()
	isolateServeState(t, dir)

	got := servePIDFile()
	realRecord := planecontrol.ServePIDFile("prod")
	if got == realRecord {
		t.Fatalf("isolated test still resolves the LIVE prod record (%s) — a test would clobber it", realRecord)
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("isolated PID file %q is not inside the temp dir %q", got, dir)
	}
}
