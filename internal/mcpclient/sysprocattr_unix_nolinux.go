//go:build !windows && !linux

package mcpclient

import "syscall"

// stdioSysProcAttr is the process attributes for a stdio MCP child on the Unix platforms that are
// NOT Linux — darwin, and the BSDs.
func stdioSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
