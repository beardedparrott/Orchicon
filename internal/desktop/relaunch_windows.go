//go:build windows

package desktop

import (
	"os/exec"
	"path/filepath"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS (0x00000008); stdlib syscall does not declare it.
const detachedProcess = 0x00000008

// startReplacement starts the replacement with NO inherited console and its own process group.
func startReplacement(self string, args []string) (int, error) {
	cmd := exec.Command(self, args...)
	// THE REPLACEMENT GETS ITS OWN WORKING DIRECTORY, NOT THIS WINDOW'S.
	cmd.Dir = filepath.Dir(self)
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
