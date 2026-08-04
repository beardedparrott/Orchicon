//go:build !windows

package logging

import (
	"os"
	"syscall"
)

// RedirectStdStreams dup2's the given file onto fds 1 and 2 so panics and
// stray prints (which Go's runtime writes directly to fd 1/2, bypassing
// the slog handler) land in the current log file instead of /dev/null.
// The serve --detach child calls this after each rotation so post-rotation
// output keeps flowing into the active file.
func RedirectStdStreams(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Dup2(int(f.Fd()), 1)
	_ = syscall.Dup2(int(f.Fd()), 2)
}
