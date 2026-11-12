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

// The runtime guard below (`procAvailable`) has always been here, and for a while it was the ONLY
// guard: the file carried no build tag.
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
		// Its controlling execution no longer exists.
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
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// notGone reports whether a kill attempt genuinely failed to reach anything.
func notGone(err error) bool {
	return err != nil && err != syscall.ESRCH
}
