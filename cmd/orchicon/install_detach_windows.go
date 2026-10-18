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
	_, err := startDetachedDaemonEnv(self, args, nil, logPath)
	return err
}

// startDetachedDaemonEnv is startDetachedDaemon with an EXPLICIT environment and the CHILD'S PID. See the
// Unix half for why both are needed; Windows behaves identically here.
func startDetachedDaemonEnv(self string, args, env []string, logPath string) (int, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	if env != nil {
		cmd.Env = env
	}
	// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP: the daemon must not inherit this console (otherwise a
	// Ctrl-C in the installer kills the runtime daemon it just started), and it needs its own process group
	// as a Ctrl-Break target. CREATE_NEW_PROCESS_GROUP alone was neither — it kept the console and only added
	// the target. See procattr_windows.go, which is the same pair for the plane's own detach.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}
