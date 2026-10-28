//go:build !windows

package main

import "os"

// openConsoleInput opens the controlling terminal, which on POSIX is /dev/tty.
//
// promptTerminal already tries /dev/tty FIRST on every platform, so on POSIX this is only reached when that
// failed — a process with no controlling terminal. Returning the error there is correct: a prompt with nowhere
// to read from must not appear.
func openConsoleInput() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
