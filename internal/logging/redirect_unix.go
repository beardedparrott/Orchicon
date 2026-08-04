//go:build !windows

package logging

import (
	"os"

	"golang.org/x/sys/unix"
)

// RedirectStdStreams dup2's the given file onto fds 1 and 2 so panics and
// stray prints (which Go's runtime writes directly to fd 1/2, bypassing
// the slog handler) land in the current log file instead of /dev/null.
// The serve --detach child calls this after each rotation so post-rotation
// output keeps flowing into the active file.
//
// unix.Dup2 (golang.org/x/sys/unix) is used instead of syscall.Dup2
// because syscall.Dup2 is only defined on a subset of Unix platforms
// (linux/amd64, not linux/arm64 or darwin); x/sys/unix covers all of
// them. The release workflow builds linux+darwin × amd64+arm64.
func RedirectStdStreams(f *os.File) {
	if f == nil {
		return
	}
	_ = unix.Dup2(int(f.Fd()), 1)
	_ = unix.Dup2(int(f.Fd()), 2)
}
