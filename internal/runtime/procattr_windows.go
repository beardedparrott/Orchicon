//go:build windows

package runtime

import (
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on Windows: process groups are a POSIX concept
// and the image path's kill semantics there are the plain process kill below.
func setProcessGroup(cmd *exec.Cmd) {}

// killGroup kills the engine CLI process. Windows has no process-group
// signal, so this is the pre-existing behaviour.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// createNoWindow is CREATE_NO_WINDOW (0x08000000), which stdlib syscall does not declare.
//
// NOT DETACHED_PROCESS: these children's stdout/stderr are piped back to this daemon, so they stay attached
// to its job and die with it. CREATE_NO_WINDOW suppresses the WINDOW and nothing else.
//
// THE DAEMON ITSELF IS STARTED DETACHED (no console), and Win32 gives a console-subsystem child of a
// console-less process a brand new window — so WITHOUT THIS every engine/pg/nats spawn below put a window on
// the operator's screen, in a burst, during an install.
const createNoWindow = 0x08000000

// setProcAttrNoWindow stops a helper tool from opening a console window.
//
// It COMPOSES rather than assigns: images.go calls setProcessGroup on the same command, and overwriting
// SysProcAttr would silently drop that group and break build cancellation.
func setProcAttrNoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
