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

// THE FINISH PAGE OFFERS THE HANDOVER, THROUGH WIXUI'S OWN CHECKBOX.
//
// The first attempt defined our own FinishDlg with a checkbox of its own. It did not work, and the reason is
// worth recording because nothing failed: a dialog from an extension is keyed by Id, so defining "FinishDlg"
// here did not replace WixUI's. The built package was found to contain BOTH control sets — our LaunchCheck and
// SetupNote, AND WixUI's OptionalCheckBox — while the operator saw a Finish page with no checkbox at all:
//
//	"it never had the checkbox to launch the orchicon desktop at the end and when I hit finish it just
//	 ended. Nothing happened."
//
// WixUI's FinishDlg already carries that control, bound to WIXUI_EXITDIALOGOPTIONALCHECKBOX, and HIDES it
// unless WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT is set. So the fix is three properties' worth of wiring and no
// dialog of ours at all — which is strictly fewer moving parts.
func TestFinishPageLaunchesTheAppOnTheServersPage(t *testing.T) {
	wxs := wxsSource(t)

	// 1. THE TEXT IS WHAT MAKES THE BOX APPEAR. Without it WixUI renders the page with no checkbox, which is
	//    exactly the reported symptom.
	if !strings.Contains(wxs, `Property Id="WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT"`) {
		t.Error("WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT is not set, so WixUI HIDES its optional checkbox — " +
			"a Finish page with nothing to tick, and a Finish button that does nothing")
	}
	// 2. CHECKED BY DEFAULT: a clean install should end with the app open, not with a box nobody notices.
	if !strings.Contains(wxs, `Property Id="WIXUI_EXITDIALOGOPTIONALCHECKBOX" Value="1"`) {
		t.Error("the optional checkbox does not default to checked, so a clean install does not open the app")
	}
	// 3. THE FINISH BUTTON RUNS THE ACTION, conditioned on the box — the documented WixUI pattern.
	if !strings.Contains(wxs, `Dialog="FinishDlg" Control="Finish" Event="DoAction" Value="LaunchApp"`) {
		t.Error("FinishDlg's Finish button does not run LaunchApp, so ticking the box does nothing")
	}
	if !strings.Contains(wxs, `Condition="WIXUI_EXITDIALOGOPTIONALCHECKBOX = 1"`) {
		t.Error("the launch is not conditioned on the checkbox, so it would fire even when unticked")
	}

	// 4. IT OPENS THE SERVERS PAGE, not the home page: on a fresh install the plane may not be serving yet,
	//    and /manage-server is the page that can show setup progress.
	if !strings.Contains(wxs, `ExeCommand="--route /manage-server"`) {
		t.Error("the launch action does not pass --route /manage-server")
	}
	if !strings.Contains(wxs, `Return="asyncNoWait"`) {
		t.Error("the launch action waits for the GUI, which would hold the wizard open behind a window")
	}

	// 5. AND WE DO NOT DEFINE A SECOND FinishDlg. Two dialogs of one name is not a merge with a defined
	//    result, and it is what made the first attempt look wired while rendering nothing.
	if strings.Contains(wxs, `Dialog Id="FinishDlg"`) {
		t.Error("the package defines its own FinishDlg again. WixUI already supplies one; defining a " +
			"second with the same Id silently produced a page with both control sets and no visible " +
			"checkbox. Use WIXUI_EXITDIALOGOPTIONALCHECKBOX* instead.")
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
