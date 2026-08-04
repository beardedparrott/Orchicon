//go:build windows

package logging

import "os"

// RedirectStdStreams is a no-op on native Windows. Orchicon's Windows
// support runs the whole Linux stack inside WSL2 (AGENTS.md invariant
// #10), where the unix implementation applies; a native Windows binary
// keeps os.Stdout/os.Stderr as-is.
func RedirectStdStreams(f *os.File) {}
