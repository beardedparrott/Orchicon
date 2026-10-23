//go:build windows

package instancectl

// alive_windows.go — the Windows liveness probe: a process handle plus its exit code.
//
// OpenProcess + GetExitCodeProcess is the same pair internal/planecontrol uses; it needs no dependency
// beyond GOROOT's syscall, which keeps this package a leaf.

import "syscall"

const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
