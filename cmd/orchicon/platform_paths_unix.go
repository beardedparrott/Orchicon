//go:build !windows

package main

// platform_paths_unix.go — the container-residency data dir, which is an IMAGE path on purpose:
// inside the runtime container the supervisor is root and /var/lib/orchicon is the mount point.

// platformContainerDataDir is where a CONTAINER-residency install keeps its state when ORCHICON_DATA_DIR is
// unset.
func platformContainerDataDir() string { return "/var/lib/orchicon" }

// procNetRoutePath is the kernel routing table the container-readiness probe reads to learn the
// bridge gateway.
const procNetRoutePath = "/proc/net/route"
