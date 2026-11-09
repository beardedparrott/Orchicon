//go:build windows

package main

import "os"

// openConsoleInput opens this process's own console input device.
func openConsoleInput() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}
