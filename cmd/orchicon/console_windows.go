//go:build windows

package main

// console_windows.go — give the setup its own visible console when it was started without one.
//
// WHY THIS EXISTS. The MSI's last step runs `orchicon install`, and the setup asks for host
// ports and (optionally) runtime images. A Windows Installer custom action has NO CONSOLE, so
// the previous design reached for `cmd.exe /c start "…" <wrapper>.cmd` to manufacture one —
// three processes deep, with the wizard's answers threaded through a command line. Every bug
// in that chain was a quoting bug: an empty `[RUNTIMEIMAGES]` became an empty quoted string,
// a quote was eaten per hop, and cmd ended up being handed the wrapper's PATH as a command
// name ("'…orchicon-setup.cmd"  "dev" "' is not recognized as an internal or external
// command").
//
// The fix is to stop building a shell chain and let the program that needs a console make
// one. `orchicon install` re-executes ITSELF with CREATE_NEW_CONSOLE and exits: one process
// becomes two, there is no command line to quote, and the answers travel as environment (or
// as a plain `--instance dev` argument, which has no empty-value case).
//
// DETACHED, NOT ATTACHED. The parent (msiexec, or a Burn bundle) is a GUI process with no
// console to attach to, so CREATE_NEW_CONSOLE is the only option — and it must not be
// combined with DETACHED_PROCESS, which suppresses exactly the console we are asking for.

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
//
// GetConsoleWindow() returns NULL when the process has no console. A process started with
// CREATE_NO_WINDOW or from a GUI parent with none gets NULL; msiexec and a Burn bundle are
// both GUI subsystem, so the setup starts here.
func hasConsole() bool {
	// windows.NewLazySystemDLL, not the plain NewLazyDLL: the latter loads from anywhere on the
	// search path, which is the DLL-hijack shape. This is the same helper the rest of the
	// Windows-only code in this package already uses.
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	if err := getConsoleWindow.Find(); err != nil {
		return true // cannot tell: assume a console, never relaunch on a guess
	}
	hwnd, _, _ := getConsoleWindow.Call()
	return hwnd != 0
}

// relaunchInOwnConsole starts THIS executable again in a NEW visible console and returns
// true. The caller must then exit without doing any work.
//
// argv is passed through verbatim. On Windows a console-subsystem child receives its
// arguments as the command line the parent wrote, so exec.Command's quoting is exactly what
// the child's os.Args will see — there is no `cmd` in between to reinterpret anything.
func relaunchInOwnConsole() (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	args := os.Args[1:]
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), consoleRelaunchEnv+"=1")
	// NIL STDIO HERE IS CORRECT, AND THE CHILD MUST FIX IT — this comment used to say the
	// opposite ("no stdio wiring: the child owns its console and prints there"), which was
	// simply wrong.
	//
	// os/exec gives a child whose Stdout/Stderr are nil the NUL DEVICE. That is not a window,
	// it is nowhere: CREATE_NEW_CONSOLE hands the child a real console, but its handles still
	// point at NUL, so everything it prints is discarded and every read hits EOF. The result is
	// an install that opens a window, finishes in seconds and asks nothing — and a person
	// cannot tell it apart from one that decided not to prompt.
	//
	// Inheriting THIS process's handles instead would be no better: msiexec has no console, so
	// they are invalid or NUL too. The only handle that means anything is the child's OWN
	// console, which it opens for itself (see reattachConsoleIO).
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &windows.SysProcAttr{
		// CREATE_NEW_CONSOLE: the whole point. NEW_PROCESS_GROUP so a Ctrl-C in the
		// parent's group does not reach it.
		CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	// DO NOT WAIT. The parent is a short-lived custom action; waiting would hold the
	// installer open for the length of a multi-hundred-megabyte download. The child is
	// independent and writes its own log.
	_ = cmd.Process.Release()
	return true, nil
}

// reattachConsoleIO points this process's standard streams at its OWN console.
//
// CALLED BY THE RELAUNCHED CHILD, AND THAT IS WHAT MAKES THE SETUP ABLE TO ASK ANYTHING.
// CREATE_NEW_CONSOLE gives the process a console; it does not give it working handles. With
// os/exec's nil stdio the handles are the NUL device, so the install's own interactivity gate
// (installIsInteractive -> isCharDevice(os.Stdout)) is deciding about a discard stream:
//
//   - its prompts are written to nowhere, so the operator sees a blank window;
//   - its reads hit EOF immediately, so every question takes its default;
//   - and it all finishes in seconds, which reads as "the installer did not offer options"
//     rather than "the installer had no output".
//
// CONIN$ and CONOUT$ are the documented way for a console process to address its own console,
// and by the time this runs the console exists — the relaunch created it.
//
// Not fatal on failure: an install with no usable console must still be able to run headless,
// and the callers fall through to the non-interactive path.
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
