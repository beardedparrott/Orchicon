//go:build !windows

package desktop

import (
	"os/exec"
	"path/filepath"
	"syscall"
)

// startReplacement starts the replacement in its own session, so closing the terminal (or the parent) cannot
// take it with it.
func startReplacement(self string, args []string) (int, error) {
	cmd := exec.Command(self, args...)
	// The replacement's working directory, never this window's — see the Windows half for the failure a
	// stale inherited cwd causes.
	cmd.Dir = filepath.Dir(self)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}
