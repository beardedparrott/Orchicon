//go:build !windows

package main

// privilege_unix.go — the Unix half of the process's own privilege identity.

import "os"

// runningElevated reports whether this process holds a privileged token.
func runningElevated() bool { return false }

// invokingUID is the uid the supervisor's own process runs as.
func invokingUID() int { return os.Getuid() }

// invokingGID is the gid the supervisor's own process runs as.
func invokingGID() int { return os.Getgid() }

// unixSocketsSupported reports whether postgres's Unix-domain socket transport
// exists on this platform. It does on Unix.
const unixSocketsSupported = true

// platformDirHint is "" on Unix: the ownership remedy is the `sudo chown` the
// caller builds, so no platform-specific sentence is needed.
func platformDirHint() string { return "" }
