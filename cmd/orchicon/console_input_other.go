//go:build !windows

package main

import "os"

// openConsoleInput opens the controlling terminal, which on POSIX is /dev/tty.
func openConsoleInput() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
