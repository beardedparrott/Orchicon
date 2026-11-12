//go:build windows

package main

// procattr_windows.go — process attributes and ownership on Windows, where both differ from POSIX rather than
// merely being spelled differently.

import (
	"os"
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS (0x00000008). stdlib syscall does not declare it; the value is the
// documented Win32 constant.
const detachedProcess = 0x00000008

// setProcAttrBackground detaches the child from the caller's console.
func setProcAttrBackground(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}

// chownPath is a NO-OP on Windows, deliberately: there is no chown, ownership follows the token
// that created the file, and directories inherit their ACLs from the parent.
func chownPath(path string, uid, gid int) error { return nil }

// fileOwner is a no-op on Windows (no syscall.Stat_t).
func fileOwner(fi os.FileInfo) (int, int, bool) { return 0, 0, false }

// setProcAttrNoWindow stops a SUPERVISED CHILD from allocating a console window.
const createNoWindow = 0x08000000

func setProcAttrNoWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
