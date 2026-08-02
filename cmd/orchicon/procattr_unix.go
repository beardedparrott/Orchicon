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

// fileOwner returns the numeric uid/gid of a file (Unix).
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	if st, ok2 := fi.Sys().(*syscall.Stat_t); ok2 {
		return int(st.Uid), int(st.Gid), true
	}
	return 0, 0, false
}
