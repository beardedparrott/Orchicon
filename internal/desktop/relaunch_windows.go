//go:build windows

package desktop

import (
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS (0x00000008); stdlib syscall does not declare it.
const detachedProcess = 0x00000008

// startReplacement starts the replacement with NO inherited console and its own process group.
//
// It is a GUI binary, so it does not need a console — but DETACHED_PROCESS is still the right flag, because
// it also means the child is not torn down when the parent's console goes away, which is what a
// self-restart requires.
func startReplacement(self string, args []string) (int, error) {
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}
