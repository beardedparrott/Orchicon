package desktop

// relaunch_test.go — the self-restart a server switch performs.
//
// REPORTED: "If you close the desktop app and re-open, THEN you are connected to the other instance. We may
// want to build in an ability to just auto restart the desktop app once someone hits connect."
//
// These assert the two parts that can be got wrong silently: the argv a replacement is started with, and the
// wait that keeps two windows off the screen at once.

import (
	"strings"
	"testing"
	"time"
)

func TestRelaunchArgsCarryTheInstanceAndRoute(t *testing.T) {
	got := RelaunchArgs(4242, "prod", "/manage-server")
	joined := strings.Join(got, " ")

	// THE PID, so the replacement can wait for this process to die.
	if !strings.Contains(joined, "--await-pid 4242") {
		t.Errorf("RelaunchArgs = %q, want it to carry --await-pid 4242", joined)
	}
	// THE INSTANCE, EXPLICITLY. Not left to the remembered preference: that write is best-effort, and a
	// switch that failed to persist must still reopen the right server.
	if !strings.Contains(joined, "--instance prod") {
		t.Errorf("RelaunchArgs = %q, want it to name the instance", joined)
	}
	// THE ROUTE, so the operator lands back on the page they switched from rather than the dashboard.
	if !strings.Contains(joined, "--route /manage-server") {
		t.Errorf("RelaunchArgs = %q, want it to keep the operator on the servers page", joined)
	}
}

func TestRelaunchArgsRejectAnUnsafeRoute(t *testing.T) {
	// SafeRoute is the same allowlist a launch argument goes through, so a route cannot become an open
	// redirect by way of a restart.
	for _, bad := range []string{"//evil.example/x", "https://evil.example", "/../../etc/passwd", "/nope"} {
		got := strings.Join(RelaunchArgs(1, "dev", bad), " ")
		if strings.Contains(got, "--route") {
			t.Errorf("RelaunchArgs(%q) = %q — an unapproved route must be dropped, not passed through", bad, got)
		}
	}
}

func TestParseAwaitPIDConsumesItsOwnFlag(t *testing.T) {
	pid, rest, ok := ParseAwaitPID([]string{"--await-pid", "99", "--instance", "prod", "--route", "/manage-server"})
	if !ok || pid != 99 {
		t.Fatalf("ParseAwaitPID = (%d, %v, %v), want (99, _, true)", pid, rest, ok)
	}
	// THE REST MUST BE FLAG-PARSEABLE. A leaked --await-pid would be an undefined flag, and the app would
	// exit without ever opening a window — a far worse failure than the one being fixed.
	if strings.Join(rest, " ") != "--instance prod --route /manage-server" {
		t.Errorf("rest = %v, want the flag removed", rest)
	}

	// An ordinary launch is untouched.
	pid, rest, ok = ParseAwaitPID([]string{"--instance", "dev"})
	if ok || pid != 0 || strings.Join(rest, " ") != "--instance dev" {
		t.Errorf("an ordinary launch was altered: (%d, %v, %v)", pid, rest, ok)
	}

	// A malformed value is consumed and ignored, not read as a positional argument.
	if _, rest, ok := ParseAwaitPID([]string{"--await-pid", "nonsense", "--instance", "dev"}); ok ||
		strings.Join(rest, " ") != "--instance dev" {
		t.Errorf("a malformed --await-pid value was not swallowed: %v", rest)
	}
}

func TestWaitForExitReturnsWhenThePredecessorGoes(t *testing.T) {
	// The replacement must not open a second window while the first is alive, and must not wait for ever if
	// the first is wedged: two windows is bad, no window is worse.
	calls := 0
	alive := func(int) bool {
		calls++
		return calls < 3 // dies on the third look
	}
	waited := WaitForExit(1234, time.Second, alive, func(time.Duration) {})
	if !waited {
		t.Error("WaitForExit reported the predecessor still alive after it went away")
	}
	if calls != 3 {
		t.Errorf("polled %d times, want 3 — it must stop as soon as the pid is gone", calls)
	}
}

func TestWaitForExitGivesUpAtTheDeadline(t *testing.T) {
	waited := WaitForExit(1234, 5*time.Millisecond, func(int) bool { return true }, func(d time.Duration) {
		time.Sleep(d)
	})
	if waited {
		t.Error("WaitForExit reported an exit that never happened; the replacement would never open a " +
			"window if the predecessor hung")
	}
}

func TestWaitForExitWithNoPIDIsImmediate(t *testing.T) {
	// A relaunch with no recorded pid (a hand start) must not block at all.
	if !WaitForExit(0, time.Second, func(int) bool { return true }, func(time.Duration) {}) {
		t.Error("WaitForExit blocked for pid 0")
	}
}
