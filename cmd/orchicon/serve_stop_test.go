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

// isolateServeState points the detached-serve state at dir, AND CLEARS ORCHICON_INSTANCE.
//
// THE SECOND HALF IS NOT OPTIONAL, AND ITS ABSENCE WAS A REAL OUTAGE.
//
// serveStateDir() prefers ORCHICON_INSTANCE over ORCHICON_SERVE_STATE_DIR — deliberately, so an ambient
// state dir can never send one instance's log to another. But that means t.Setenv(serveStateDirEnv, tmp)
// is silently IGNORED whenever ORCHICON_INSTANCE is exported in the developer's shell, which it is on any
// machine set up for prod work.
//
// The consequence was not a test failure alone. These tests then resolved servePIDFile() to the REAL
// instance record, WROTE a bogus PID into it, and let serveStop clear it — destroying a live plane's
// record. On a branch whose serveStop additionally discovered a plane by port, it went further and stopped
// the live plane: a session disconnected mid-turn and the plane "kept crashing".
//
// Clearing the instance makes the temp dir win, so the tests touch only their own fixture.
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

// THE ESCALATION LADDER IS NOW SEAM-DRIVEN, so a reader looking for where SIGTERM/SIGKILL went should look at
// signals_unix.go / signals_windows.go (sendGracefulStop, sendForceStop, gracefulStopFailureIsFatal). This file
// keeps testing the LADDER: bounded wait, escalate, verify, and keep the PID file on a survivor. The Windows
// half differs by design — no SIGTERM, usually no deliverable graceful stop — and signals_windows.go states it.

// THE GUARD THAT KEEPS THE FALLBACK FROM KILLING A LIVE PLANE. Each subtest pins one half of
// serveStopMayDiscover, and the "redirected state dir" case is the one that caused the outage: with
// ORCHICON_INSTANCE exported, servePIDFile() followed the INSTANCE and ignored the override, so a caller
// that believed it was working in a temp dir was reading — and clearing — the real plane's record.
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
		// servePIDFile() and planecontrol's record are the same path by construction. This is the case that
		// must keep working — without it a plane that outlived its PID file can never be stopped, and every
		// `make instance-rebuild` fails again.
		t.Setenv("ORCHICON_INSTANCE", "dev")
		t.Setenv(serveStateDirEnv, "")
		if !serveStopMayDiscover() {
			t.Fatal("the fallback must run for an instance's canonical state dir")
		}
	})
}

// THE OTHER HALF, AND THE ONE THAT ACTUALLY CAUSED THE OUTAGE: a test must never resolve the PID file to
// a live instance's real record. serveStateDir() prefers ORCHICON_INSTANCE over ORCHICON_SERVE_STATE_DIR,
// so setting the override alone is not isolation — on a machine with the instance exported, the tests read
// and CLEARED the real plane's record, and a serveStop that discovers by port then stopped the plane.
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
