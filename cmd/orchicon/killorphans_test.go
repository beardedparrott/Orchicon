//go:build !windows

package main

// killorphans_test.go — the orphan sweep's tests, which are Unix-shaped BY NATURE rather than by
// accident: the fixture makes a REAL orphan by spawning `/bin/sh` and cleans it up with SIGKILL.

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

// The plane's boot sweep (killOrphans) must reap what a CRASHED plane left behind and nothing else.

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

// shellQuote now lives in shquote_test.go, which has no build tag: it is pure Go and is shared with
// the untagged binaries_test.go.

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

// startOrphanedSleep starts a sleep whose parent has already exited, leaving it reparented to
// whatever this environment hands orphans to — exactly the shape a crashed plane leaves.
func startOrphanedSleep(t *testing.T, marked bool) int {
	t.Helper()
	// The backgrounded sleep must not inherit the shell's stdout: with the inherited pipe still open,
	// Command.Output() would block on EOF until the sleep exits (300s).
	cmd := exec.Command("/bin/sh", "-c", "/bin/sleep 300 >/dev/null 2>&1 & echo $!; echo $$")
	if marked {
		// The shell exports it, so the backgrounded sleeper inherits it — the same way the plane's marker
		// reaches the `orchicon mcp` sidecars that opencode (not the plane) starts.
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

// running reports whether pid is still executing.
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

// waitForPlaneMarker polls until pid's environ shows the plane spawn marker — i.e. until the child
// has completed execve.
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

// A live-parented process survives the sweep; a genuine orphan is reaped.
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

// ORCHICON_SANDBOX_PLANE still suppresses the sweep ENTIRELY: the orphan survives and pgrep is
// never even invoked.
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

// reparentedAway is the whole safety property of the sweep, so it is asserted directly: our own
// child is not an orphan, a reparented process.
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

// TestKillOrphansSparesAnUnmarkedOrphan is the regression guard for the POINT of this whole sweep:
// an `opencode` the operator started is not ours to kill, however orphaned it becomes.
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

// The one pid the reap set must NEVER contain is this process: its children are LIVE children, and
// reaping them would be the worst possible failure of the sweep.
func TestReapPointsNeverIncludeSelf(t *testing.T) {
	reap := reapPoints()
	if reap[os.Getpid()] {
		t.Errorf("reapPoints() includes this process (%d): its own children are live and must never be reaped", os.Getpid())
	}
	if !reap[1] {
		t.Error("reapPoints() must always include PID 1: an environment with no subreaper hands orphans to init")
	}
}
