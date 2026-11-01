//go:build !windows

package mcpclient

import (
	"os/exec"
	"syscall"
)

// killProcessTree terminates a stdio child AND everything it forked.
//
// THE WHOLE GROUP, NOT THE PID. newStdioTransport sets Setpgid (sysprocattr_linux.go), so the child is its
// own process-group leader and `-pid` names the group. Signalling the pid alone would leave a Python server's
// forked workers running — and a leaked worker is indistinguishable from a leaked server when the sweep
// later looks for marker-bearing processes.
//
// This is the second half of the leak fix: mcpclient.connectOne now KILLS a child whose handshake failed,
// because the go-sdk does not close the transport on connect failure. Without this, every failed start
// leaked a live child — measured as an `aseprite_mcp` still running 18 hours later.
func killProcessTree(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	// The group first; ESRCH means it is already gone (fine). Then the process itself, in case Setpgid
	// did not apply (a non-Linux build path) and the group id is not ours to signal.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
	// Reap so the child does not linger as a zombie and ProcessState reflects the kill.
	_ = cmd.Wait()
}
