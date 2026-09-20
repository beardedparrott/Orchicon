//go:build linux

package mcpclient

import "syscall"

// stdioSysProcAttr is the process attributes for a stdio MCP child on Linux.
//
// PDEATHSIG is the parent-death signal: when the control plane dies, the kernel kills the child,
// so an MCP server process can never outlive a dead plane (ADR-0008). Setpgid puts the child in
// its own process group so the boot-time sweep can signal the whole group, and so an in-flight
// child cannot receive terminal signals intended for the plane.
//
// PDEATHSIG IS LINUX-ONLY, which is exactly why this tiny function exists per-OS: it used to be
// written inline in transport_stdio_unix.go, whose `!windows` tag covers darwin as well, and the
// field does not exist there — so the file failed to COMPILE for darwin/amd64 and darwin/arm64.
//
// See sysprocattr_unix_nolinux.go for the other platforms.
func stdioSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}
}
