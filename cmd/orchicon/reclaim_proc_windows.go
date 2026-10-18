//go:build windows

package main

// reclaim_proc_windows.go — reclaim's process identity on Windows, where it CANNOT BE ESTABLISHED, so this is a
// documented degradation rather than an implementation.
//
// WHAT IS LOST, IN FULL (AC8). Reclaim may only terminate a process that is VERIFIABLY this instance's own
// leftover: our executable, this instance's name, and for a port holder the socket it holds. All three are
// /proc reads on Unix — /proc/<pid>/exe, /proc/<pid>/environ, /proc/net/tcp with /proc/<pid>/fd — and Windows
// has no /proc at all. So:
//
//   A STOPPED INSTANCE'S LEFTOVERS ARE REPORTED, NEVER RECLAIMED. `orchicon services --force`, `plane stop`
//   and the teardown will name the ports that are still held and print the holder if the OS will tell us, but
//   they will not terminate anything on this platform.
//
// THAT IS THE RIGHT DIRECTION, AND IT IS WHY THE ALTERNATIVE IS REFUSED. The tempting shortcut is "kill
// whatever holds the port": 5432 is a postgres port, and the holder may be the operator's own database, a
// colleague's instance, or an unrelated service. Killing on port number alone is how a convenience becomes
// data loss — which is the whole reason reclaim.go:18-28 draws the boundary in the first place. A wrong kill
// is unrecoverable; an unreclaimed leftover is an operator action.
//
// SO THE MESSAGES POINT SOMEWHERE REACHABLE. portHolderHint returns a PowerShell/Task Manager recipe instead
// of Unix tooling, because a refusal that tells a Windows user to run `ss -ltnp` is a refusal that wastes their
// next ten minutes. portHolders itself returns nothing — it cannot identify — and every caller already treats
// "no holder identified" as "report and refuse".

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

// portHolderHint names a Windows-reachable way to find a port's holder: PowerShell's Get-NetTCPConnection gives
// the owning PID, and Task Manager → Details shows what that PID is. Neither of the Unix tools named in the
// Unix half exists here.
func portHolderHint(port int) string {
	return fmt.Sprintf("PowerShell: `Get-NetTCPConnection -LocalPort %d -State Listen | "+
		"Select-Object -ExpandProperty OwningProcess`, then find that PID in Task Manager → Details", port)
}
