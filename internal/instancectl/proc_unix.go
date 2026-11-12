//go:build !windows

package instancectl

// proc_unix.go — spawning the CLI detached, and liveness, on POSIX.

import (
	"io"
	"os/exec"
	"syscall"

	"github.com/beardedparrott/orchicon/internal/procwin"
)

func newCommand(bin string, args ...string) *exec.Cmd { return procwin.Command(bin, args...) }

// detach puts the child in its own session, so closing the terminal (or a GUI exiting) does not take a
// minutes-long install with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func writerOrDiscard(w interface{ Write([]byte) (int, error) }) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
