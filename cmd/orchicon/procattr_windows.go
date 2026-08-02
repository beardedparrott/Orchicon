//go:build windows

package main

import (
	"os"
	"os/exec"
)

func setProcAttrBackground(cmd *exec.Cmd) {
	// Windows does not support Setpgid. The child inherits the parent's
	// console group, so Ctrl-C in the parent will also reach the child.
	// This is acceptable: dev stop still works via PID file signaling.
}

// fileOwner is a no-op on Windows (no syscall.Stat_t).
func fileOwner(fi os.FileInfo) (int, int, bool) { return 0, 0, false }
