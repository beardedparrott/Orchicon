//go:build !windows

package main

// console_other.go — the console relaunch is a Windows-only concern.

// ensureInteractiveConsole always reports "did not relaunch": on Unix there is nothing to
// relaunch for, so the caller proceeds with the install it was already asked to do.
func ensureInteractiveConsole(bool) bool { return false }
