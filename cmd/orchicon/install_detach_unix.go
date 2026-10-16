//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetachedDaemon launches a long-running daemon (the runtime daemon, or the
// host services) in its own session, detached from the install command's
// terminal, with its output appended to logPath. The child keeps running after
// the parent exits.
func startDetachedDaemon(self string, args []string, logPath string) error {
	_, err := startDetachedDaemonEnv(self, args, nil, logPath)
	return err
}

// startDetachedDaemonEnv is startDetachedDaemon with an EXPLICIT environment and the CHILD'S PID.
//
// A nil env INHERITS this process's environment; a non-nil one REPLACES it (a caller that wants
// inheritance plus additions builds it from os.Environ()). The PID is what lets a caller record the child it
// just started — the host-services supervisor is stopped by PID (stopHostServices), and there is no
// pre-written PID file to read the way `serve --detach` has one.
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}
