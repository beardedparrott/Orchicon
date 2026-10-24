//go:build !windows

package planecontrol

// orphaned_plane_test.go — a plane that outlived its PID file.
//
// REPORTED (Linux, `make instance-rebuild instance=prod`):
//
//	Control plane (web UI & API): this instance is already installed on port 8091, which is held by a process
//	this tool cannot identify
//
// and, underneath it, `orchicon status prod` reporting "plane: stopped" while :8091 answered /healthz 200.
// MEASURED: the listener was our own `orchicon serve` with ORCHICON_INSTANCE=prod, five days old, its
// /proc/<pid>/exe reading `… (deleted)` after a rebuild replaced the binary, and `serve/pids/` EMPTY.
//
// Every consumer assumed the PID file was the answer:
//
//   - Drift answered Unknown, so Start printed "could not identify the process; leaving it" and returned
//     SUCCESS over a stale binary — the silent failure to upgrade this codebase already records;
//   - Stop ran `serve --stop`, which reads the same missing file, said "serve is not running", and the caller
//     concluded there was nothing to stop;
//   - the install then refused on the held port.
//
// These assert the recovery, in the state it exists for.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// orphanSeams installs the fakes for a plane whose PID file is gone, and returns a cleanup that restores the
// real functions. It deliberately does NOT touch the PID-file seams: the whole point is that no PID file names
// anything.
func orphanSeams(t *testing.T, pid int, terminateErr error, stillAlive bool) *int {
	t.Helper()
	oldUnrec, oldTerm, oldPID := unrecordedPlane, terminateProcess, pidAlive
	t.Cleanup(func() { unrecordedPlane, terminateProcess, pidAlive = oldUnrec, oldTerm, oldPID })

	terminated := new(int)
	*terminated = 0
	unrecordedPlane = func(string) (int, bool) {
		if pid == 0 {
			return 0, false
		}
		return pid, true
	}
	terminateProcess = func(got int) error {
		if got != pid {
			t.Errorf("terminateProcess(%d), want %d", got, pid)
		}
		*terminated = got
		return terminateErr
	}
	pidAlive = func(int) bool { return stillAlive }
	return terminated
}

func TestStopStopsAPlaneThatLostItsPIDFile(t *testing.T) {
	f := newFakeSeams(t)
	// `serve --stop` fails, exactly as it does against a missing PID file.
	f.runErr = errors.New("exit status 1: serve is not running")
	f.alive = func(string) bool { return false }

	terminated := orphanSeams(t, 595842, nil, false)

	if err := Stop(Options{Instance: "prod"}); err != nil {
		t.Fatalf("Stop: %v — a plane it could identify and stop must not be reported as a failure", err)
	}
	if *terminated != 595842 {
		t.Fatalf("the orphaned plane was not stopped (terminateProcess called with %d). This is the case that "+
			"left prod serving a five-day-old binary while every command said nothing was running", *terminated)
	}
}

func TestStopDoesNotGuessWhenTheOrphanCannotBeProven(t *testing.T) {
	f := newFakeSeams(t)
	f.runErr = errors.New("exit status 1: serve is not running")
	f.alive = func(string) bool { return false }

	// pid 0 = "I could not verify this listener is ours".
	terminated := orphanSeams(t, 0, nil, false)

	if err := Stop(Options{Instance: "prod"}); err != nil {
		t.Fatalf("Stop: %v — with nothing identified and nothing recorded, there is nothing to stop", err)
	}
	if *terminated != 0 {
		t.Fatalf("Stop signalled pid %d without proof it belongs to this instance. An unidentified listener "+
			"is a port conflict to REPORT, never a process to kill", *terminated)
	}
}

func TestStopReportsAPlaneThatWillNotDie(t *testing.T) {
	f := newFakeSeams(t)
	f.runErr = errors.New("exit status 1: serve is not running")
	f.alive = func(string) bool { return false }

	// Signalled, but still there: a stop that cannot stop is not a success.
	orphanSeams(t, 595842, nil, true)

	err := Stop(Options{Instance: "prod"})
	if err == nil {
		t.Fatal("Stop reported success for a plane that survived SIGTERM. A stop that cannot stop is not a " +
			"success — that posture is the whole reason `serve --stop` exits non-zero")
	}
	if !strings.Contains(err.Error(), "595842") {
		t.Errorf("the failure does not name the pid: %v", err)
	}
}

func TestStopSurfacesATerminationError(t *testing.T) {
	f := newFakeSeams(t)
	f.runErr = errors.New("exit status 1: serve is not running")
	f.alive = func(string) bool { return false }

	orphanSeams(t, 595842, errors.New("operation not permitted"), false)

	err := Stop(Options{Instance: "prod"})
	if err == nil {
		t.Fatal("Stop swallowed a termination failure")
	}
	if !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("the failure does not include the cause: %v", err)
	}
}

func TestStatusOfReportsTheOrphanedPlane(t *testing.T) {
	// The reported screen said "plane: stopped" for a plane answering /healthz. It must now name the process
	// it found, and its verdict must be the OS's (there is no PID file to ask).
	f := newFakeSeams(t)
	f.writePortRecord(t, "prod", map[string]int{"ORCHICON_CONTROL_PORT": 18122})
	f.alive = func(string) bool { return false }
	orphanSeams(t, 595842, nil, true)

	st, err := StatusOf(Options{Instance: "prod"})
	if err != nil {
		t.Fatalf("StatusOf: %v", err)
	}
	if st.PID != 595842 {
		t.Errorf("status PID = %d, want 595842 — the listener on the recorded control port IS the plane, and "+
			"reporting 0 is what made every caller conclude nothing was installed or running", st.PID)
	}
	if !st.Alive {
		t.Error("status Alive = false for a verified, running listener; there is no PID file to ask, so the " +
			"OS's answer is the only one available")
	}
	if st.Binary != f.binary {
		t.Errorf("status Binary = %q, want %q (read from the recovered pid)", st.Binary, f.binary)
	}
}

// TestDriftResolvesThePlaneThroughTheRecoverableIdentity is a WIRING guard, and deliberately so.
//
// Drift's whole body resolves `planeBinary()` and calls procExe on the pid — neither of which is a seam, so a
// unit test cannot drive its verdict without resolving a real plane binary, and one that tried would be
// testing the test host rather than the code. What CAN be pinned is the line the fix turned on: Drift asks
// effectivePlanePID (PID file, then the verified listener) instead of ServingPlanePID (PID file only). Asking
// the latter is what answered Unknown and let `Start` leave a superseded binary serving while reporting
// success.
//
// The behaviour itself is recorded from a LIVE measurement in the fix that introduced this: for the reported
// plane — five days old, PID file gone, /proc/<pid>/exe reading "(deleted)" — StatusOf reported PID 595842,
// Alive true, Drift "binary superseded".
func TestDriftResolvesThePlaneThroughTheRecoverableIdentity(t *testing.T) {
	src, err := os.ReadFile("hostproc_unix.go")
	if err != nil {
		t.Fatalf("cannot read hostproc_unix.go: %v", err)
	}
	body := string(src)
	i := strings.Index(body, "func Drift(")
	if i < 0 {
		t.Fatal("Drift moved; this guard must move with it")
	}
	drift := body[i:]
	if end := strings.Index(drift, "\nfunc "); end > 0 {
		drift = drift[:end]
	}
	if !strings.Contains(drift, "effectivePlanePID(instance)") {
		t.Error("Drift no longer resolves the plane through effectivePlanePID, so a plane that outlived its " +
			"PID file answers Unknown — which is what made `Start` report success over a stale binary")
	}
	if strings.Contains(drift, "ServingPlanePID(instance)") {
		t.Error("Drift resolves the plane from the PID file alone again; a plane whose PID file is gone is " +
			"then unidentifiable and is never replaced")
	}
}

func TestIsPlaneBinaryName(t *testing.T) {
	// The `(deleted)` suffix is the COMMON case in this state, not an edge one: replacing the binary is
	// exactly what severs it from its PID file. `orch` is the thin client and never serves a plane.
	for _, ok := range []string{
		"/opt/orchicon/bin/orchicon",
		"/home/u/projects/Orchicon/bin/orchicon (deleted)",
	} {
		if !isPlaneBinaryName(ok) {
			t.Errorf("isPlaneBinaryName(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{
		"/usr/bin/orch",              // the thin client
		"/opt/orchicon/bin/postgres", // a service, not the plane
		"",
	} {
		if isPlaneBinaryName(no) {
			t.Errorf("isPlaneBinaryName(%q) = true, want false", no)
		}
	}
}
