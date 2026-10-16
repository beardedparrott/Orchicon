//go:build !windows

package main

// hostproc_unix.go — the host plane's PROCESS IDENTITY probe. It is Unix-shaped by construction: it
// reads `/proc/<pid>/exe` to learn what a running plane is actually executing, and it asks
// `syscall.Kill(pid, 0)` whether a PID from a stale PID file is still a live process. Neither exists
// on Windows, and `syscall.Kill` is ABSENT from the Windows syscall package — so these three functions
// sitting in the untagged install_host.go broke the BUILD for windows/amd64 and windows/arm64 rather
// than degrading at runtime.
//
// THAT IS THE SAME BUG THIS REPOSITORY ALREADY FIXED ONCE, and serve_reap_unix.go states the lesson in
// as many words: A FILE THAT SHELLS OUT TO /bin/sh OR CALLS syscall.Kill IS A UNIX FILE, and if it
// lives in an untagged file it is a Windows build failure waiting for the next release. The warning did
// not travel with the code, so the platform pair is re-stated here. hostproc_windows.go is the other
// half, and the pair is what makes "Windows degrades deliberately" true in the compiler rather than in a
// comment.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// planeDrift identifies the serving plane from the instance's PID file and compares its executable with
// this one.
//
// WHY THE PID FILE AND /proc: they are the only truthful source. A version endpoint would answer with
// whatever the OLD process believes about itself, and a timestamp comparison cannot see a rebuilt binary of
// the same version. `/proc/<pid>/exe` shows the APEX of the running process, and a replaced path reports
// `(deleted)` — the exact fingerprint of "the installer overwrote the binary under a live plane".
func planeDrift(instance string, out io.Writer) planeDriftState {
	self, err := os.Executable()
	if err != nil {
		return planeUnknown
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return planeUnknown
	}
	pid, ok := servingPlanePID(instance)
	if !ok {
		return planeUnknown
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return planeUnknown
	}
	// A path is `(deleted)`-suffixed once the file it names has been replaced. Anything else is compared
	// against the binary we are running from.
	if strings.HasSuffix(exe, " (deleted)") {
		return planeDrifted
	}
	if exe != self {
		// Serving from a DIFFERENT path than the binary this install just placed in the install dir. It may
		// be a dev checkout serving deliberately; only a plane the INSTALLER started is ours to restart, and
		// the PID file is what distinguishes them.
		fmt.Fprintf(out, "  (plane runs from %s, not %s — leaving it)\n", exe, self)
		return planeUnknown
	}
	return planeCurrent
}

// servingPlanePID reads the instance's detached-plane PID file and reports whether that process is alive.
func servingPlanePID(instance string) (int, bool) {
	dataDir, err := hostDataDir(instance)
	if err != nil {
		return 0, false
	}
	stateDir := filepath.Join(dataDir, "serve")
	b, err := os.ReadFile(filepath.Join(stateDir, "pids", "orchicon.pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	// The PID file outlives the process; /proc is what says whether it is still that process.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return 0, false
	}
	return pid, true
}

// servingPlaneAlive reports whether the instance's PID-file process is still running.
func servingPlaneAlive(instance string) bool {
	pid, ok := servingPlanePID(instance)
	if !ok {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
