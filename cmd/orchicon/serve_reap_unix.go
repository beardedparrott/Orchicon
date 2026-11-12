//go:build !windows

package main

// serve_reap_unix.go — the orphan sweep's reap-point PROBE, which is inherently Unix-shaped: it
// makes a real orphan by spawning `/bin/sh`, and it kills the probe's own sleeper with SIGKILL.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// discoverReapPoint finds where THIS environment hands orphans, by making one: a short-lived shell
// spawns a sleeper and exits, and we observe where the sleeper lands.
func discoverReapPoint() int {
	out, err := procwin.Command("/bin/sh", "-c", "/bin/sleep 30 >/dev/null 2>&1 & echo $!; echo $$").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return 0
	}
	spawner, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	// The probe's own sleeper is ours to clean up, whatever we learn.
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	deadline := time.Now().Add(reapProbeTimeout)
	for time.Now().Before(deadline) {
		if pp, ok := parentPID(pid); ok && pp != spawner {
			return pp
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0
}

// ---------------------------------------------------------------------------------------------------
// THE ORPHAN SWEEP ITSELF — moved here from serve.go, unchanged, because it is a UNIX file.
//
// It was untagged in serve.go and every line of it reads /proc (environ, status) or sends SIGTERM. That
// compiled fine on Windows, where it simply did nothing — the sweep matched nothing, reported success, and
// left a crashed plane's opencode and `orchicon mcp` children running with no message. Untagged POSIX code
// that fails silently is worse than a tagged file that fails to build, which is why the whole sweep now
// lives behind this build tag and serve_reap_windows.go states what Windows loses.
//
// NOTHING HERE IS REWRITTEN, INCLUDING THE COMMENTS: the reasoning below (marker + parentage, the reap
// point probe, why a missed orphan is preferred to a wrong kill) is the safety property of the sweep.
// ---------------------------------------------------------------------------------------------------

// killOrphans kills the leftover opencode and orchicon mcp processes found by every plane boot's
// sweep.
func killOrphans() {
	if os.Getenv("ORCHICON_SANDBOX_PLANE") != "" {
		return
	}
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return
	}
	// Resolve the reap set ONCE per sweep: it is an environment property, and
	// every candidate must be judged against the same answer.
	reap := reapPoints()
	for _, name := range []string{"opencode", "orchicon mcp"} {
		sweepOrphans(orphanCandidates(pgrep, name), reap)
	}
}

// orphanCandidates returns the PIDs pgrep matches for name.
func orphanCandidates(pgrep, name string) []int {
	args := []string{"-x"}
	if strings.Contains(name, " ") {
		args = []string{"-f"} // use -f for multi-word patterns
	}
	out, err := procwin.Command(pgrep, append(args, name)...).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// sweepOrphans SIGTERMs the candidates that are genuine orphans (reparentedAway) and returns how
// many it signalled.
func sweepOrphans(pids []int, reap map[int]bool) int {
	signalled := 0
	for _, pid := range pids {
		if !carriesPlaneMarker(pid) || !reparentedAway(pid, reap) {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Signal(syscall.SIGTERM)
			signalled++
		}
	}
	return signalled
}

// planeSpawnMarker is the variable the plane stamps on every process it starts
// (opencode.PlaneSpawnEnv).
const planeSpawnMarker = opencode.PlaneSpawnEnv

// carriesPlaneMarker reports whether pid's environment shows the plane spawn marker — i.e. whether
// an Orchicon plane started it.
func carriesPlaneMarker(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return false
	}
	for _, kv := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(kv, planeSpawnMarker+"=") {
			return true
		}
	}
	return false
}

// reapProbeTimeout bounds the orphan probe below.
const reapProbeTimeout = 2 * time.Second

// parentPID reads a process's parent straight from /proc.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// This process is deliberately NEVER in the set: its own children are LIVE children, and reaping
// them would be the worst possible failure of this sweep.
func reapPoints() map[int]bool {
	pts := map[int]bool{1: true}
	if rp := discoverReapPoint(); rp > 1 && rp != os.Getpid() {
		pts[rp] = true
	}
	return pts
}

// reparentedAway reports whether pid's parent is one of the session's reap points (reap) — the
// signature of a process a crashed plane left behind, because the parent that spawned it is gone.
func reparentedAway(pid int, reap map[int]bool) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	pp, ok := parentPID(pid)
	if !ok || pp == os.Getpid() {
		return false
	}
	return reap[pp]
}
