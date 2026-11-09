//go:build !windows

package config

// defaults_unix.go — the Unix defaults for the two paths a user must be able to REACH.

// defaultDataDir is the control plane's state dir when ORCHICON_DATA_DIR is unset.
func defaultDataDir() string { return "/var/lib/orchicon" }

// defaultRuntimeSocket is the workflow runtime's socket when ORCHICON_RUNTIME_SOCKET is unset.
func defaultRuntimeSocket() string { return "/var/run/orchicon-runtime/runtime.sock" }
