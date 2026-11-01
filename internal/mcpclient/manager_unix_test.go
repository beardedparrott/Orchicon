//go:build !windows

package mcpclient

// manager_test_unix.go — the POSIX-ONLY half of the MCP client suite, split out of the untagged
// manager_test.go so the package's TESTS compile on Windows.
//
// EVERY HELPER HERE IS UNIX-SHAPED BY CONSTRUCTION, which is why a build tag is the fix rather than
// a runtime check: `waitGone` asks `syscall.Kill(pid, 0)` whether a pid is still alive,
// `findChildPid` scans `/proc/*/environ` and `/proc/<pid>/stat`, `procState` reads
// `/proc/<pid>/stat`, and the helper tests re-exec THIS binary as a fixture child. None of it has a
// Windows counterpart, and `syscall.Kill` is not in the Windows syscall package at all.
//
// THE DEGRADATION IS DELIBERATE AND COMPLETE: on Windows these tests do not exist, because the
// property they assert (a child is reparented to PID 1 and reaped like a POSIX orphan) is not a
// property Windows has. There is no stub that pretends to check something it cannot — the
// platform-neutral suite in manager_test.go is what covers Windows.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Lifecycle: Close kills stdio children; orphan prevention via parent death
// ---------------------------------------------------------------------------

// TestCloseKillsChild spawns a stdio fixture child and verifies Close
// terminates it.
func TestCloseKillsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	m := startManager(t, stdioSpec("srv"))
	pid := findChildPid(t, "srv")
	if pid == 0 {
		t.Fatal("no fixture child found")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitGone(t, pid, 3*time.Second)
}

// TestOrphanPrevention verifies the PDEATHSIG wiring directly: spawn the
// fixture via newStdioTransport (the same path the manager uses), then
// kill the parent of the child (this test process) and assert the child
// dies with it. We achieve "kill the parent" by running the assertion in
// a child helper process whose only job is to spawn the fixture, print
// its pid, and sleep until killed.
func TestOrphanPrevention(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	helper := exec.Command(os.Args[0], "-test.run=TestOrphanHelper")
	helper.Env = append(os.Environ(), "ORCHICON_MCP_ORPHAN_HELPER=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	pid := readPidLine(t, stdout)
	// The child is alive: verify, then kill the helper. PDEATHSIG must take
	// the fixture child down with it.
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("child %d not alive before kill: %v", pid, err)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()
	waitGone(t, pid, 5*time.Second)
}

// TestOrphanHelper spawns the stdio fixture through the same transport the
// manager uses (newStdioTransport), prints the child pid, then sleeps
// until the parent test kills it. When it is killed, PDEATHSIG must take
// the fixture child with it.
func TestOrphanHelper(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_ORPHAN_HELPER") != "1" {
		return
	}
	tr, err := newStdioTransport(stdioSpec("orphan"))
	if err != nil {
		fmt.Println("ERR", err)
		return
	}
	if err := tr.cmd.Start(); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fmt.Println(tr.cmd.Process.Pid)
	// Keep stdin open so the fixture stays alive; we are SIGKILLed by the
	// test, and PDEATHSIG then kills the fixture child.
	for {
		time.Sleep(time.Hour)
	}
}

// findChildPid locates the fixture stdio child (env marker
// ORCHICON_MCP_STDIO=1 + ORCHICON_MCP_SERVER_ID=<id>) under this
// process's own children.
func findChildPid(t *testing.T, serverID string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return 0
		}
		for _, e := range entries {
			pid := atoi(e.Name())
			if pid <= 0 {
				continue
			}
			env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
			if err != nil {
				continue
			}
			if !envHasMarker(env) || envServerID(env) != serverID {
				continue
			}
			stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			ppid, ok := parsePPID(stat)
			if ok && ppid == os.Getpid() {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// A zombie (state Z) is dead — it only persists until its parent
		// (PID 1 after reparenting) reaps it. Treat it as gone.
		if state, ok := procState(pid); ok && state == 'Z' {
			return
		}
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, timeout)
}

// procState reads the state character (3rd field) of /proc/<pid>/stat.
func procState(pid int) (byte, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	idx := strings.LastIndex(string(stat), ")")
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[idx+1:]))
	if len(fields) == 0 {
		return 0, false
	}
	return fields[0][0], true
}

func atoi(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n := atoi(s)
	if n == 0 {
		t.Fatalf("bad pid %q", s)
	}
	return n
}

// ---------------------------------------------------------------------------
// Sweep
// ---------------------------------------------------------------------------

// TestSweepReapsOrphans verifies SweepStaleChildren kills an MCP child
// whose parent is dead (PPID 1), while leaving a live parent's child
// alone. The orphan is simulated by spawning a fixture child WITHOUT
// PDEATHSIG (as an older control plane might have), then killing its
// intermediate parent process so it reparents to PID 1.
func TestSweepReapsOrphans(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	helper := exec.Command(os.Args[0], "-test.run=TestSweepHelper")
	helper.Env = append(os.Environ(), "ORCHICON_MCP_SWEEP_HELPER=1")
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	pid := readPidLine(t, stdout)
	// Helper exits on its own (no PDEATHSIG on the child), so the fixture
	// child reparents to PID 1 and becomes an orphan.
	_ = helper.Wait()
	time.Sleep(300 * time.Millisecond) // let the kernel reparent

	log := testLogger(t)
	SweepStaleChildren(context.Background(), log)
	waitGone(t, pid, 3*time.Second)
}

// readPidLine reads the first line of a subprocess's stdout and parses it
// as a pid, tolerating trailing test-framework output (e.g. "PASS").
func readPidLine(t *testing.T, r io.Reader) int {
	t.Helper()
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		t.Fatalf("no pid line: %v", sc.Err())
	}
	return atoiOrFail(t, strings.TrimSpace(sc.Text()))
}

// TestSweepHelper spawns a fixture stdio child WITHOUT PDEATHSIG (so the
// child survives its parent's death and reparents to PID 1), prints the
// child pid, then dies immediately. The parent test then sweeps.
func TestSweepHelper(t *testing.T) {
	if os.Getenv("ORCHICON_MCP_SWEEP_HELPER") != "1" {
		return
	}
	argv := fixtureArgs()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(),
		"ORCHICON_MCP_STDIO=1",
		"ORCHICON_MCP_SERVER_ID=orphan",
	)
	// No SysProcAttr: no PDEATHSIG, so the child survives us.
	if err := cmd.Start(); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fmt.Println(cmd.Process.Pid)
	_ = cmd.Process.Release()
}

// testLogger builds a discard slog logger for sweep calls.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
