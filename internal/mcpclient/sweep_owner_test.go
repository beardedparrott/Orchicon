//go:build !windows

package mcpclient

// sweep_owner_test.go — the sweep's rule is OWNERSHIP, not ppid, and a failed start must not leak a
// child.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// deadPID returns a pid that is certainly not running, by starting and reaping a process.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a throwaway process: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	return pid
}

// spawnMarkedChild starts the fixture child with an explicit OWNER marker and returns its pid.
func spawnMarkedChild(t *testing.T, ownerEnv ...string) int {
	t.Helper()
	argv := fixtureArgs()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID=owner-test",
	)
	cmd.Env = append(cmd.Env, ownerEnv...)
	// STDIN MUST STAY OPEN.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	// No SysProcAttr: no PDEATHSIG, so the child survives its parent and can be swept.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start marked child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
	})
	return pid
}

// processGone reports whether pid no longer names a RUNNING process.
func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err != nil {
			return true // gone from /proc entirely
		} else if idx := strings.LastIndexByte(string(stat), ')'); idx >= 0 {
			if fields := strings.Fields(string(stat[idx+1:])); len(fields) > 0 && fields[0] == "Z" {
				return true // zombie: the process is NOT running
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// THE REPORTED LEAK, pinned: a child whose recorded owner is DEAD is reaped even though its ppid is a live
// process that is not init.
func TestSweepReapsAChildWhoseRecordedOwnerIsGone(t *testing.T) {
	dead := deadPID(t)
	// A start time is deliberately OMITTED: the owner pid alone must be enough to establish "gone", which
	// is the fallback path in planeOwner.alive.
	pid := spawnMarkedChild(t, mcpPlanePIDEnv+"="+strconv.Itoa(dead), mcpPlaneStartEnv+"=0")

	SweepStaleChildren(context.Background(), testLogger(t))

	if !processGone(pid, 3*time.Second) {
		t.Fatalf("an MCP child whose owning plane (pid %d) is dead was NOT reaped. Its ppid is this live "+
			"test process, not 1 — which is precisely the reported case: reparented to a live pid, skipped "+
			"forever by the old ppid==1 rule", dead)
	}
}

// THE SAFETY HALF: a child whose owner is genuinely alive must be left alone. Reaping a live plane's server
// mid-session would be worse than leaking one.
func TestSweepLeavesAChildWhoseOwnerIsAlive(t *testing.T) {
	owner := os.Getpid()
	pid := spawnMarkedChild(t,
		mcpPlanePIDEnv+"="+strconv.Itoa(owner),
		mcpPlaneStartEnv+"="+strconv.FormatUint(procStartTime(owner), 10),
		mcpPlaneBootEnv+"="+bootID(),
	)

	SweepStaleChildren(context.Background(), testLogger(t))

	if processGone(pid, 400*time.Millisecond) {
		t.Fatal("the sweep reaped a child whose owning plane is ALIVE — that is a live session's server killed " +
			"mid-turn, which is worse than the leak it was meant to fix")
	}
}

// PID REUSE IS NOT OWNERSHIP. A pid that exists but belongs to a different process (different start time)
// is a dead owner, not a live one.
func TestSweepTreatsAReusedPIDAsADeadOwner(t *testing.T) {
	owner := os.Getpid()
	actual := procStartTime(owner)
	if actual == 0 {
		t.Skip("cannot read this process's start time on this host")
	}
	// Same pid, WRONG start time — indistinguishable from pid reuse by any liveness probe.
	pid := spawnMarkedChild(t,
		mcpPlanePIDEnv+"="+strconv.Itoa(owner),
		mcpPlaneStartEnv+"="+strconv.FormatUint(actual+1, 10),
	)

	SweepStaleChildren(context.Background(), testLogger(t))

	if !processGone(pid, 3*time.Second) {
		t.Fatal("a child whose recorded owner start time does not match the live process was NOT reaped — " +
			"that is pid reuse being read as ownership, the exact failure the start time exists to prevent")
	}
}

// THE LEAK ITSELF: a start that times out must not leave a live child behind.
func TestAFailedStartDoesNotLeakTheChild(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	m := NewManager(testLogger(t))
	t.Cleanup(func() { _ = m.Close() })

	// A server that starts, announces its pid, and then never answers the handshake.
	spec := ServerSpec{
		ID:      "hangs",
		Type:    TypeStdio,
		Timeout: 700 * time.Millisecond,
		Command: []string{sh, "-c", "echo $$ > " + pidFile + "; sleep 60"},
	}
	if _, err := m.Start(context.Background(), []ServerSpec{spec}); err == nil {
		t.Fatal("Start succeeded for a server that never answers the handshake")
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("the child never started (no pid file): %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid <= 0 {
		t.Fatalf("unreadable pid file %q", string(raw))
	}
	if !processGone(pid, 3*time.Second) {
		t.Fatalf("the child (pid %d) survived a FAILED start — every failed start leaks a live MCP server, "+
			"which is how one was found still running 18 hours after its plane died", pid)
	}
}
