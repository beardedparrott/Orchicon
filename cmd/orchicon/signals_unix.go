//go:build !windows

package main

// signals_unix.go — the Unix half of THE SIGNAL SEAM. cmd/orchicon used to spell SIGTERM/SIGKILL out at
// every site that stops a process (serve.go's stop ladder, container.go's supervisor shutdown, the two
// signal.NotifyContext calls). Those compile on Windows — syscall.SIGTERM is just a number there — and then
// DELIVER NOTHING: Go's Windows runtime supports os.Interrupt only, so a detached plane could never be
// stopped and no supervisor child ever received a graceful stop. Silent, which is the worst kind.
//
// So the signal names live here, behind the build tag, and signals_windows.go says what the same three
// verbs mean on a platform with no SIGTERM. The Unix bodies are byte-for-byte what the call sites did.

import (
	"fmt"
	"os"
	"syscall"
)

// shutdownSignals are the signals that mean "stop this process": SIGTERM from a supervisor or a service
// manager, SIGINT from a terminal.
func shutdownSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGINT} }

// sendGracefulStop asks a process to run its own shutdown path.
func sendGracefulStop(proc *os.Process) error { return proc.Signal(syscall.SIGTERM) }

// sendForceStop ends a process without letting it run its shutdown path. SIGKILL semantics.
func sendForceStop(proc *os.Process) error { return proc.Signal(syscall.SIGKILL) }

// gracefulStopFailureIsFatal reports whether a failed graceful stop is the END of the stop attempt. On Unix
// it is: an EPERM from SIGTERM means we may not signal this process at all, and escalating to SIGKILL would
// only fail differently.
const gracefulStopFailureIsFatal = true

// gracefulStopLabel names the graceful-stop mechanism, for logs that must not claim SIGTERM on a platform
// without it.
func gracefulStopLabel() string { return "SIGTERM" }

// gracefulStopUnavailable is the message for a graceful stop that could not be delivered.
func gracefulStopUnavailable(pid int, err error) string {
	return fmt.Sprintf("✗ failed to signal PID %d: %v", pid, err)
}

// forceStopNotice explains that the graceful stop was not honoured and the stop is escalating.
func forceStopNotice(pid int, grace string) string {
	return fmt.Sprintf("… PID %d did not exit within %s — escalating to SIGKILL", pid, grace)
}

// forceStopResult is the success line after a force stop, and it must not hide that the plane's own
// shutdown path was skipped.
func forceStopResult(pid int) string {
	return fmt.Sprintf("✓ serve killed (PID %d) — it ignored SIGTERM", pid)
}
