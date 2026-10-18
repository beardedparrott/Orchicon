//go:build windows

package config

// defaults_windows.go — the Windows defaults, and the reason this pair exists rather than one string in
// config.go: `/var/lib/orchicon` and `/var/run/...` are not paths a Windows user can reach. On Windows they
// resolve against the root of the current drive, so a default install would either fail to create its state
// dir or scatter it somewhere nobody looks.
//
// THE SOCKET DEFAULT IS REACHABLE, NOT FUNCTIONAL, and that distinction is stated here rather than implied.
// The workflow runtime needs a Linux container; there is no Unix socket on the Windows host, so a Windows plane
// degrades to IN-PROCESS execution and this path is only where a socket WOULD live if one were ever created.
// Setting it to something under /var/run would make the failure look like a permission problem.

import (
	"os"
	"path/filepath"
)

// defaultDataDir is the control plane's state dir when ORCHICON_DATA_DIR is unset: per-user, no elevation.
func defaultDataDir() string { return userStateDir() }

// defaultRuntimeSocket is where the workflow runtime's socket would live. See the file header: reachable, not
// functional on this platform.
func defaultRuntimeSocket() string {
	return filepath.Join(os.TempDir(), "orchicon-runtime", "runtime.sock")
}

// userStateDir is LOCALAPPDATA\\orchicon, falling back to the temp dir when LOCALAPPDATA is unset (a service
// token, a stripped environment).
func userStateDir() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "orchicon")
	}
	return filepath.Join(os.TempDir(), "orchicon")
}
