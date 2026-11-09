//go:build windows

package instancectl

// proc_windows.go — spawning the CLI detached, and liveness, on Windows.

import (
	"io"
	"os/exec"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

// DETACHED_PROCESS (0x8): the child gets NO inherited console, so it outlives the app that launched
// it.
const detachedProcess = 0x00000008

func newCommand(bin string, args ...string) *exec.Cmd { return procwin.Command(bin, args...) }

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

func writerOrDiscard(w interface{ Write([]byte) (int, error) }) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
