//go:build !windows

package main

// platform_paths_unix.go — the container-residency data dir, which is an IMAGE path on purpose: inside the
// runtime container the supervisor is root and /var/lib/orchicon is the mount point the launcher and the
// container contract both name.

// platformContainerDataDir is where a CONTAINER-residency install keeps its state when ORCHICON_DATA_DIR is
// unset.
func platformContainerDataDir() string { return "/var/lib/orchicon" }

// procNetRoutePath is the kernel routing table the container-readiness probe reads to learn the bridge gateway.
// A CONSTANT rather than an inline literal so the parser is testable on a fixture; the parser itself is pure Go
// and deliberately untagged.
const procNetRoutePath = "/proc/net/route"
