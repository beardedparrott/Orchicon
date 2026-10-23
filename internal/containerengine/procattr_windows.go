//go:build windows

package containerengine

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW (0x08000000). stdlib syscall does not declare it, so it is spelled out
// — the same treatment cmd/orchicon's procattr_windows.go gives it.
//
// IT IS NOT DETACHED_PROCESS. These children keep their stdout/stderr pipes (the probe reads the version
// string out of one), so they must stay attached to this process's job and die with it. CREATE_NO_WINDOW
// suppresses the WINDOW and nothing else.
const createNoWindow = 0x08000000

// setProcAttrNoWindow stops a probe from opening a console window.
func setProcAttrNoWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
