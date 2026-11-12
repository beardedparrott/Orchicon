package instancectl

// spawndir.go — WHICH DIRECTORY A CHILD RUNS IN, which must never be inherited from the app.

import (
	"os"
	"path/filepath"
)

// spawnDir is the working directory for a child process this package starts, or "" when nothing
// usable could be found (in which case the caller inherits.
func spawnDir() string {
	if home, err := os.UserHomeDir(); err == nil && isUsableDir(home) {
		return home
	}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); isUsableDir(dir) {
			return dir
		}
	}
	return ""
}

// isUsableDir reports whether p is an existing directory.
func isUsableDir(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// unusableCwd explains a failure to start a child when this process's own working directory is the
// reason.
func unusableCwd() string {
	if _, err := os.Getwd(); err != nil {
		return "this process has no usable working directory (it was started from a directory that no longer " +
			"exists), which is why Windows refused to start the child at all — relaunch Orchicon from the " +
			"Start menu, or from any directory that exists"
	}
	return ""
}
