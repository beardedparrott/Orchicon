//go:build windows

package planecontrol

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
//   - `drift` returning `Unknown` makes Start print "could not identify the process; leaving it" and
//     RETURN WITHOUT RESTARTING. That is the right answer for a plane whose identity we cannot establish:
//     an operator tool that restarts a service it cannot justify killing is worse than one that leaves a
//     working instance alone. (planecontrol.go, the `default:` arm.)
//   - `ServingPlaneAlive` returning `false` makes Stop's bounded wait loop exit immediately rather than
//     sleeping 5s for a process it can never confirm is gone.
//
// So the Windows answer is "never guess", which is the same direction these guards fail in everywhere
// else. A Windows liveness probe that is WRONG in the other direction — reporting a stale PID as live,
// or a live plane as drifted and restarting it — is the failure mode this file exists to avoid.

import (
	"fmt"
	"io"
	"os"
)

// procExe cannot read /proc on Windows, so the binary behind a PID is never claimed.
func procExe(pid int) (string, error) { return "", os.ErrNotExist }

// drift cannot identify a serving process on Windows (no /proc/<pid>/exe), so it reports Unknown and the
// caller leaves the running plane alone.
func Drift(instance string, out io.Writer) DriftState { return Unknown }

// ServingPlanePID cannot read /proc on Windows, so it never reports a live PID.
func ServingPlanePID(instance string) (int, bool) { return 0, false }

// ServingPlaneAlive cannot ask syscall.Kill on Windows, so it never claims a PID-file process is alive.
func ServingPlaneAlive(instance string) bool { return false }

// TerminateProcess asks a host process to stop. WINDOWS HAS NO SIGTERM TO SEND, and this file's doctrine is
// that a probe which cannot tell the truth says so rather than guessing — so instead of pretending a
// graceful stop happened (or hard-killing a supervisor whose postgres children would be orphaned), it
// reports a clear error and lets the caller print "could not stop" and carry on.
func TerminateProcess(pid int) error {
	return fmt.Errorf("cannot signal host process %d on Windows (no SIGTERM) — stop the instance from Task Manager, then re-run `orchicon install`", pid)
}
