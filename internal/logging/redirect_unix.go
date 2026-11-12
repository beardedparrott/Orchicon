//go:build !windows

package logging

import (
	"os"

	"golang.org/x/sys/unix"
)

// RedirectStdStreams dup2's the given file onto fds 1 and 2 so panics and stray prints (which Go's
// runtime writes directly to fd 1/2.
func RedirectStdStreams(f *os.File) {
	if f == nil {
		return
	}
	_ = unix.Dup2(int(f.Fd()), 1)
	_ = unix.Dup2(int(f.Fd()), 2)
}
