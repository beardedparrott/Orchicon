package planecontrol

// stop_test.go — AC5: a stop is graceful (it re-execs `serve --stop`, the one implementation that SIGTERMs and
// escalates) and BOUNDED, and it REPORTS failure rather than hanging when the socket is never released.

import (
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
