//go:build windows

package main

// privilege_windows.go — the Windows half of the process's own privilege identity, and the ONE
// platform where "am I elevated?" is a question the services care about.

import "golang.org/x/sys/windows"

// runningElevated reports whether the current process token is elevated.
func runningElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// invokingUID/invokingGID report the invoking user's ids.
func invokingUID() int { return 0 }
func invokingGID() int { return 0 }

// unixSocketsSupported reports whether postgres's Unix-domain socket transport exists.
const unixSocketsSupported = false

// platformDirHint is non-empty on Windows, so the data-dir-ownership remedy names the Windows
// mechanism (take ownership from an elevated prompt) instead of a `sudo chown` that does not exist.
func platformDirHint() string { return "windows" }
