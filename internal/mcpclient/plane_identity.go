package mcpclient

// plane_identity.go — WHO OWNS THIS CHILD, recorded in the child itself.
//
// THE REPORTED FAILURE. An `aseprite_mcp` process was found alive 18 HOURS after its plane died, with
// PGID=399808 (a plane that no longer existed), reparented to pid 3450. The boot sweep skipped it, because
// the sweep's only staleness test was "is ppid 1": the kernel had given it a new, perfectly live parent, so
// by that test the child looked owned.
//
// ppid CANNOT ANSWER THE QUESTION. Reparenting is exactly the case where ppid stops describing the owner —
// whether the child was adopted by init, by a session leader, or by whatever subreaper happened to be
// above it. The only party that knows who owns the child is the owner, at spawn time.
//
// So the owner stamps ITSELF into the child's environment, with an identity that cannot be recycled:
//
//	ORCHICON_MCP_PLANE_PID     the spawning process's pid
//	ORCHICON_MCP_PLANE_START   that process's start time (clock ticks since boot, from /proc/<pid>/stat)
//	ORCHICON_MCP_PLANE_BOOT    the machine's boot id
//
// PID ALONE WOULD BE WRONG and the start time is what makes it right: pids are reused, so "pid 3450 is
// alive" says nothing about whether it is the process that spawned this child. Requiring the START TIME to
// match turns a reused pid into a correct "owner is gone". The boot id covers the remaining case — a reboot
// inside a container or a host with no /proc continuity — where no pid comparison means anything.
//
// Everything here degrades: when /proc is not readable the markers are simply absent, and the sweep falls
// back to its old ppid rule rather than guessing.

import (
	"os"
	"strconv"
	"strings"
)

// Environment keys the sweep reads back. They are exported for the tests that pin the sweep's contract.
const (
	// mcpPlanePIDEnv names the process that spawned the child.
	mcpPlanePIDEnv = "ORCHICON_MCP_PLANE_PID"
	// mcpPlaneStartEnv is that process's start time, which is what makes a pid comparison meaningful.
	mcpPlaneStartEnv = "ORCHICON_MCP_PLANE_START"
	// mcpPlaneBootEnv is the machine's boot id, so a pid from a previous boot can never match.
	mcpPlaneBootEnv = "ORCHICON_MCP_PLANE_BOOT"
)

// planeIdentityEnv returns the env entries that identify THIS process as the child's owner.
func planeIdentityEnv() []string {
	pid := os.Getpid()
	return []string{
		mcpPlanePIDEnv + "=" + strconv.Itoa(pid),
		mcpPlaneStartEnv + "=" + strconv.FormatUint(procStartTime(pid), 10),
		mcpPlaneBootEnv + "=" + bootID(),
	}
}

// procStartTime returns the process's start time in clock ticks since boot — field 22 of /proc/<pid>/stat.
//
// THE FIELD COUNT IS THE TRAP, and it is why this parses from the LAST ')' rather than by splitting the
// whole line: field 2 (comm) is the executable name in parentheses and MAY CONTAIN SPACES AND PARENS, so a
// naive Fields() on the line shifts every subsequent index. After the last ')', field 3 (state) is index 0,
// which puts starttime at index 19.
//
// Returns 0 when it cannot be read, which the sweep treats as "no start time recorded" rather than as a
// match — a false negative leaks one child; a false positive kills a live one.
func procStartTime(pid int) uint64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(string(data[idx+1:]))
	const startTimeField = 19 // 0-based index of field 22 after the state field
	if len(fields) <= startTimeField {
		return 0
	}
	v, err := strconv.ParseUint(fields[startTimeField], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// bootID returns the kernel's boot identifier, or "" when it is unavailable (non-Linux, or an unreadable
// /proc).
func bootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// planeOwner identifies the process that spawned an MCP child, as recorded in the child's environment.
type planeOwner struct {
	pid       int
	startTime uint64
	boot      string
	// marked reports whether the child carried any owner marker at all. An unmarked child is from a build
	// that predates this, so the sweep must fall back to its older rule for it.
	marked bool
}

// ownerFromEnviron reads the owner markers out of a /proc/<pid>/environ blob.
func ownerFromEnviron(environ []byte) planeOwner {
	var o planeOwner
	for _, part := range strings.Split(string(environ), "\x00") {
		switch {
		case strings.HasPrefix(part, mcpPlanePIDEnv+"="):
			if pid, err := strconv.Atoi(strings.TrimPrefix(part, mcpPlanePIDEnv+"=")); err == nil {
				o.pid = pid
				o.marked = true
			}
		case strings.HasPrefix(part, mcpPlaneStartEnv+"="):
			if v, err := strconv.ParseUint(strings.TrimPrefix(part, mcpPlaneStartEnv+"="), 10, 64); err == nil {
				o.startTime = v
			}
		case strings.HasPrefix(part, mcpPlaneBootEnv+"="):
			o.boot = strings.TrimPrefix(part, mcpPlaneBootEnv+"=")
		}
	}
	return o
}

// alive reports whether the owning plane is still the process that spawned the child.
//
// THREE WAYS TO BE DEAD, and each exists for a measured reason:
//   - no recorded pid        → the owner cannot be identified, so it cannot be trusted (unmarked children
//     are handled by the caller's fallback, not here)
//   - a different boot       → the recorded pid belongs to a machine generation that is over
//   - a different start time → the pid exists but belongs to SOMEONE ELSE (pid reuse)
func (o planeOwner) alive(nowBoot string) bool {
	if !o.marked || o.pid <= 0 {
		return false
	}
	if o.boot != "" && nowBoot != "" && o.boot != nowBoot {
		return false
	}
	if o.startTime == 0 {
		// No start time recorded: fall back to bare liveness, which is what the old rule did.
		return processAlive(o.pid)
	}
	return procStartTime(o.pid) == o.startTime
}
