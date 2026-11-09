//go:build windows

package main

// So: A STOPPED INSTANCE'S LEFTOVERS ARE REPORTED, NEVER RECLAIMED.

import "fmt"

// ourInstanceProcesses identifies nothing on Windows: process identity needs /proc/<pid>/exe and
// /proc/<pid>/environ. See the file header — the leftovers are reported, never terminated.
func ourInstanceProcesses(instance string) []int { return nil }

// portHolders identifies nothing on Windows: it would need /proc/net/tcp and /proc/<pid>/fd. The caller's
// message then falls back to portHolderHint, which names a tool that exists here.
func portHolders(ports ...int) []portHolder { return nil }

// processBelongsTo cannot confirm ownership on Windows (no /proc/<pid>/environ), so it answers false — the
// answer that never claims a process is ours.
func processBelongsTo(pid int, instance string) bool { return false }

// portHolderHint names a Windows-reachable way to find a port's holder: PowerShell's
// Get-NetTCPConnection gives the owning PID, and Task Manager → Details shows what that PID is.
func portHolderHint(port int) string {
	return fmt.Sprintf("PowerShell: `Get-NetTCPConnection -LocalPort %d -State Listen | "+
		"Select-Object -ExpandProperty OwningProcess`, then find that PID in Task Manager → Details", port)
}
