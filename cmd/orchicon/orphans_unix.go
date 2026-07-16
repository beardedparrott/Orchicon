//go:build !windows
// +build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// killOrphanOrchiconProcs finds any running `orchicon` processes that
// are NOT us and NOT the PID we already stopped, and SIGKILLs them.
//
// Why: `orchicon dev stop` only knows about the process whose PID is
// recorded in the dev PID file. If the user started the control plane
// in another way (e.g. ran `orchicon` directly, or a previous install
// left a stray process whose PID file was clobbered by a newer
// `orchicon dev start`), that process is an "orphan" — it stays alive,
// holds the binary file lock (so `mv`/`cp` of a new binary returns
// "Text file busy"), and continues to listen on :8080. The
// orchicon installer's `--clean` / `--force-clean` flags would then
// fail with a confusing error.
//
// We use `pgrep -x orchicon` (exact executable-name match, not a
// substring of the full command line — important because a worker
// running `opencode run ...` shouldn't match). We then filter out our
// own PID (this `orchicon dev stop` invocation is itself an orchicon
// process) and any PID we already handled above, and SIGKILL the rest.
//
// Returns the count of processes killed (0 if pgrep is unavailable or
// no orphans were found). The SIGKILL is intentional: by the time we
// get here the user has already asked for a stop, the PID-file process
// has had its 15s grace period, and leftover orphans are almost always
// in a state where a SIGTERM would just bounce (their own context is
// gone, they're stuck on a syscall, etc.). SIGKILL is the only signal
// that guarantees the binary file lock is released.
func killOrphanOrchiconProcs(exclude ...int) int {
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		// pgrep missing (rare on Linux; never on macOS by default).
		// Fall back to scanning /proc directly so we still find
		// orphans on minimal containers.
		return killOrphanOrchiconProcsProcFS(exclude...)
	}

	out, err := exec.Command(pgrep, "-x", "orchicon").Output()
	if err != nil {
		// pgrep returns exit 1 when no processes match. That's the
		// happy path — no orphans.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return 0
		}
		return 0
	}

	excludeSet := make(map[int]bool, len(exclude)+1)
	excludeSet[os.Getpid()] = true // never kill ourselves
	for _, pid := range exclude {
		excludeSet[pid] = true
	}

	killed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || excludeSet[pid] {
			continue
		}
		// SIGKILL directly. /proc/<pid>/exe is the actual file the
		// kernel has mmap'd — releasing it is what frees the binary
		// for replacement. pkill -9 here is the documented
		// installer-time escape valve (the user's own request).
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
			killed++
		}
	}
	if killed > 0 {
		fmt.Printf("  ✓ Killed %d orphan orchicon process(es)\n", killed)
	}
	return killed
}

// killOrphanOrchiconProcsProcFS is the fallback when pgrep isn't on
// PATH (small Alpine / distroless containers). It walks /proc,
// resolves each PID's exe link, and matches on the basename "orchicon".
func killOrphanOrchiconProcsProcFS(exclude ...int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	excludeSet := make(map[int]bool, len(exclude)+1)
	excludeSet[os.Getpid()] = true
	for _, pid := range exclude {
		excludeSet[pid] = true
	}
	killed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || excludeSet[pid] {
			continue
		}
		target, err := os.Readlink(fmt.Sprintf("/proc/%s/exe", e.Name()))
		if err != nil {
			// /proc/<pid>/exe vanishes the instant the process
			// exits; ESRCH means "already gone", which is fine.
			continue
		}
		base := target
		if i := strings.LastIndex(target, "/"); i >= 0 {
			base = target[i+1:]
		}
		if base != "orchicon" {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
			killed++
		}
	}
	if killed > 0 {
		fmt.Printf("  ✓ Killed %d orphan orchicon process(es) (via /proc)\n", killed)
	}
	return killed
}