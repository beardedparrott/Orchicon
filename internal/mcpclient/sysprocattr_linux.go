//go:build linux

package mcpclient

import "syscall"

// PDEATHSIG IS LINUX-ONLY, which is exactly why this tiny function exists per-OS: it used to be
// written inline in transport_stdio_unix.go, whose `!windows` tag covers darwin as well.
func stdioSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
