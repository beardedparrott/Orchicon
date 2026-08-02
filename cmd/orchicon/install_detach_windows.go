//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetachedDaemon launches the runtime daemon in a new process group
// on Windows (no setsid); the child keeps running after the parent exits
// and writes its output to logPath.
func startDetachedDaemon(self string, args []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	return cmd.Start()
}
