package main

// install_desktop.go — WINDING UP THE INSTALL BY OPENING THE APP.
//
// THE OPERATOR, after the third attempt at a Finish-page checkbox: "Maybe we should launch the desktop in the
// install script that pops up after it determines an environment is healthy?"
//
// THEY ARE RIGHT, AND THE REASON IS STRUCTURAL RATHER THAN A BUG I KEEP MAKING. The handover lived in the
// MSI's UI, and the MSI's UI is not ours: WixUI owns the wizard's navigation. Three attempts failed there, and
// the third one explains why it cannot work by wishing harder:
//
//	1. a custom `FinishDlg`		PRUNED at link — this package's UI sequence never referenced it
//	2. WIXUI_EXITDIALOGOPTIONALCHECKBOX*	published on `FinishDlg`, which the bind reported as
//						"WIX0094: Dialog:FinishDlg could not be found"
//	3. a custom `OrchiconFinishDlg`		SHIPPED but never reached: WixUI's own publish on VerifyReadyDlg's
//						Finish navigates to ITS ExitDialog first, and a second NewDialog
//						on the same control cannot outrank it without a duplicate ControlEvent key
//
//		…which is exactly what the operator then saw: "Completed the Orchicon Setup Wizard /
//		Click the Finish button to exit the Setup Wizard" — WixUI's exit page, not ours.
//
// SO THE HANDOVER MOVES TO WHERE THE FACT IS KNOWN. `orchicon install` already WAITS for the control plane to
// answer /healthz; when that succeeds the environment is genuinely up, which is a far better moment to open a
// GUI than "the wizard is about to close". The installer stops asking, and the thing that did the work opens
// the app.
//
// IT IS OPT-IN, AND THE MSI SETS IT. A one-liner on a headless box, a CI install and `--non-interactive` in a
// pipe all keep the old behaviour: install, print, exit. Only a caller that says so gets a window — which is
// also what keeps this testable, because "did it try to open a GUI" must not depend on the host having one.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// desktopLauncherEnv is the environment spelling, so a script can request the same thing the wizard does.
const desktopLauncherEnv = "ORCHICON_INSTALL_LAUNCH_DESKTOP"

// desktopBinName is the desktop client's file name on this platform.
func desktopBinName() string {
	if runtime.GOOS == "windows" {
		return "orchicon-desktop.exe"
	}
	return "orchicon-desktop"
}

// launchDesktop opens the installed desktop app on ITS START PAGE, and reports whether it managed to.
//
// NOT /manage-server, AND THAT WAS A REAL FIRST-RUN DEFECT. The app used to be handed `--route /manage-server`,
// on the D7 reasoning that a first run should land on the page that finishes setting up. What that produced on a
// clean Windows install was a window with NO TAB BAR and no way to sign in:
//
//   - /manage-server is in the frontend's PUBLIC_PATHS, because it must render when no plane answers (it is how
//     you start one). `requireAuth` returns EARLY for a public path — before it calls ensureSession — so landing
//     there never triggers the redirect to /login either.
//   - with no session, __root.tsx renders that public path BARE (no AppShell) by design, so there is no nav, no
//     tab bar, and no route to a login screen. The operator was simply stuck.
//
// The start page has neither problem: the guard resolves the session, an unauthenticated visitor is redirected to
// /login with ?next=/, and an ALREADY-authenticated one (a reinstall over a live session) goes straight to the
// app. Routing explicitly to /login would be wrong in that second case — it would ask a signed-in user to sign in.
//
// The plane is up by the time this runs (install waits for /healthz), so there is always something to
// authenticate against. /manage-server remains reachable from the Control nav once signed in, and stays public so
// it still works when the plane is DOWN and cannot be logged into at all.
//
// IT NEVER FAILS THE INSTALL. The install has already succeeded by the time this runs — the plane is up and
// serving — so a missing or unlaunchable GUI is a note, not an error. Turning a completed install into a
// failure over a window would be the worst trade available here.
//
// THE SIBLING IS THE SOURCE OF TRUTH, not PATH: the MSI installs orchicon.exe and orchicon-desktop.exe into one
// directory, so the app belonging to THIS install is the one next to the binary that installed it. Resolving
// through PATH would find a different build — the same class of defect resolvePlaneBinary documents.
func launchDesktop() bool {
	exe, err := os.Executable()
	if err != nil {
		fmt.Printf("note: could not locate this binary to find the desktop app: %v\n", err)
		return false
	}
	desktop := filepath.Join(filepath.Dir(exe), desktopBinName())
	if _, err := os.Stat(desktop); err != nil {
		fmt.Printf("note: no desktop app next to %s — not opening one\n", exe)
		return false
	}

	// NO --route. See above: the app's own start page is the one that works whether or not a session exists.
	cmd := exec.Command(desktop)
	// AN EXPLICIT WORKING DIRECTORY. The app must not inherit the install's — an install runs DETACHED, and a
	// GUI whose cwd is a directory that later disappears cannot start its own children on Windows
	// (CREATE_ERROR, "The directory name is invalid"), which is the reported "it complained about … the
	// directory the desktop ran from". The install directory is where this binary lives, so it exists.
	cmd.Dir = filepath.Dir(exe)
	// DETACHED, so the window outlives this process: install exits immediately after printing its summary, and
	// a GUI that died with it would be worse than not launching one at all.
	setProcAttrBackground(cmd)
	// NO INHERITED STDIO. The app is a GUI; if it inherits this console's handles the wizard's console stays
	// pinned open behind the window, which reads as a hung install.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Printf("note: could not open the desktop app (%v) — start it from the Start menu\n", err)
		return false
	}
	// RELEASED, so nothing waits on a GUI. There is no exit code we would act on.
	_ = cmd.Process.Release()
	fmt.Printf("==> opened the desktop app on %s\n", "http://localhost:"+env("ORCHICON_CONTROL_PORT", "8080"))
	return true
}
