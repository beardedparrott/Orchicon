//go:build windows

package main

// console_windows.go — give the setup its own visible console when it was started without one.

import (
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// consoleRelaunchEnv marks the child of a console relaunch. Without it the child would find
// itself console-less in some hosting contexts and relaunch again, forever.
const consoleRelaunchEnv = "ORCHICON_CONSOLE_RELAUNCH"

// hasConsole reports whether this process already owns a usable console window.
func hasConsole() bool {
	// windows.NewLazySystemDLL, not the plain NewLazyDLL: the latter loads from anywhere on the search
	// path, which is the DLL-hijack shape.
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	if err := getConsoleWindow.Find(); err != nil {
		return true // cannot tell: assume a console, never relaunch on a guess
	}
	hwnd, _, _ := getConsoleWindow.Call()
	return hwnd != 0
}

// relaunchInOwnConsole starts THIS executable again in a NEW visible console and returns true.
func relaunchInOwnConsole() (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	args := os.Args[1:]
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), consoleRelaunchEnv+"=1")
	// NIL STDIO HERE IS CORRECT, AND THE CHILD MUST FIX IT — this comment used to say the opposite ("no
	// stdio wiring: the child owns its console and prints there"), which was simply wrong.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &windows.SysProcAttr{
		// CREATE_NEW_CONSOLE: the whole point. NEW_PROCESS_GROUP so a Ctrl-C in the
		// parent's group does not reach it.
		CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	// DO NOT WAIT.
	_ = cmd.Process.Release()
	return true, nil
}

// reattachConsoleIO points this process's standard streams at its OWN console.
func reattachConsoleIO() {
	if in, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = in
	}
	if out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout = out
		os.Stderr = out
	}
}

// ensureInteractiveConsole makes sure the setup has somewhere to ask its questions.
//
// Returns true when the process relaunched and the caller must exit. That is a SUCCESSFUL
// outcome, not a failure: the work has been handed to a process the operator can see.
//
// It is deliberately conservative — it only relaunches when
//
//	(a) this is a windowed-capable Windows build,
//	(b) the process has no console, AND
//	(c) interaction is actually wanted (the caller decided that),
//
// so a headless `orchicon install -Yes` in a pipeline is never silently re-launched into a
// window nobody is watching.
func ensureInteractiveConsole(wantInteractive bool) bool {
	if !wantInteractive {
		return false
	}
	if os.Getenv(consoleRelaunchEnv) == "1" {
		// Already the relaunched child. It has the console; it does NOT have the handles —
		// they are NUL — so it must attach them before anything tries to print or ask.
		reattachConsoleIO()
		return false
	}
	// An explicit operator choice wins over the probe: ORCHICON_INSTALL_INTERACTIVE=0 means
	// "never prompt", and re-launching a console would be the opposite of that.
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("ORCHICON_INSTALL_INTERACTIVE"))); v == "0" || v == "false" || v == "no" {
		return false
	}
	if hasConsole() {
		// Started from a console already (an operator running `msiexec /i` in a shell, or the
		// CLI by hand). Attach the streams to it so output and prompts land there.
		reattachConsoleIO()
		return false
	}
	relaunched, err := relaunchInOwnConsole()
	if err != nil {
		// Not fatal: fall through and install non-interactively rather than failing the
		// whole setup because a window could not be created.
		return false
	}
	return relaunched
}
