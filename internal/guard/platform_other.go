//go:build !windows

package guard

// platform_other.go — the guard works here.
//
// The shim is a `#!/bin/bash` script with symlinks named after each guarded binary, which is exactly
// what a POSIX host resolves through PATH. Nothing to gate.

// supported reports whether the guard can be BUILT and FUNCTION on this platform.
func supported() bool { return true }

// unsupportedReason is the platform-specific explanation, empty when supported.
func unsupportedReason() string { return "" }
