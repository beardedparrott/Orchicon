//go:build !windows

package opencode

// pathrefresh_other.go — the registry PATH refresh is a Windows concern only, and this file is the
// statement of that rather than an omission.
func registryPathDirs() []string { return nil }
