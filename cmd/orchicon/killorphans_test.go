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
func startLiveParentedSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "300")
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
func startOrphanedSleep(t *testing.T) int {
	t.Helper()
	// The backgrounded sleep must not inherit the shell's stdout: with the
	// inherited pipe still open, Command.Output() would block on EOF until the
	// sleep exits (300s), hanging the test instead of leaving an orphan.
	//
	// $$ is the shell's own pid. The sleeper STARTS as that shell's child, so
	// "its parent is no longer the shell" is the reparenting event itself, and
	// needs no assumption about where it reparents TO.
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 300 >/dev/null 2>&1 & echo $!; echo $$").Output()
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

// A live-parented process survives the sweep; a genuine orphan is reaped. Both
// are asserted with real processes and real signals, and the discovery step is
// asserted to still use the documented pgrep forms for BOTH patterns.
func TestKillOrphansReapsReparentedAndSparesLiveParent(t *testing.T) {
	marker := fakePgrep(t)
	live := startLiveParentedSleep(t)
	orphan := startOrphanedSleep(t)
	t.Setenv("FIXTURE_PIDS", fmt.Sprintf("%d\n%d", live.Process.Pid, orphan))
	t.Setenv("ORCHICON_SANDBOX_PLANE", "")

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

	control := startOrphanedSleep(t)
	t.Setenv("FIXTURE_PIDS", strconv.Itoa(control))
	t.Setenv("ORCHICON_SANDBOX_PLANE", "")
	killOrphans()
	waitNotRunning(t, control, 5*time.Second)

	orphan := startOrphanedSleep(t)
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
	live := startLiveParentedSleep(t)
	if reparentedAway(live.Process.Pid, reap) {
		t.Errorf("pid %d is this test's own child (parent %d), but reparentedAway called it an orphan", live.Process.Pid, os.Getpid())
	}
	orphan := startOrphanedSleep(t)
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
