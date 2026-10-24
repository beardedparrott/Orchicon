//go:build windows

package runtime

import (
	"os/exec"
)

// setProcessGroup is a no-op on Windows: process groups are a POSIX concept
// and the image path's kill semantics there are the plain process kill below.
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills the engine CLI process. Windows has no process-group
// signal, so this is the pre-existing behaviour.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
