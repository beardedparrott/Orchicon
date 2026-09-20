//go:build !windows && !linux

package mcpclient

import "syscall"

// stdioSysProcAttr is the process attributes for a stdio MCP child on the Unix platforms that
// are NOT Linux — darwin, and the BSDs.
//
// Setpgid is portable and is kept, so the child still gets its own process group (which the
// transport's Close and the manager rely on when signalling).
//
// PDEATHSIG IS DELIBERATELY ABSENT: the field does not exist on these platforms, and there is no
// portable equivalent. That is a REAL, STATABLE WEAKNESS rather than an oversight — on darwin a
// control plane killed with SIGKILL can leave an MCP child running, where on Linux the kernel
// reaps it. The boot-time sweep is the mitigation, and it is itself Unix-only (see sweep_unix.go);
// on these platforms a child of an uncleanly-killed plane is therefore not guaranteed to be
// reaped, and the manager's Close path is the only guarantee. Written down so it is a known
// limitation rather than a surprise.
func stdioSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
