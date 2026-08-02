//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetachedDaemon launches a long-running daemon (the runtime
// daemon) in its own session, detached from the install command's
// terminal, with its output appended to logPath. The child keeps running
// after the parent exits.
func startDetachedDaemon(self string, args []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
