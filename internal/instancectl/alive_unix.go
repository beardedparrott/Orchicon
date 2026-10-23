//go:build !windows

package instancectl

// alive_unix.go — the POSIX liveness probe: signal 0 succeeds while the PID exists.

import "syscall"

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
