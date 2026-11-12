//go:build !windows

package guard

// platform_other.go — the guard works here.

// supported reports whether the guard can be BUILT and FUNCTION on this platform.
func supported() bool { return true }

// unsupportedReason is the platform-specific explanation, empty when supported.
func unsupportedReason() string { return "" }
