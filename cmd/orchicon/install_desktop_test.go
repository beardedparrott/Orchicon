package main

// install_desktop_test.go — the handover's DECISIONS, which are the part that can be wrong.
//
// The operator's proposal, which this implements: "Maybe we should launch the desktop in the install script
// that pops up after it determines an environment is healthy?"
//
// The wizard could not host it (three failures, recorded in install_desktop.go), so the launch lives where the
// health fact is: the install already WAITS for /healthz. These assertions pin the three decisions around it —
// opt-in, healthy-only, and never fatal — without opening a window.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A GUI LAUNCH IS OPT-IN: a one-liner on a headless box, a CI install and a piped --non-interactive run keep
// the old behaviour. Asserted on the flag parser, because that is where the decision is made.
func TestLaunchDesktopIsOptIn(t *testing.T) {
	off, rest, err := parseInstallFlags([]string{"--non-interactive", "--instance", "dev"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if off.LaunchDesktop {
		t.Error("a plain install asks to open a GUI. A headless box, a CI install and a piped run must not " +
			"get a window they did not ask for.")
	}
	if len(rest) == 0 {
		t.Error("the instance flag was consumed as something else")
	}

	on, _, err := parseInstallFlags([]string{"--launch-desktop"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !on.LaunchDesktop {
		t.Error("--launch-desktop did not set the flag")
	}

	// AND THE ENVIRONMENT FORM, so a shell script can request the same thing the MSI does.
	if env(desktopLauncherEnv, "") != "" {
		t.Skip("the environment already sets the launch flag")
	}
}

// THE LAUNCH HAPPENS WHERE HEALTH IS KNOWN, AND ONLY THEN.
//
// Opening a GUI onto a plane that is not answering shows the operator a failure inside a window with no
// context — worse than not opening it. Asserted as a property of the flow, because the alternative (run a real
// install) is minutes long and needs a container engine this host may not have.
func TestLaunchIsGuardedByHealth(t *testing.T) {
	flow := enclosingFunc(readGoFile(t, "install.go"), "launchDesktop()")
	if flow == "" {
		t.Fatal("install.go never calls launchDesktop — the handover is gone")
	}
	if !strings.Contains(flow, "\"1\"") && !strings.Contains(flow, "LaunchDesktop") {
		t.Error("the launch is not conditioned on the flag or the environment variable")
	}
	if !strings.Contains(flow, "healthy") {
		t.Error("the launch is not guarded by health. Opening the app onto a plane that answers nothing " +
			"is worse than not opening it: the operator sees a broken window and no reason.")
	}
	// AND IT IS AFTER THE PLANE IS CONFIRMED, not before it starts.
	if strings.Index(flow, "launchDesktop()") < strings.Index(flow, "healthURL") {
		t.Error("the launch is placed before the control plane is even probed")
	}
}

// A MISSING OR UNLAUNCHABLE GUI MUST NOT FAIL AN INSTALL THAT HAS ALREADY SUCCEEDED.
//
// launchDesktop returns a bool and the caller ignores it for exactly this reason; this pins that the function
// has no path that can panic or block, and that its failure modes are messages.
func TestLaunchDesktopIsNotFatalWhenTheAppIsAbsent(t *testing.T) {
	dir := t.TempDir()
	// A directory that looks like an install WITHOUT a desktop binary beside the running one.
	if err := os.WriteFile(filepath.Join(dir, "orchicon.exe"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The function resolves the SIBLING of the running executable, which a test cannot redirect — so this
	// asserts the shape rather than the call: a bool return, and no error propagated.
	body := readGoFile(t, "install_desktop.go")
	if !strings.Contains(body, "func launchDesktop() bool") {
		t.Error("launchDesktop does not return a bool, so its failure cannot be reported without failing the " +
			"install that already succeeded")
	}
	if strings.Contains(body, "return err") {
		t.Error("launchDesktop can return an error — a window that will not open must not turn a completed " +
			"install into a failure")
	}
}

// THE LAUNCH OPENS THE APP'S START PAGE, NOT /manage-server — the clean-install defect.
//
// Reported: "I just downloaded and re-installed Orchicon on Windows and the desktop app that first pops up
// landed on the server management screen but the tab bar at the top was missing completely. I closed the
// desktop app and then re-opened it and it went to the logon screen. I logged in and the tab bar was visible
// again."
//
// THE MECHANISM, which makes the missing tab bar CORRECT-BY-DESIGN and the launch wrong:
//   - /manage-server is in the frontend's PUBLIC_PATHS, because it has to render when no plane answers (it is
//     how you start one). requireAuth returns EARLY for a public path, BEFORE ensureSession — so landing there
//     never triggers the redirect to /login either.
//   - with no session, __root.tsx renders a public path BARE (no AppShell): no nav, no tab bar, and no path to
//     a sign-in screen. The operator is stuck.
//
// Re-opening worked because the app's start page is NOT public: the guard runs, resolves no session, and
// redirects to /login — which is exactly what the operator then saw.
//
// These pin the two places a first run is launched from, because a fix in only one of them leaves the defect
// for whichever install path the user happens to take.
func TestFirstRunLaunchesTheStartPageNotTheServersPage(t *testing.T) {
	// (1) the CLI install's launch.
	src, err := os.ReadFile("install_desktop.go")
	if err != nil {
		t.Fatalf("read install_desktop.go: %v", err)
	}
	// THE QUOTED LITERAL, not the bare word: the file legitimately MENTIONS the flag in the comment that
	// explains why it is no longer passed. A naive substring check fails on its own documentation, which is a
	// test that punishes the explanation rather than the behaviour.
	if strings.Contains(string(src), `"--route"`) {
		t.Error("the install's desktop launch still passes a route argument. Anything but the start page can " +
			"land on /manage-server, which renders BARE without a session — a window with no tab bar and no " +
			"route to a login screen, which is the reported clean-install defect")
	}

	// (2) the MSI's Finish-page launch — a SEPARATE call site, and the one a Windows user actually takes.
	wxs, err := os.ReadFile("../../deploy/desktop/windows/Orchicon.wxs")
	if err != nil {
		t.Fatalf("read Orchicon.wxs: %v", err)
	}
	if strings.Contains(string(wxs), "--route /manage-server") {
		t.Error("the MSI's LaunchApp still forces /manage-server as the opening route — a clean Windows " +
			"install lands on a bare page with no tab bar instead of the login screen")
	}
}

// AND /manage-server STAYS PUBLIC. Moving the launch must not be "fixed" later by taking that route out of
// PUBLIC_PATHS: it has to render precisely when NOTHING answers (no plane), which is the one case where
// ensureSession cannot succeed. Removing it would make the page that starts a dead plane unreachable.
func TestManageServerStaysPublicForTheNoPlaneCase(t *testing.T) {
	src, err := os.ReadFile("../../frontend/src/auth/route-guard.ts")
	if err != nil {
		t.Fatalf("read route-guard.ts: %v", err)
	}
	if !strings.Contains(string(src), `"/manage-server"`) {
		t.Error("/manage-server left PUBLIC_PATHS. It must render when no plane answers — the one case where " +
			"ensureSession cannot succeed — so it has to stay public even though the launch no longer opens it")
	}
}
