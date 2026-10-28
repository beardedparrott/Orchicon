//go:build windows

package main

import "os"

// openConsoleInput opens this process's own console input device.
//
// os.Stdin is not always the console: under `irm https://orchicon.dev/install.ps1 | iex` it is the HTTP body,
// and under a captured or redirected run it is whatever the parent handed over. CONIN$ is the documented way for
// a console process to address the console it is attached to, and it is what console_windows.go's
// reattachConsoleIO already uses for the same reason.
//
// It fails when the process has NO console (a service, a detached background run) — which is the case where a
// prompt must not appear at all, so failing is the right answer rather than an error to report.
func openConsoleInput() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}
