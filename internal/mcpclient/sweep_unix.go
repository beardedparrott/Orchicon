//go:build !windows

package mcpclient

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// SweepStaleChildren reaps MCP stdio server processes whose OWNING PLANE no longer exists.
//
// THE RULE IS OWNERSHIP, NOT ppid — and that distinction is the reported bug. The sweep used to reap only
// children whose PPID was 1, on the theory that a dead plane's children get reparented to init. But the
// kernel reparents a child to whatever subreaper is above it, which on a modern system is usually a session
// leader with a perfectly live pid — so an orphan can look owned forever. Measured: an `aseprite_mcp`
// process alive 18 HOURS after its plane died, PGID from a plane that no longer existed, reparented to pid
// 3450, skipped by this sweep on every boot.
//
// So the owner stamps its own identity into the child at spawn (plane_identity.go), and this sweep asks
// THAT question: is the process recorded in the child's environment still the process that spawned it?
// A reused pid fails the check because the start time no longer matches; a reboot fails it because the
// boot id differs; and a reparented child no longer hides behind a live neighbour.
//
// A child with NO owner markers (spawned by a build that predates this) still falls back to the old
// ppid==1 rule, so an upgrade does not strand the children the previous build leaked.
//
// Children whose owning plane is alive are untouched: they belong to a live plane (or an in-progress
// session).
//
// Only works on Linux/BSD where /proc and process groups exist — and the build tag above says
// so, which it did NOT before. The runtime guard below (`procAvailable`) has always been here,
// and for a while it was the ONLY guard: the file carried no build tag, so it compiled
// everywhere and this package failed to build for Windows on `syscall.Kill` / `syscall.ESRCH`,
// which do not exist there. That is not a runtime-degradation issue — it is a compile error in
// the release matrix, so a runtime check can never cover it. Callers hook this into the
// boot/periodic adopt sweep of Server.Run (ADR-0008: stdio children cannot outlive a dead
// control plane); see sweep_windows.go for the no-op the other platforms get.
func SweepStaleChildren(ctx context.Context, log *slog.Logger) {
	if !procAvailable() {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Debug("mcp sweep: cannot read /proc", "err", err)
		return
	}
	// Read ONCE: every child is judged against the same boot generation, and a reboot mid-sweep must not
	// produce a half-converted set of verdicts.
	nowBoot := bootID()
	swept := 0
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue // gone or not readable (different uid) — skip
		}
		if !envHasMarker(data) {
			continue
		}
		owner := ownerFromEnviron(data)
		var reason string
		if owner.marked {
			if owner.alive(nowBoot) {
				continue // its plane is alive — not ours to touch
			}
			reason = "owning plane is gone"
		} else {
			// UNMARKED: a child from a build that predates the owner markers. Fall back to the rule this
			// sweep shipped with, so an upgrade reclaims what the old build leaked rather than ignoring it.
			stat, serr := os.ReadFile("/proc/" + e.Name() + "/stat")
			if serr != nil {
				continue
			}
			ppid, ok := parsePPID(stat)
			if !ok || ppid != 1 {
				continue // adopted by something live; identity unknown, so leave it
			}
			reason = "legacy child reparented to init"
		}
		// Its controlling execution no longer exists. Kill the process GROUP *and* the process.
		//
		// BOTH, AND NOT BECAUSE OF REDUNDANCY. Children spawned by newStdioTransport are Setpgid'd, so
		// `-pid` names their group — but that is not every child this sweep meets. One spawned by an older
		// build, or by any path that did not set the group, is not a group leader: `kill(-pid)` then fails
		// with ESRCH, which the old code treated as success. The result was a sweep that logged
		// "reaped stale MCP child" and killed NOTHING, so the leak it reported fixing survived it.
		// Sending the signal to the pid as well makes the reap true either way; when the group kill worked
		// the second call is a harmless ESRCH.
		groupErr := syscall.Kill(-pid, syscall.SIGKILL)
		pidErr := syscall.Kill(pid, syscall.SIGKILL)
		if notGone(groupErr) && notGone(pidErr) {
			log.Debug("mcp sweep: kill stale child", "pid", pid, "group_err", groupErr, "pid_err", pidErr)
			continue
		}
		swept++
		log.Info("mcp sweep: reaped stale MCP child",
			"pid", pid, "server_id", envServerID(data), "reason", reason,
			"owner_pid", owner.pid)
	}
	if swept > 0 {
		log.Info("mcp sweep: reaped stale MCP children", "count", swept)
	}
}

// envHasMarker reports whether an environ blob contains the stdio marker.
func envHasMarker(environ []byte) bool {
	return indexOf(environ, []byte("ORCHICON_MCP_STDIO=1")) >= 0
}

// envServerID extracts the server id marker from an environ blob, if any.
func envServerID(environ []byte) string {
	for _, part := range strings.Split(string(environ), "\x00") {
		if strings.HasPrefix(part, "ORCHICON_MCP_SERVER_ID=") {
			return strings.TrimPrefix(part, "ORCHICON_MCP_SERVER_ID=")
		}
	}
	return ""
}

// parsePPID extracts the parent pid (4th field) from /proc/<pid>/stat.
// Format: pid (comm) state ppid ... — comm may contain spaces and
// parens, so find the LAST ')' and parse after it.
func parsePPID(stat []byte) (int, bool) {
	idx := lastIndexByte(stat, ')')
	if idx < 0 {
		return 0, false
	}
	fields := strings.Fields(string(stat[idx+1:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

func indexOf(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

var procAvailable = func() bool {
	_, err := os.Stat("/proc")
	return err == nil
}

// processAlive reports whether pid names a live process we may signal.
//
// Signal 0 performs the permission/existence check without delivering anything — the same probe
// `kill -0` makes. EPERM means the process EXISTS but belongs to another user, which is still "alive"
// for the purpose of "do not reap it": the sweep must never kill a child it cannot identify.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// notGone reports whether a kill attempt genuinely failed to reach anything. ESRCH ("no such process")
// is the SUCCESS case for a kill — it means the target is already gone — so treating it as a failure, or
// as proof the kill landed, are both wrong; it is simply "nothing left to do".
func notGone(err error) bool {
	return err != nil && err != syscall.ESRCH
}
