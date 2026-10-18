//go:build !windows

package main

// serve_reap_unix.go — the orphan sweep's reap-point PROBE, which is inherently Unix-shaped: it makes
// a real orphan by spawning `/bin/sh`, and it kills the probe's own sleeper with SIGKILL. None of that
// exists on Windows, and `syscall.Kill` is absent from the Windows syscall package — so this file's
// contents used to sit in serve.go UNTAGGED and broke the RELEASE BUILD for windows/amd64 and
// windows/arm64 rather than merely degrading at runtime on those hosts.
//
// THAT IS THE SAME BUG THIS REPOSITORY ALREADY FIXED ONCE. internal/mcpclient's sweep is split for
// exactly this reason, and its windows file says so in the same words — "the untagged file broke the
// RELEASE BUILD for windows/amd64 and windows/arm64". The lesson did not travel with the fix, so the
// pattern is worth stating where the code is: A FILE THAT SHELLS OUT TO /bin/sh OR CALLS syscall.Kill
// IS A UNIX FILE, and if it lives in an untagged file it is a Windows build failure waiting for the
// next release rather than a bug someone notices on a Windows host.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beardedparrott/orchicon/internal/opencode"
)

// discoverReapPoint finds where THIS environment hands orphans, by making one:
// a short-lived shell spawns a sleeper and exits, and we observe where the
// sleeper lands. Returns 0 when it cannot be established.
//
// WHY A PROBE RATHER THAN REASONING ABOUT THE PROCESS TREE. The reap point is
// the nearest ancestor carrying PR_SET_CHILD_SUBREAPER, and that attribute is
// not readable from outside the process that holds it. An earlier version of
// this walked our own ancestry and took the outermost non-init ancestor, on the
// theory that a login session's manager sits there. That theory is WRONG
// whenever anything sits ABOVE the real reaper — a container entrypoint, a
// nested subreaper — and it fails SILENTLY, because the walk still returns a
// plausible-looking pid; the sweep then matches nothing and reports success.
// (Demonstrated by planting a subreaper beneath an outer ancestor: the orphan
// landed on the planted reaper while the walk returned the outer process.)
// Making one real orphan and observing it is exact in any topology, and costs
// one subprocess per sweep.
func discoverReapPoint() int {
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 30 >/dev/null 2>&1 & echo $!; echo $$").Output()
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

// killOrphans kills the leftover opencode and orchicon mcp processes found by
// every plane boot's sweep. These accumulate when the server is killed before
// the ChatStream subprocess exits (e.g. during a forced binary replacement).
//
// WHAT COUNTS AS AN ORPHAN: a process that matches pgrep AND carries the
// PLANE SPAWN MARKER (carriesPlaneMarker) AND has been reparented away from the
// process that started it (reparentedAway). BOTH halves are required.
//
// The marker is the half that keeps the sweep honest. The point of this guard
// has always been to leave the operator's OWN opencode alone, and parentage
// alone cannot express that: on a host, an `opencode` started from a terminal
// that has since closed and a plane-spawned `opencode` left behind by a crash
// are reparented to the SAME place. Only the marker says which is which, so
// only a marked process is ever a candidate.
//
// The parentage half is what makes it an ORPHAN: a marked process whose parent
// is still alive is one we are still supervising (the `orchicon mcp` sidecars
// are children of a live opencode), and killing it would be a self-inflicted
// outage.
//
// The reap point is NOT always PID 1. Reparenting climbs to the nearest
// ancestor that set PR_SET_CHILD_SUBREAPER and stops there, falling back to
// PID 1 only when there is none. In a container nothing sets it, so orphans
// land on PID 1; in a HOST user session the session manager sets it, so a
// crashed plane's leftover opencode reparents to the session manager instead.
// A PID-1-only test would therefore reap in a container and silently collect
// nothing on a host — a sweep that reports success and leaves the processes
// running. See reapPoints.
//
// WHY THE PARENTAGE GUARD EXISTS: inside a container the pgrep could only ever
// see the plane's own children, so an unconditional SIGTERM sweep was harmless
// there. On a HOST-resident plane the same pgrep sees the WHOLE USER SESSION,
// and the harm is asymmetric — a sibling instance's plane is restarted by its
// serve watchdog, but the operator's own `opencode` (a legitimate, unrelated
// process) simply dies. The sweep therefore bounds itself to genuine orphans.
//
// Skipped in the runtime-container sandbox plane (ORCHICON_SANDBOX_PLANE=1):
// there the opencode serve is a LIVE child of the runtime supervisor, not
// an orphan of a crashed plane — pgrep would match and SIGTERM it (plus any
// live `orchicon mcp` sidecars serving worker sessions). The supervisor's
// own serve watchdog owns that process's lifecycle.
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

// orphanCandidates returns the PIDs pgrep matches for name. `-x` matches the
// process NAME exactly; a multi-word pattern such as "orchicon mcp" cannot be
// a process name, so it is matched against the whole command line (`-f`) —
// both patterns get the identical parentage treatment. A pgrep that finds
// nothing (or is unusable) yields no candidates.
func orphanCandidates(pgrep, name string) []int {
	args := []string{"-x"}
	if strings.Contains(name, " ") {
		args = []string{"-f"} // use -f for multi-word patterns
	}
	out, err := exec.Command(pgrep, append(args, name)...).Output()
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

// sweepOrphans SIGTERMs the candidates that are genuine orphans
// (reparentedAway) and returns how many it signalled. reap is this session's
// reap set, resolved once by the caller so every candidate is judged against
// the same answer.
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
// (opencode.PlaneSpawnEnv). It is read from the package that SETS it, so the
// sweeper and the spawner cannot drift apart.
const planeSpawnMarker = opencode.PlaneSpawnEnv

// carriesPlaneMarker reports whether pid's environment shows the plane spawn
// marker — i.e. whether an Orchicon plane started it, directly or through the
// opencode that inherited the marker and passed it on.
//
// /proc/<pid>/environ is readable only for processes of the same user, which is
// exactly the set this sweep may ever touch. An unreadable environ reports
// false, and false means the process is LEFT ALONE: a missed orphan is a leak,
// while a wrong SIGTERM kills the operator's own opencode. The guard fails in
// the direction that keeps processes alive, as it always has.
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

// reapProbeTimeout bounds the orphan probe below. Reparenting is immediate
// once the spawning shell exits; this only covers a pathologically slow
// scheduler.
const reapProbeTimeout = 2 * time.Second

// parentPID reads a process's parent straight from /proc. ok is false when the
// entry is unreadable — another user's process, a pid that has already exited,
// or a non-Linux host.
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

// reapPoints returns the parent PIDs that mean "whatever spawned this process is
// gone, and it has been handed to the reaper": PID 1, plus whatever
// discoverReapPoint observed.
//
// WHY NOT JUST PID 1. Reparenting does not necessarily end at PID 1 — it stops
// at the nearest ancestor carrying PR_SET_CHILD_SUBREAPER. A container has no
// such ancestor, so orphans land on PID 1. A host user session manager sets the
// attribute on itself, so on a HOST-resident plane every orphan of a crashed
// plane lands THERE. Testing only for PID 1 is therefore a container-shaped
// assumption: the sweep would look like it ran and quietly collect nothing on
// the host, which is where users are.
//
// This process is deliberately NEVER in the set: its own children are LIVE
// children, and reaping them would be the worst possible failure of this sweep.
func reapPoints() map[int]bool {
	pts := map[int]bool{1: true}
	if rp := discoverReapPoint(); rp > 1 && rp != os.Getpid() {
		pts[rp] = true
	}
	return pts
}

// reparentedAway reports whether pid's parent is one of the session's reap
// points (reap) — the signature of a process a crashed plane left behind,
// because the parent that spawned it is gone.
//
// WHY /proc RATHER THAN `pgrep -P <reaper>`: the parentage test is the whole
// safety property of this sweep, so it lives in Go where it is explicit and
// covered by a test that needs no reparented process (and where it applies
// identically to both patterns, whatever pgrep is installed). An unreadable
// /proc entry (another user's process, a non-Linux host) reports false: a
// missed orphan is a leak, a wrong SIGTERM is the operator's opencode dying —
// so the guard fails in the direction that keeps processes alive.
//
// A LIVE child of OURS is never an orphan, whatever the reap set says: it is
// still supervised by this very process. This is checked here, structurally,
// rather than relied on from the call site.
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
