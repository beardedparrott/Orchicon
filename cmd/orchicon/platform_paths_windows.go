//go:build windows

package main

// platform_paths_windows.go — there is no container residency on Windows, so this value exists only to keep
// defaultDataDirFor TOTAL and a Windows boot DIAGNOSABLE: without it, a supervisor that somehow resolved the
// container residency would report an unopenable `/var/lib/orchicon` (a path that on Windows means the root of
// the current drive) instead of a real directory the operator recognises.
//
// The path is the host-shape state dir, under LOCALAPPDATA — reachable without elevation and per-user, which
// is where a Windows install belongs. os.TempDir() is the fallback when LOCALAPPDATA is unset.

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
// bridgeGatewayIP falls through to its conventional base+1 fallback. Container residency — the only caller's
// context — is unreachable here anyway, but an untagged `/proc/...` literal is exactly the class of code this
// port exists to remove, so the path is per-OS even where the result is unused.
const procNetRoutePath = ""
