package mcpclient

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// SweepStaleChildren reaps MCP stdio server processes whose controlling
// execution no longer exists. It scans /proc/*/environ for children
// launched with the ORCHICON_MCP_STDIO=1 marker and, for any whose parent
// (PPID) is 1 (i.e. the control plane that spawned them has died and the
// kernel reparented them — PDEATHSIG normally prevents this, this is the
// belt-and-suspenders sweep for children that survived by other means)
// kills the whole process group.
//
// Children whose parent is still alive are untouched: they belong to a
// live plane (or a session in progress). No disk registry is needed — the
// environment marker is written by newStdioTransport on every launch.
//
// Only works on Linux/BSD where /proc and process groups exist; on other
// platforms it is a no-op. Callers hook this into the boot/periodic adopt
// sweep of Server.Run (ADR-0008: stdio children cannot outlive a dead
// control plane).
func SweepStaleChildren(ctx context.Context, log *slog.Logger) {
	if !procAvailable() {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Debug("mcp sweep: cannot read /proc", "err", err)
		return
	}
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
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		ppid, ok := parsePPID(stat)
		if !ok || ppid != 1 {
			continue // parent alive — live plane or running session
		}
		// Reaped child: its controlling execution no longer exists. Kill
		// the whole process group (children were Setpgid'd at launch).
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			log.Debug("mcp sweep: kill stale child", "pid", pid, "err", err)
			continue
		}
		swept++
		log.Info("mcp sweep: reaped stale MCP child", "pid", pid, "server_id", envServerID(data))
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
