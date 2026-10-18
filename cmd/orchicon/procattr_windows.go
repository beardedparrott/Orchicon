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

// setProcAttrBackground detaches the child from the caller's console. This is the setsid ANALOGUE, and the
// previous comment here ("Windows does not support Setpgid. The child inherits the parent's console group, so
// Ctrl-C in the parent will also reach the child. This is acceptable...") was wrong to call it acceptable:
//
//   - DETACHED_PROCESS gives the child NO inherited console at all, so a Ctrl-C (or console close) in the
//     parent cannot reach the detached plane. That is exactly what `setsid` buys on Unix, and it is the whole
//     point of `serve --detach`: the plane must outlive the terminal that started it.
//   - CREATE_NEW_PROCESS_GROUP additionally makes the child the root of its own process group, which gives it
//     a stable Ctrl-Break TARGET — the graceful-stop mechanism on this platform (signals_windows.go).
//     Without it there is nothing to aim at and every stop is a force stop with no option to be gentler.
//
// Both flags together are what makes the detached-serve path work on Windows; either alone is not.
func setProcAttrBackground(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
}

// chownPath is a NO-OP on Windows, deliberately: there is no chown, ownership follows the token that created
// the file, and directories inherit their ACLs from the parent. Returning nil rather than an error is the
// point — ERROR_NOT_SUPPORTED here would fail a supervisor boot for a step that is meaningless on this
// platform (see container.go's prepare(), which chowns its data dirs unconditionally).
func chownPath(path string, uid, gid int) error { return nil }

// fileOwner is a no-op on Windows (no syscall.Stat_t).
func fileOwner(fi os.FileInfo) (int, int, bool) { return 0, 0, false }
