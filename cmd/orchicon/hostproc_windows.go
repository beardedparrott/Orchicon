//go:build windows

package main

// hostproc_windows.go — the Windows half of the host plane's process-identity probe, and it degrades
// DELIBERATELY rather than accidentally.
//
// WHY THESE ARE NOT IMPLEMENTED: the Unix versions read `/proc/<pid>/exe` and `/proc/<pid>`, and ask
// `syscall.Kill(pid, 0)` about a PID from a stale PID file. Windows has no `/proc`, and `syscall.Kill`
// is absent from its syscall package — which is why the untagged versions broke the BUILD rather than
// merely misbehaving.
//
// WHAT THE DEGRADATION BUYS IS THE POINT. Both callers already have a correct "I cannot tell" branch:
//
//   - `planeDrift` returning `planeUnknown` makes startHostPlane print "could not identify the process;
//     leaving it" and RETURN WITHOUT RESTARTING. That is the right answer for a plane whose identity we
//     cannot establish: an installer that restarts a service it cannot justify killing is worse than one
//     that leaves a working instance alone. (install_host.go, the `default:` arm.)
//   - `servingPlaneAlive` returning `false` makes stopHostPlane's bounded wait loop exit immediately
//     rather than sleeping 5s for a process it can never confirm is gone.
//
// So the Windows answer is "never guess", which is the same direction these guards fail in everywhere
// else. A Windows liveness probe that is WRONG in the other direction — reporting a stale PID as live,
// or a live plane as drifted and restarting it — is the failure mode this file exists to avoid.

import "io"

// planeDrift cannot identify a serving process on Windows (no /proc/<pid>/exe), so it reports
// planeUnknown and the caller leaves the running plane alone.
func planeDrift(instance string, out io.Writer) planeDriftState { return planeUnknown }

// servingPlanePID cannot read /proc on Windows, so it never reports a live PID.
func servingPlanePID(instance string) (int, bool) { return 0, false }

// servingPlaneAlive cannot ask syscall.Kill on Windows, so it never claims a PID-file process is alive.
func servingPlaneAlive(instance string) bool { return false }
