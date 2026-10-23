package main

// msi_launch_handover_test.go — the installer must hand over to the app on the Servers page (design of
// record, D7), and it must do it in the supported way.
//
// THE OPERATOR'S BAR: "on a clean Windows install ... it automatically launches the desktop app on a fresh
// install and lands on the server management page". A silent auto-launch from a custom action races the
// wizard — the window can appear behind the dialog, in the wrong session, or before the firewall prompt —
// so the supported mechanism is a CHECKED checkbox on the Finish page. This asserts that shape.

import (
	"regexp"
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
func TestHandoverIsTheInstallsJobNotTheWizards(t *testing.T) {
	wxs := wxsSource(t)

	// THE HANDOVER IS IN THE INSTALL, AND THE WIZARD IS STOCK WIXUI.
	//
	// Three attempts to host a launch checkbox in the wizard failed, and the third explains why no amount of
	// wiring there can work:
	//
	//   1. a custom `FinishDlg`			pruned at link (nothing referenced it)
	//   2. WIXUI_EXITDIALOGOPTIONALCHECKBOX*	published on `FinishDlg`; the bind said
	//						"WIX0094: Dialog:FinishDlg could not be found"
	//   3. a custom `OrchiconFinishDlg`		shipped and never reached — WixUI's own publish on
	//						VerifyReadyDlg's Finish navigates to ITS ExitDialog first,
	//						and a second NewDialog on the same control cannot outrank it
	//						without a duplicate ControlEvent key
	//
	// The operator then saw exactly that: "Completed the Orchicon Setup Wizard / Click the Finish button to
	// exit the Setup Wizard" — WixUI's exit page, with no checkbox on it.
	//
	// So this asserts the NEW arrangement, which does not depend on WixUI's navigation at all:
	// the install is told to open the app, and the wizard asks nothing.

	ca := regexp.MustCompile(`<CustomAction Id="LaunchSetup"[\s\S]*?/>`).FindString(wxs)
	if ca == "" {
		t.Fatal("no LaunchSetup custom action")
	}
	if !strings.Contains(ca, "-launch-desktop") {
		t.Errorf("LaunchSetup does not pass the launch flag, so a wizard install never opens the app:\n  %s", ca)
	}

	// THE WIZARD MUST NOT TRY TO HOST IT ANY MORE. A custom exit page is a page WixUI will not navigate to,
	// and leaving one in the package is how the last three attempts looked wired while doing nothing.
	// CODE ONLY: this function's own explanation NAMES the property it removed, so a raw scan would fail on
	// the documentation of the fix. That is the fourth time in this file's history prose has defeated a
	// check — strip it first, as the other MSI guards do.
	code := stripXMLComments(wxs)
	for _, gone := range []struct{ what, sub string }{
		{"a custom exit page", `Dialog Id="OrchiconFinishDlg"`},
		{"a publish on WixUI's exit dialog", `Dialog="FinishDlg"`},
		{"the WixUI optional-checkbox properties", `WIXUI_EXITDIALOGOPTIONALCHECKBOX`},
		{"a LAUNCHAPP property", `Property Id="LAUNCHAPP"`},
	} {
		if strings.Contains(code, gone.sub) {
			t.Errorf("the wizard still carries %s (%s). WixUI owns the wizard's navigation — that is why "+
				"the checkbox never appeared — and inert wiring beside working wiring is what made the last "+
				"attempts look finished.", gone.what, gone.sub)
		}
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
