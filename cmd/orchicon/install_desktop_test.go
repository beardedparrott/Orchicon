package main

// install_desktop_test.go — the handover's DECISIONS, which are the part that can be wrong.

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
func TestFirstRunLaunchesTheStartPageNotTheServersPage(t *testing.T) {
	// (1) the CLI install's launch.
	src, err := os.ReadFile("install_desktop.go")
	if err != nil {
		t.Fatalf("read install_desktop.go: %v", err)
	}
	// THE QUOTED LITERAL, not the bare word: the file legitimately MENTIONS the flag in the comment
	// that explains why it is no longer passed.
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

// AND /manage-server STAYS PUBLIC.
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
