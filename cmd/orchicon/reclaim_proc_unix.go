//go:build !windows

package main

// reclaim_proc_unix.go — reclaim's PROCESS IDENTITY, which is /proc-shaped on every Unix.
//
// WHAT IT IS FOR. Reclaim may only terminate a process that is VERIFIABLY this instance's own leftover: our
// binary AND this instance's name AND (for a port holder) the socket it holds. Every one of those three
// readings is a /proc read — /proc/<pid>/exe, /proc/<pid>/environ, /proc/net/tcp plus /proc/<pid>/fd. That is
// why the whole layer is behind this build tag rather than in reclaim.go, where it used to sit UNTAGGED and
// silently identified nothing on Windows.
//
// portHolderHint is the platform-reachable way to find a port's holder, so the refusal messages point at a
// command that exists. reclaim_proc_windows.go is the other half and states what Windows loses (AC8).

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ourInstanceProcesses lists pids that are VERIFIABLY this instance's: the executable is one of our binaries
// AND the process carries the instance's name in its environment or argv.
//
// Both conditions matter. Our binary alone would match a DIFFERENT instance's plane — killing it would take
// down a sibling (dev while you meant test); the instance name alone would match nothing useful.
func ourInstanceProcesses(instance string) []int {
	self := os.Getpid()
	var pids []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	tag := "ORCHICON_INSTANCE=" + instance
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		exe, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil || !isOurBinary(exe) {
			continue
		}
		environ, _ := os.ReadFile(filepath.Join(dir, "environ"))
		args, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if strings.Contains(string(environ), tag) || strings.Contains(string(args), instance) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// portHolders names the processes listening on the given ports, so a refusal can say WHO instead of telling
// the operator to go run `ss` themselves. It reads /proc directly rather than shelling out to ss/lsof:
// neither is guaranteed present, and the failure path of a teardown must not depend on an optional tool.
//
// BEST-EFFORT AND HONEST ABOUT IT: an unreadable /proc (another user's process, a hardened kernel) yields
// nothing rather than a guess, and the caller's message still stands on its own.
func portHolders(ports ...int) []portHolder {
	want := map[string]bool{}
	for _, p := range ports {
		if p > 0 {
			// /proc/net/tcp lists LOCAL_ADDRESS:PORT as 4 hex digits of the port, upper-case.
			want[fmt.Sprintf(":%04X", p)] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || !want[fields[1][strings.LastIndex(fields[1], ":")+1:]] {
				continue
			}
			if fields[3] != "0A" { // 0A = TCP_LISTEN
				continue
			}
			inodes["socket:["+fields[9]+"]"] = true
		}
	}
	if len(inodes) == 0 {
		return nil
	}
	var out []portHolder
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !inodes[target] {
				continue
			}
			exe, _ := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
			if exe == "" {
				exe = "unknown (not readable by this user)"
			}
			out = append(out, portHolder{pid: pid, exe: exe})
			break
		}
	}
	return out
}

// processBelongsTo confirms a pid carries this instance's name, so our binary serving a SIBLING instance is
// never mistaken for ours.
func processBelongsTo(pid int, instance string) bool {
	dir := filepath.Join("/proc", strconv.Itoa(pid))
	environ, _ := os.ReadFile(filepath.Join(dir, "environ"))
	return strings.Contains(string(environ), "ORCHICON_INSTANCE="+instance+"\x00") ||
		strings.HasSuffix(string(environ), "ORCHICON_INSTANCE="+instance)
}

// portHolderHint names a command that finds who holds a port on this platform, for messages that must point at
// something the operator can actually run.
func portHolderHint(port int) string {
	return fmt.Sprintf("`ss -ltnp` / `lsof -i :%d`", port)
}
