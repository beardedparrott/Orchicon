//go:build windows

package main

// IT ONLY REACHES A PROCESS GROUP ATTACHED TO THIS CONSOLE.

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

// sendGracefulStop asks a process to run its own shutdown path, which on Windows means a Ctrl-Break
// to its process group.
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
