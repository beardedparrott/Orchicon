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

// chownPath hands a path to uid:gid.
func chownPath(path string, uid, gid int) error { return os.Chown(path, uid, gid) }

// fileOwner returns the numeric uid/gid of a file (Unix).
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	if st, ok2 := fi.Sys().(*syscall.Stat_t); ok2 {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}

// setProcAttrNoWindow is a NO-OP on Unix, and that is not laziness: a Unix process has no console
// to allocate, so a supervised child can never open a window.
func setProcAttrNoWindow(*exec.Cmd) {}
