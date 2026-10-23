//go:build !windows && orchicon_desktop

package main

import "syscall"

// processAlive reports whether pid exists. Signal 0 is the documented existence probe: it performs no
// permission check beyond "can this process be signalled", which for our own child-replacement is always yes.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
