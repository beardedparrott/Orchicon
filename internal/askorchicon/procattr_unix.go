//go:build !windows

package askorchicon

import (
	"os/exec"
	"syscall"
)

// setChildProcessGroup places the child in its own process group so the
// parent can kill the whole group (opencode + MCP sidecars) with
// syscall.Kill(-pgid, sig) after an unexpected death.
func setChildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
