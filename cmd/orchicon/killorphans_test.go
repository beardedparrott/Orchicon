package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The plane's boot sweep (killOrphans) must reap what a CRASHED plane left
// behind and nothing else. Inside a container that distinction never mattered
// — the only matching processes were the plane's own children — but a
// HOST-resident plane's pgrep sees the whole user session, where the
// operator's own `opencode`, a sibling instance's plane and its `orchicon mcp`
// sidecars are legitimate processes with LIVE parents. A killed sibling plane
// is restarted by its watchdog; a killed `opencode` is simply gone.
//
// The suite runs on the HOST, which is where users are, so it assumes nothing
// about the host's reparenting behaviour: it establishes where this environment
// actually hands orphans (see startOrphanedSleep) and asserts the guard agrees.
//
// These tests drive the REAL killOrphans against REAL processes, with a fake
// `pgrep` first on PATH so the matched PIDs are ours to choose, and assert with
// real signals and real /proc parentage. They never depend on a real pgrep
// (the build image has none) and never sweep the machine's real processes.

// fakePgrep puts an executable pgrep shim first on PATH that records its
// arguments and prints $FIXTURE_PIDS, one PID per line.
func fakePgrep(t *testing.T) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + shellQuote(marker) + "\n" +
		"printf '%s\\n' \"${FIXTURE_PIDS:-}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pgrep"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return marker
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// startLiveParentedSleep starts a sleep that is THIS process's child: its
// parent is alive, so it is not an orphan.
func startLiveParentedSleep(t *testing.T, marked bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "300")
	if marked {
		cmd.Env = append(os.Environ(), planeSpawnMarker+"=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

// startOrphanedSleep starts a sleep whose parent has already exited, leaving it
// reparented to whatever this environment hands orphans to — exactly the shape
// a crashed plane leaves.
//
// NOTHING HERE ASSUMES PID 1. Where an orphan lands is an environment
// property: a container has no subreaper, so reparenting ends at PID 1, while a
// host user session hands it to the session manager (which sets
// PR_SET_CHILD_SUBREAPER, so reparenting stops there). The test therefore
// establishes its OWN ground truth — the sleeper's parent really is no longer
// the shell that spawned it — and adapts to whichever pid that turns out to be.
// Hardcoding PID 1 here is what made this suite pass in a container and fail on
// a host, which is backwards: the host is where the plane now runs.
func startOrphanedSleep(t *testing.T, marked bool) int {
	t.Helper()
	// The backgrounded sleep must not inherit the shell's stdout: with the
	// inherited pipe still open, Command.Output() would block on EOF until the
	// sleep exits (300s), hanging the test instead of leaving an orphan.
	//
	// $$ is the shell's own pid. The sleeper STARTS as that shell's child, so
	// "its parent is no longer the shell" is the reparenting event itself, and
	// needs no assumption about where it reparents TO.
	cmd := exec.Command("/bin/sh", "-c", "/bin/sleep 300 >/dev/null 2>&1 & echo $!; echo $$")
	if marked {
		// The shell exports it, so the backgrounded sleeper inherits it — the
		// same way the plane's marker reaches the `orchicon mcp` sidecars that
		// opencode (not the plane) starts.
		cmd.Env = append(os.Environ(), planeSpawnMarker+"=1")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawn a reparented sleep: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		t.Fatalf("expected '<sleep-pid> <shell-pid>' from the fixture shell, got %q", out)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse the orphan pid from %q: %v", fields[0], err)
	}
	spawner, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse the shell pid from %q: %v", fields[1], err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ppidOf(t, pid) != spawner {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never left the shell that spawned it (pid %d): reparenting did not happen, so the orphan contract cannot be established here", pid, spawner)
	return 0
}

// ppidOf reads a process's parent from /proc directly — deliberately NOT via
// reparentedAway, so a broken reparentedAway cannot make these tests pass.
func ppidOf(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", pid, err)
	}
	return intField(t, string(data), "PPid:")
}

func intField(t *testing.T, status, field string) int {
	t.Helper()
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, field) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, field)))
		if err != nil {
			t.Fatalf("parse %q in %q: %v", field, line, err)
		}
		return n
	}
	t.Fatalf("no %s line in:\n%s", field, status)
	return -1
}

// running reports whether pid is still executing. A zombie counts as NOT
// running: it has been terminated and merely not reaped yet (reaping belongs to
// whichever reaper owns it in this environment) — termination is what the sweep
// promises either way.
func running(t *testing.T, pid int) bool {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "State:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "State:"))
		if len(fields) == 0 {
			return true
		}
		return fields[0] != "Z" && fields[0] != "X"
	}
	return true
}

func waitNotRunning(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !running(t, pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d is still running after %s (the sweep did not reap it)", pid, within)
}

// waitForPlaneMarker polls until pid's environ shows the plane spawn marker —
// i.e. until the child has completed execve.
//
// Reading /proc/<pid>/environ in the window between fork and execve returns the
// PARENT's environment, because cmd.Env is applied BY execve. An immediate read
// after Start therefore misses the marker completely (measured: false
// immediately, true ~150ms later, every time).
//
// The sweep never races this way — it inspects processes that have been running
// long enough to be left behind — but a test that asserts on a child it started
// microseconds ago must, or it asserts on the fork rather than the exec.
func waitForPlaneMarker(t *testing.T, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if carriesPlaneMarker(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A live-parented process survives the sweep; a genuine orphan is reaped. Both
// are asserted with real processes and real signals, and the discovery step is
// asserted to still use the documented pgrep forms for BOTH patterns.
func TestKillOrphansReapsReparentedAndSparesLiveParent(t *testing.T) {
	marker := fakePgrep(t)
	live := startLiveParentedSleep(t, true)
	orphan := startOrphanedSleep(t, true)
	t.Setenv("FIXTURE_PIDS", fmt.Sprintf("%d\n%d", live.Process.Pid, orphan))
	t.Setenv("ORCHICON_SANDBOX_PLANE", "")

	// Wait for the marked child to finish execve, so its survival is evidence
	// about the PARENTAGE check and not about a marker read that raced exec.
	if !waitForPlaneMarker(t, live.Process.Pid, 3*time.Second) {
		t.Fatalf("pid %d never showed the plane marker; this test cannot isolate the parentage check", live.Process.Pid)
	}

	killOrphans()

	if !running(t, live.Process.Pid) {
		t.Errorf("the boot sweep killed pid %d, which has a LIVE parent — on a host plane that is the operator's own opencode", live.Process.Pid)
	}
	waitNotRunning(t, orphan, 5*time.Second)

	// Discovery must still cover both patterns, with the multi-word one matched
	// against the command line.
	calls := readFile(t, marker)
	for _, want := range []string{"-x opencode", "-f orchicon mcp"} {
		if !strings.Contains(calls, want) {
			t.Errorf("pgrep was not invoked as %q; calls were:\n%s", want, calls)
		}
	}
}

// ORCHICON_SANDBOX_PLANE still suppresses the sweep ENTIRELY: the orphan
// survives and pgrep is never even invoked. The control run first proves the
// same setup would otherwise reap it, so the suppression assertion cannot pass
// vacuously.
func TestKillOrphansSuppressedBySandboxPlane(t *testing.T) {
	marker := fakePgrep(t)

	control := startOrphanedSleep(t, true)
	t.Setenv("FIXTURE_PIDS", strconv.Itoa(control))
	t.Setenv("ORCHICON_SANDBOX_PLANE", "")
	killOrphans()
	waitNotRunning(t, control, 5*time.Second)

	orphan := startOrphanedSleep(t, true)
	t.Setenv("FIXTURE_PIDS", strconv.Itoa(orphan))
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Setenv("ORCHICON_SANDBOX_PLANE", "1")
	killOrphans()
	time.Sleep(300 * time.Millisecond)

	if !running(t, orphan) {
		t.Errorf("ORCHICON_SANDBOX_PLANE=1 did not suppress the sweep: pid %d was terminated", orphan)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("ORCHICON_SANDBOX_PLANE=1 did not suppress the sweep: pgrep was invoked")
	}
}

// reparentedAway is the whole safety property of the sweep, so it is asserted
// directly: our own child is not an orphan, a reparented process is, and PID 1
// (whose parent is 0) and a nonexistent pid never are.
//
// The reparented half is asserted against a process this environment ACTUALLY
// reparented (startOrphanedSleep observes the reparenting rather than assuming
// its target), so this test is a real check of the host's reap point rather
// than a restatement of the container's.
func TestReparentedAway(t *testing.T) {
	reap := reapPoints()
	live := startLiveParentedSleep(t, true)
	if reparentedAway(live.Process.Pid, reap) {
		t.Errorf("pid %d is this test's own child (parent %d), but reparentedAway called it an orphan", live.Process.Pid, os.Getpid())
	}
	orphan := startOrphanedSleep(t, true)
	if !reparentedAway(orphan, reap) {
		t.Errorf("pid %d was reparented away from the shell that spawned it, but reparentedAway called it not an orphan — the reap points resolved as %v, which is where this environment hands orphans", orphan, reap)
	}
	if reparentedAway(1, reap) {
		t.Error("PID 1 reported as an orphan: its parent is 0, and init is never the sweep's to kill")
	}
	if reparentedAway(1<<30, reap) {
		t.Error("a nonexistent pid reported as an orphan")
	}
}

// TestKillOrphansSparesAnUnmarkedOrphan is the regression guard for the POINT
// of this whole sweep: an `opencode` the operator started is not ours to kill,
// however orphaned it becomes.
//
// This is the case parentage alone gets wrong. Start opencode in a terminal,
// close the terminal, and it reparents to exactly where a crashed plane's
// leftovers reparent to — the same reaper, the same ppid. Nothing about its
// parentage marks it as the operator's. Only the absence of the plane spawn
// marker does, and without that check the sweep would kill it at the next boot.
func TestKillOrphansSparesAnUnmarkedOrphan(t *testing.T) {
	fakePgrep(t)

	// Control FIRST: the identical shape, but MARKED, is reaped — so the
	// assertion below cannot pass merely because the sweep did nothing.
	marked := startOrphanedSleep(t, true)
	t.Setenv("FIXTURE_PIDS", strconv.Itoa(marked))
	t.Setenv("ORCHICON_SANDBOX_PLANE", "")
	if !waitForPlaneMarker(t, marked, 3*time.Second) {
		t.Fatalf("pid %d never showed the plane marker; the control run cannot prove the sweep was armed", marked)
	}
	killOrphans()
	waitNotRunning(t, marked, 5*time.Second)

	unmarked := startOrphanedSleep(t, false)
	t.Setenv("FIXTURE_PIDS", strconv.Itoa(unmarked))
	killOrphans()
	time.Sleep(300 * time.Millisecond)

	if !running(t, unmarked) {
		t.Errorf("pid %d is a genuine orphan but carries no plane marker — on a host that is the operator's own opencode, and the sweep killed it", unmarked)
	}
}

// carriesPlaneMarker is what makes the sweep safe to run on a host, so it is
// asserted directly against real processes rather than trusted.
func TestCarriesPlaneMarker(t *testing.T) {
	marked := startLiveParentedSleep(t, true)
	if !waitForPlaneMarker(t, marked.Process.Pid, 3*time.Second) {
		t.Errorf("pid %d was started with %s=1 but carriesPlaneMarker never reported it", marked.Process.Pid, planeSpawnMarker)
	}
	unmarked := startLiveParentedSleep(t, false)
	if carriesPlaneMarker(unmarked.Process.Pid) {
		t.Errorf("pid %d carries no marker but carriesPlaneMarker said yes", unmarked.Process.Pid)
	}
	if carriesPlaneMarker(1 << 30) {
		t.Error("a nonexistent pid reported as carrying the plane marker")
	}
}

// The one pid the reap set must NEVER contain is this process: its children are
// LIVE children, and reaping them would be the worst possible failure of the
// sweep. The walk up the tree can land on us when we are a direct child of PID
// 1 (a container, or a service), so this is asserted rather than assumed.
func TestReapPointsNeverIncludeSelf(t *testing.T) {
	reap := reapPoints()
	if reap[os.Getpid()] {
		t.Errorf("reapPoints() includes this process (%d): its own children are live and must never be reaped", os.Getpid())
	}
	if !reap[1] {
		t.Error("reapPoints() must always include PID 1: an environment with no subreaper hands orphans to init")
	}
}
