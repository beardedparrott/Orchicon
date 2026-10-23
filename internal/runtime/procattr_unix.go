//go:build unix

package runtime

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the engine CLI the leader of its own process group,
// so the whole build/pull tree can be signalled at once. Podman and Docker
// both run the real builder as a CHILD of the CLI; killing only the parent
// leaves the builder running and the image half-written.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the command's whole process group (see
// setProcessGroup). Falls back to killing just the process when the group is
// already gone.
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// setProcAttrNoWindow is a NO-OP on Unix: a Unix process has no console to allocate. It exists so
// quietCommand (quiet.go) has one definition on every platform rather than a build-tagged call site.
func setProcAttrNoWindow(*exec.Cmd) {}
