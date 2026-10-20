//go:build !windows

package main

// console_other.go — the console relaunch is a Windows-only concern.
//
// On Unix the setup is started from a terminal (install.sh, or `orchicon install` by hand),
// so there is always a console to prompt on, and there is no MSI custom action to be started
// without one. Keeping the stub here means install.go can call this unconditionally instead
// of carrying a build-tagged branch in the middle of the install flow.

// ensureInteractiveConsole always reports "did not relaunch": on Unix there is nothing to
// relaunch for, so the caller proceeds with the install it was already asked to do.
func ensureInteractiveConsole(bool) bool { return false }
