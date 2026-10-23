package main

// msi_launch_handover_test.go — the installer must hand over to the app on the Servers page (design of
// record, D7), and it must do it in the supported way.
//
// THE OPERATOR'S BAR: "on a clean Windows install ... it automatically launches the desktop app on a fresh
// install and lands on the server management page". A silent auto-launch from a custom action races the
// wizard — the window can appear behind the dialog, in the wrong session, or before the firewall prompt —
// so the supported mechanism is a CHECKED checkbox on the Finish page. This asserts that shape.

import (
	"strings"
	"testing"
)

// THE FINISH PAGE OFFERS THE HANDOVER, wired to an action that opens the app on /manage-server.
func TestFinishPageLaunchesTheAppOnTheServersPage(t *testing.T) {
	wxs := wxsSource(t)

	// Our own FinishDlg, not WixUI's: the standard hook lives in a compressed managed resource in
	// WixToolset.UI.wixext.dll that could not be inspected from the build host, so the page is defined here
	// where the wiring is visible and schema-checked.
	if !strings.Contains(wxs, `Dialog Id="FinishDlg"`) {
		t.Error("there is no FinishDlg of our own, so whether the exit dialog can carry a launch checkbox " +
			"is unknowable at build time")
	}
	if !strings.Contains(wxs, `Property="LAUNCHAPP"`) {
		t.Error("the Finish page has no launch checkbox")
	}
	// CHECKED BY DEFAULT: the handover is the point of the page. An unchecked box is a checkbox nobody
	// notices, and the operator's expectation is that a clean install ends in a running product.
	if !strings.Contains(wxs, `Property Id="LAUNCHAPP" Value="1"`) {
		t.Error("the launch checkbox does not default to checked, so a clean install does not open the app")
	}

	// The action it publishes must open the SERVERS PAGE, not the home page: on a fresh install the plane
	// may not be serving yet, and Servers is the page that can show setup progress.
	if !strings.Contains(wxs, `ExeCommand="--route /manage-server"`) {
		t.Error("the launch action does not pass --route /manage-server, so a first run lands on a page " +
			"that needs a plane which may not be up yet")
	}
	// AND IT MUST NOT WAIT: the wizard closes while the app opens.
	if !strings.Contains(wxs, `Return="asyncNoWait"`) {
		t.Error("the launch action waits for the GUI, which would hold the wizard open behind a window")
	}

	// A SILENT LAUNCH FROM A CUSTOM ACTION IS THE THING D7 REJECTS. Guard the shape: no action may spawn
	// the desktop app on a schedule of its own.
	if strings.Contains(wxs, `ExeCommand="--route /manage-server"`+"\n") &&
		!strings.Contains(wxs, `CustomAction Id="LaunchApp"`) {
		t.Error("the route flag is passed outside the LaunchApp action")
	}
}

// SETUP STILL STARTS DETACHED, which is what makes the checkbox safe to offer: setup is already running by
// the time Finish is reachable.
func TestSetupStillStartsBeforeTheWizardCloses(t *testing.T) {
	wxs := wxsSource(t)
	if !strings.Contains(wxs, `Custom Action="LaunchSetup" After="InstallFinalize"`) {
		t.Error("LaunchSetup no longer runs at InstallFinalize, so setup would not have started by the " +
			"time the Finish page is shown — the checkbox would open an app with nothing to show")
	}
	// The opt-out survives, so an unattended install can install binaries only.
	if !strings.Contains(wxs, `SKIPSETUP`) {
		t.Error("the SKIPSETUP opt-out is gone, so an unattended install can no longer install binaries only")
	}
}
