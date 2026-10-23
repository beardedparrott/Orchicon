//go:build windows && orchicon_desktop

package main

import "golang.org/x/sys/windows"

// processAlive reports whether pid still exists.
//
// OpenProcess rather than a wait handle: this process does not own the pid (it is the PREDECESSOR), so
// there is nothing to wait on. A process that has exited but is still referenced by a handle will fail to
// open with ERROR_INVALID_PARAMETER, which is exactly the "gone" answer we want.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}
