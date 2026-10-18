//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcAttrBackground(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// chownPath hands a path to uid:gid. It is a SEAM because os.Chown is not portable: on Windows it returns
// ERROR_NOT_SUPPORTED, and a supervisor whose prepare() chowns its data dirs would then FAIL ITS BOOT for a
// reason that is not a failure there. procattr_windows.go is the other half.
func chownPath(path string, uid, gid int) error { return os.Chown(path, uid, gid) }

// fileOwner returns the numeric uid/gid of a file (Unix).
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	if st, ok2 := fi.Sys().(*syscall.Stat_t); ok2 {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}
