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
//
// IT IS NOT DETACHED_PROCESS, and the difference is the whole point. DETACHED_PROCESS means NO console, which
// is precisely why the child's own children then each allocate a visible one. CREATE_NO_WINDOW means a HIDDEN
// console, which the whole subtree inherits.
//
// It also leaves the child attached to this process's job and to its stdio pipes, so a probe can still read
// the tool's output and a supervised child still dies with its supervisor. DETACHED_PROCESS would give up
// both.
func applyNoWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// |= , never = : a caller-set process group (build cancellation) or detach flag must survive.
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
