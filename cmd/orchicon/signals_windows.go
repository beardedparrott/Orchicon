//go:build windows

package main

// signals_windows.go — the Windows half of the signal seam, and THE DIFFERENCE FROM SIGTERM/SIGKILL IS THE
// POINT OF THIS FILE (the supervisor's shutdown difference, stated rather than discovered).
//
// WHAT UNIX DOES: shutdownSignals() is SIGTERM+SIGINT, a graceful stop is SIGTERM, and a force stop is
// SIGKILL. Every child of the supervisor gets SIGTERM, is given `shutdownTimeout` (20s) to exit, and is then
// SIGKILLed. `serve --stop` is the same ladder against the detached plane's PID.
//
// WHAT WINDOWS HAS INSTEAD, and there is no SIGTERM to emulate:
//
//   - shutdownSignals() is {os.Interrupt}. That is the only signal the Go runtime delivers here (Ctrl-C,
//     Ctrl-Break or a console close map onto it); SIGTERM compiles and is never delivered.
//   - A graceful stop is GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, pid). IT ONLY REACHES A PROCESS GROUP
//     ATTACHED TO THIS CONSOLE — the detached plane is deliberately in its own console
//     (DETACHED_PROCESS, procattr_windows.go), so in practice this call usually FAILS, and the stop then
//     escalates straight to force. That is why gracefulStopFailureIsFatal is false here: a stop that cannot
//     stop is not a success, so a failed Ctrl-Break is not the end of the attempt.
//   - A force stop is TerminateProcess, i.e. SIGKILL semantics: NO shutdown hook runs, so a force-stopped
//     plane never flushes, never stops its supervisor children, and its own children are not reaped (the
//     sweep is inert here — serve_reap_windows.go).
//
// SO THE ESCALATION LADDER IS PRESERVED AND ITS OUTCOME DIFFERS, which is what the operator is told: the
// success line says the plane was FORCE-stopped and never ran its shutdown path, rather than "it ignored
// SIGTERM", which would imply it received one.
//
// THE TIMING COST OF KEEPING THE LADDER IDENTICAL, stated because it is the part an operator notices. On Unix
// a cooperative child exits on SIGTERM at once, so the supervisor's 20 s `shutdownTimeout` is a CEILING that is
// rarely reached. Here the graceful attempt routinely cannot be delivered, so nothing child-side acts on it and
// the wait is spent in full: a Windows supervisor shutdown reads "children did not exit within the grace period"
// and takes ~20 s — one shared deadline across ALL children, not 20 s each — before TerminateProcess ends them.
// That is bounded and it does end in a clean stop; it is simply slower than the Unix path. Shortening the grace
// for the undeliverable case was considered and rejected as the more surprising behaviour: a child that stops
// itself a few seconds after its console closes would then be killed mid-exit.
//
// DECISION (revisitable): if CTRL_BREAK_EVENT proves deliverable in practice — a plane started
// foreground/attached to the caller's console — this stays as-is and the ladder degrades gracefully. The
// alternative (an RPC stop path) is explicitly out of scope: package planecontrol's doc forbids it, because
// a lifecycle verb that needs the plane to be healthy cannot stop an unhealthy plane.

import (
	"fmt"
	"os"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/planecontrol"
)

// ctrlBreakEvent is CTRL_BREAK_EVENT. stdlib syscall does have it (syscall.CTRL_BREAK_EVENT), but
// GenerateConsoleCtrlEvent itself is NOT in GOROOT's syscall package, so it is reached through the DLL.
const ctrlBreakEvent = uint32(syscall.CTRL_BREAK_EVENT)

var procGenerateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// shutdownSignals are the signals that mean "stop this process". Windows delivers os.Interrupt and nothing
// else — see the file header.
func shutdownSignals() []os.Signal { return []os.Signal{os.Interrupt} }

// sendGracefulStop asks a process to run its own shutdown path, which on Windows means a Ctrl-Break to its
// process group. It only reaches a group attached to this console, so it routinely fails and the caller
// escalates (see gracefulStopFailureIsFatal).
func sendGracefulStop(proc *os.Process) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	if !pidAlive(proc.Pid) {
		return os.ErrProcessDone
	}
	r, _, err := procGenerateConsoleCtrlEvent.Call(uintptr(ctrlBreakEvent), uintptr(uint32(proc.Pid)))
	if r == 0 {
		if err == nil || err == syscall.Errno(0) {
			err = fmt.Errorf("GenerateConsoleCtrlEvent failed")
		}
		return fmt.Errorf("deliver Ctrl-Break to process group of %d: %w", proc.Pid, err)
	}
	return nil
}

// sendForceStop ends a process without letting it run its shutdown path — SIGKILL semantics, no hook runs.
func sendForceStop(proc *os.Process) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	return planecontrol.TerminateProcess(proc.Pid)
}

// gracefulStopFailureIsFatal is FALSE here: Windows usually cannot deliver a graceful stop to a detached
// plane, so a failed Ctrl-Break must escalate to force rather than fail the command.
const gracefulStopFailureIsFatal = false

// gracefulStopLabel names the graceful-stop mechanism for logs that must not claim SIGTERM here.
func gracefulStopLabel() string { return "CTRL_BREAK (Windows has no SIGTERM)" }

// gracefulStopUnavailable is the message for a graceful stop that could not be delivered.
func gracefulStopUnavailable(pid int, err error) string {
	return fmt.Sprintf("… Windows has no SIGTERM: %v — forcing PID %d instead", err, pid)
}

// forceStopNotice explains that the stop is escalating. On Windows there is nothing to escalate FROM, so it
// says that instead of implying a SIGTERM went unanswered.
func forceStopNotice(pid int, grace string) string {
	return fmt.Sprintf("… PID %d did not exit within %s — escalating to a force stop", pid, grace)
}

// forceStopResult is the success line after a force stop. It must not claim the plane ran a shutdown path.
func forceStopResult(pid int) string {
	return fmt.Sprintf("✓ serve force-stopped (PID %d) — Windows has no SIGTERM, so the plane never ran its "+
		"shutdown path; its supervisor children may be left running", pid)
}
