//go:build windows

package planecontrol

// hostproc_windows_test.go — the Windows process-identity probes, asserted on a real Windows host (this file is
// COMPILED by `make cross-compile`'s go vet and RUN only where the platform exists).
//
// WHAT IT PINS, and why each case is here rather than in an untagged test: the previous Windows
// implementation was four stubs that returned "nothing" for everything, which made `plane status` report no
// PID for a running plane and `plane stop` a silent no-op. These cases fail if that ever comes back.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedPlanePIDFile points this package's home-directory seam at a temp dir and writes an instance's PID file.
func seedPlanePIDFile(t *testing.T, instance string, pid int) {
	t.Helper()
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	t.Cleanup(func() { userHomeDir = os.UserHomeDir })
	path := ServePIDFile(instance)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the PID file dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatalf("write the PID file: %v", err)
	}
}

// TestProcExeNamesThisBinary: QueryFullProcessImageNameW must resolve THIS test binary's image path. If it
// returns an error or another path, the drift comparison can never answer `Current`.
func TestProcExeNamesThisBinary(t *testing.T) {
	exe, err := procExe(os.Getpid())
	if err != nil {
		t.Fatalf("procExe(this pid): %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(exe), filepath.Clean(self)) {
		t.Errorf("procExe(this pid) = %q, want this binary %q (case-insensitively — Windows paths are)", exe, self)
	}
}

// TestProcessAliveAnswersForSelfAndForNothing: the exit-code probe must say yes for a live PID and no for one
// that does not exist. It is the whole liveness answer on this platform (no /proc, no zombies).
func TestProcessAliveAnswersForSelfAndForNothing(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("processAlive(this pid) = false; a running process must read as alive")
	}
	if processAlive(999999999) {
		t.Error("processAlive(999999999) = true; a PID that cannot be opened is not alive")
	}
	if _, ok := procStateChar(os.Getpid()); ok {
		t.Error("procStateChar reported a POSIX process state on Windows, where there is none")
	}
}

// TestServingPlanePIDFindsALivePlane is the AC3 case: `plane status` must find a detached plane on Windows.
func TestServingPlanePIDFindsALivePlane(t *testing.T) {
	seedPlanePIDFile(t, "wintest", os.Getpid())
	pid, ok := ServingPlanePID("wintest")
	if !ok || pid != os.Getpid() {
		t.Fatalf("ServingPlanePID = (%d,%v), want (%d,true) — the PID file names a live process", pid, ok, os.Getpid())
	}
	if !ServingPlaneAlive("wintest") {
		t.Error("ServingPlaneAlive = false for a live PID-file process; `plane stop` would be a silent no-op")
	}
}

// TestServingPlanePIDRejectsAStalePIDFile: the PID file outlives its process, so a stale one must not report a
// plane.
func TestServingPlanePIDRejectsAStalePIDFile(t *testing.T) {
	seedPlanePIDFile(t, "win-stale", 999999999)
	if pid, ok := ServingPlanePID("win-stale"); ok {
		t.Errorf("ServingPlanePID = (%d,true) for a PID that does not exist", pid)
	}
	if ServingPlaneAlive("win-stale") {
		t.Error("ServingPlaneAlive = true for a stale PID file")
	}
}

// TestDriftIsCurrentForOurOwnBinaryAndNeverDrifted: `Current` when a live PID file names this binary, and NEVER
// `Drifted` — a running Windows image cannot be unlinked, so the Unix drift fingerprint cannot arise (see
// hostproc_windows.go). The second half is the property a future reader would otherwise have to rediscover.
func TestDriftIsCurrentForOurOwnBinaryAndNeverDrifted(t *testing.T) {
	seedPlanePIDFile(t, "win-drift", os.Getpid())
	var out bytes.Buffer
	if got := Drift("win-drift", &out); got != Current {
		t.Fatalf("Drift = %v, want Current (the PID file names this very binary); output: %s", got, out.String())
	}
}

// TestTerminateProcessRefusesThisProcess: force-stopping is SIGKILL semantics with no shutdown path, so it must
// never be aimed at the caller.
func TestTerminateProcessRefusesThisProcess(t *testing.T) {
	if err := TerminateProcess(os.Getpid()); err == nil {
		t.Fatal("TerminateProcess(this pid) = nil; it must refuse to kill the caller")
	}
}
