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

	// THE EXIT PAGE IS OURS, and the route to it is a page this package PROVABLY has.
	//
	// TWO WIRINGS FAILED BEFORE THIS ONE, and both are recorded in the source because the second looked
	// correct:
	//
	//   1. a custom `FinishDlg` — PRUNED at link, because this package's UI sequence never referenced it;
	//   2. the documented WixUI hook (WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT / ...CHECKBOX) published on
	//      `FinishDlg` — the BIND failed with
	//          WIX0094: The identifier 'Dialog:FinishDlg' could not be found
	//      i.e. there is no dialog of that name in this package to publish on. Which name the WixUI
	//      extension gives its exit dialog is not something this file can verify: its definitions live in a
	//      compressed managed resource in WixToolset.UI.wixext.dll, and `wix-check.sh` cannot reach the link.
	//
	// SO THE GUARD DEMANDS THE SHAPE THAT CANNOT GUESS: an exit dialog this package DEFINES, and a publish
	// FROM a page it already routes to.
	if !strings.Contains(wxs, `Dialog Id="OrchiconFinishDlg"`) {
		t.Error("there is no exit page of our own. Naming one of WixUI's dialogs is a guess this file " +
			"cannot check, and the wrong guess is a LINK failure the compile-only checker cannot see")
	}
	if strings.Contains(wxs, `Dialog="FinishDlg"`) {
		t.Error("a publish targets `FinishDlg`, whose existence in the linked package is exactly what the " +
			"last bind failure disproved (WIX0094). Do not name WixUI's exit dialog.")
	}
	if !strings.Contains(wxs, `Publish Dialog="VerifyReadyDlg" Control="Finish" Event="NewDialog" Value="OrchiconFinishDlg" Order="2"`) {
		t.Error("VerifyReadyDlg's Finish does not lead to our exit page. VerifyReadyDlg is a page this " +
			"package already publishes TO, so the linker keeps it — which is what makes it a safe anchor.")
	}

	// The checkbox exists, defaults to CHECKED (the handover is the point of the page), and is wired.
	if !strings.Contains(wxs, `Property="LAUNCHAPP" CheckBoxValue="1"`) {
		t.Error("the exit page has no launch checkbox")
	}
	if !strings.Contains(wxs, `Property Id="LAUNCHAPP" Value="1"`) {
		t.Error("the launch checkbox does not default to CHECKED, so a clean install does not open the app")
	}
	if !strings.Contains(wxs, `Event="DoAction" Value="LaunchApp"`) {
		t.Error("nothing runs the launch action, so ticking the box does nothing")
	}
	if !strings.Contains(wxs, `Condition="LAUNCHAPP=&quot;1&quot;"`) {
		t.Error("the launch action is not conditioned on the checkbox, so it runs whether or not it is ticked")
	}

	// The action opens the SERVERS page, not the home page: on a fresh install the plane may not be serving
	// yet, and Servers is the page that can show setup progress.
	if !strings.Contains(wxs, `ExeCommand="--route /manage-server"`) {
		t.Error("the launch action does not pass --route /manage-server, so a first run lands on a page " +
			"that needs a plane which may not be up yet")
	}
	// AND IT MUST NOT WAIT: the wizard closes while the app opens.
	if !strings.Contains(wxs, `Return="asyncNoWait"`) {
		t.Error("the launch action waits for the GUI, which would hold the wizard open behind a window")
	}
}

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
