//go:build windows

package askorchicon

import "os/exec"

// setChildProcessGroup is a no-op on Windows: SysProcAttr.Setpgid does
// not exist there. The child inherits the parent's console group, so
// Ctrl-C in the parent also reaches it — acceptable for the dev flow.
func setChildProcessGroup(cmd *exec.Cmd) {}
