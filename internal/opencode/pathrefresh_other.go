//go:build !windows

package opencode

// pathrefresh_other.go — the registry PATH refresh is a Windows concern only, and this file is the
// statement of that rather than an omission.
//
// On Unix the plane's PATH is also a snapshot, but there is no single authoritative store to re-read:
// a login shell composes it from profile files this process has already sourced, so "the real PATH"
// has no answer that a background service could act on. The equivalent remedy on Unix is the one the
// error message already names — install under $HOME where the candidates live, or set
// ORCHICON_OPENCODE_BIN.
//
// It exists so internal/opencode compiles without a build tag at every call site.
func registryPathDirs() []string { return nil }
