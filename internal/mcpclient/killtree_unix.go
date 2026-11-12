//go:build !windows

package mcpclient

import (
	"os/exec"
	"syscall"
)

// killProcessTree terminates a stdio child AND everything it forked.
func killProcessTree(cmd *exec.Cmd) {
	pid := cmd.Process.Pid
	// The group first; ESRCH means it is already gone (fine). Then the process itself, in case Setpgid
	// did not apply (a non-Linux build path) and the group id is not ours to signal.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
	// Reap so the child does not linger as a zombie and ProcessState reflects the kill.
	_ = cmd.Wait()
}
