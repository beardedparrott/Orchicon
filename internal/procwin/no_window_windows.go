//go:build windows

package procwin

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW (0x08000000). stdlib syscall does not declare it, so it is spelled out —
// the same treatment DETACHED_PROCESS gets elsewhere in this codebase.
const createNoWindow = 0x08000000

// applyNoWindow gives the child a console that exists but is never shown.
func applyNoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// |= , never = : a caller-set process group (build cancellation) or detach flag must survive.
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
