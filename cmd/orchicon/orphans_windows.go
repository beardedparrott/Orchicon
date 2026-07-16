//go:build windows
// +build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// killOrphanOrchiconProcs finds any running orchicon.exe processes and
// SIGKILLs them. The Unix implementation has the full rationale; the
// short version is that `orchicon dev stop` only knows about the PID
// recorded in the dev PID file, so any orchicon.exe that was started
// in some other way (orphans from a previous install, manual launch,
// etc.) survives — and that holds the binary file lock on Windows
// too, so the installer's `Move-Item` of the new binary fails with
// "file in use". This is the belt-and-suspenders cleanup.
//
// We exclude ourselves implicitly: `taskkill /F /IM orchicon.exe`
// matches by image name only, and this process is the orchicon.exe
// running `orchicon dev stop`. The recommended escape is to spawn a
// detached `taskkill` process via `cmd /C start /B taskkill` so it
// outlives us; otherwise `taskkill` would take us down too. We do
// exactly that below.
func killOrphanOrchiconProcs(exclude ...int) int {
	// First pass: count. taskkill exits non-zero if it found and killed
	// something (per its conventions); we don't actually care about
	// the exit code here — the second pass is the authoritative one.
	out, err := exec.Command("tasklist", "/NH", "/FI", "IMAGENAME eq orchicon.exe", "/FO", "CSV").Output()
	if err != nil {
		// tasklist returns "INFO: No tasks are running which match..." on
		// stderr when there are no matches; that's exit code 0 here (we
		// captured stdout). If we have any orchicon.exe listed, kill it.
		_ = out
	}
	// If tasklist printed anything other than the "no tasks" message,
	// there are orchicon.exe processes to kill. We launch taskkill via
	// cmd /C start /B so it runs detached and outlives us.
	if !strings.Contains(string(out), "orchicon.exe") {
		return 0
	}
	// Detach: `cmd /C start /B "" taskkill ...` runs taskkill in a new
	// background process tree rooted at the new cmd, not us. Without
	// the start /B the taskkill would race with our own exit.
	cmd := exec.Command("cmd", "/C", "start", "/B", "", "taskkill", "/F", "/IM", "orchicon.exe")
	_ = cmd.Start()
	// Don't Wait — we want the spawn to fire-and-forget. The new
	// taskkill will outlive us. Return approximate count.
	count := strings.Count(string(out), "orchicon.exe")
	fmt.Printf("  ✓ Signaled %d orphan orchicon.exe process(es) for termination\n", count)
	return count
}