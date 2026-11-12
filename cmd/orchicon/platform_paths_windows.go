//go:build windows

package main

// platform_paths_windows.go — there is no container residency on Windows, so this value exists only
// to keep defaultDataDirFor TOTAL and a Windows boot DIAGNOSABLE: without it.

import (
	"os"
	"path/filepath"
)

// platformContainerDataDir is the container-residency data dir. Container residency is unreachable on
// Windows; this value is a diagnosable fallback, not a supported layout.
func platformContainerDataDir() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return filepath.Join(base, "orchicon")
	}
	return filepath.Join(os.TempDir(), "orchicon")
}

// procNetRoutePath is EMPTY on Windows: there is no /proc/net/route, so the read fails soft and
// bridgeGatewayIP falls through to its conventional base+1 fallback.
const procNetRoutePath = ""
