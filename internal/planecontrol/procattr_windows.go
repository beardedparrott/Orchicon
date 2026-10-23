//go:build windows

package planecontrol

// procattr_windows.go — no console window for a tool this package runs itself.
//
// THE REPORT DID NOT GO AWAY, AND THE REASON WAS SCOPE. "No, it's happening in the desktop app when you
// install a new server until it says 'setup complete'." The install runs as a DETACHED process, which by
// definition has no console — so every console-subsystem child ANYWHERE in its subtree gets a brand new
// window. Fixing cmd/orchicon's own spawns left this package's untouched: BridgeIP shells out to
// `docker network inspect` on the hot path, and TerminateProcessTree runs `taskkill` on every stop.
//
// CREATE_NO_WINDOW is the documented "run this console application with no window" flag. It is NOT
// DETACHED_PROCESS: these are short-lived helpers whose output is captured through pipes and whose exit
// code is the answer, and detaching them would give up both.
import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func setProcAttrNoWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
