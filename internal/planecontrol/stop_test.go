package planecontrol

// stop_test.go — AC5: a stop is graceful (it re-execs `serve --stop`, the one implementation that SIGTERMs and
// escalates) and BOUNDED, and it REPORTS failure rather than hanging when the socket is never released.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestStopIsBoundedAndReportsFailure(t *testing.T) {
	f := newFakeSeams(t)
	f.alive = func(string) bool { return true } // the plane never goes away
	dir := t.TempDir()

	err := Stop(Options{Instance: "dev", HostDataDir: dir})
	if err == nil {
		t.Fatal("a stop that never releases the socket must FAIL, not report success")
	}
	if !strings.Contains(err.Error(), "did not stop") || !strings.Contains(err.Error(), "dev") {
		t.Errorf("the failure must name the state and the instance; got: %v", err)
	}
	if f.aliveCall != stopPolls {
		t.Errorf("the wait must be BOUNDED to %d polls, ran %d", stopPolls, f.aliveCall)
	}
	if f.sleeps != stopPolls {
		t.Errorf("the wait must sleep between polls exactly %d times, slept %d", stopPolls, f.sleeps)
	}
	if len(f.runs) != 1 || strings.Join(f.runs[0].args, " ") != "serve --stop" {
		t.Fatalf("the stop must re-exec `serve --stop` exactly once; got %v", f.runs)
	}
	// PER-INSTANCE: the child carries THIS instance's state dir, or it would stop a sibling's plane.
	want := ServeStateDirEnv + "=" + filepath.Join(dir, "serve")
	if got := envValue(f.runs[0].env, ServeStateDirEnv); got != filepath.Join(dir, "serve") {
		t.Errorf("the child env must carry %s; got %q", want, got)
	}
}

func TestStopReportsSuccessOnceTheSocketIsReleased(t *testing.T) {
	f := newFakeSeams(t)
	calls := 0
	f.alive = func(string) bool {
		calls++
		return calls < 2 // gone on the second poll
	}
	if err := Stop(Options{Instance: "dev", HostDataDir: t.TempDir()}); err != nil {
		t.Fatalf("a released socket must report success: %v", err)
	}
	if calls != 2 {
		t.Errorf("the wait must stop as soon as the PID file process is gone (polled %d times)", calls)
	}
}

// TestStopOfAnAlreadyStoppedPlaneIsANoOp — `serve --stop` exits 1 with "serve is not running" when there is
// nothing to stop, and that must NOT surface as a failure: `down`, a rebuild and `plane stop` are all run
// twice, and an error here aborted them on an instance that was already down.
func TestStopOfAnAlreadyStoppedPlaneIsANoOp(t *testing.T) {
	f := newFakeSeams(t)
	f.runErr = errors.New("exit status 1") // what `serve --stop` returns for "serve is not running"
	if err := Stop(Options{Instance: "dev", HostDataDir: t.TempDir()}); err != nil {
		t.Fatalf("stopping an instance that is not running must be a NO-OP, not a failure: %v", err)
	}
	if len(f.runs) != 1 || strings.Join(f.runs[0].args, " ") != "serve --stop" {
		t.Fatalf("the graceful stop must still be attempted (and clean up a stale PID file); got %v", f.runs)
	}
}

// TestStopReportsAFailedStopOfALivePlane — the other half of the same distinction. A process still alive after
// `serve --stop` is a stop that FAILED and must be reported, never swallowed as "nothing was running".
func TestStopReportsAFailedStopOfALivePlane(t *testing.T) {
	f := newFakeSeams(t)
	f.runErr = errors.New("exit status 1")
	f.alive = func(string) bool { return true }

	err := Stop(Options{Instance: "dev", HostDataDir: t.TempDir()})
	if err == nil {
		t.Fatal("a plane still alive after `serve --stop` must be reported as a FAILED stop")
	}
	if !strings.Contains(err.Error(), "dev") {
		t.Errorf("the failure must name the instance; got: %v", err)
	}
}
