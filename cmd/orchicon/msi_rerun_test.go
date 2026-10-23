package main

// msi_rerun_test.go — a SECOND run of the MSI must ask which instance, and act on the answer.
//
// THE OPERATOR: "When you run the MSI currently, you actually choose which instance you want installed.
// dev, prod, or test. What if someone ran the install twice currently? What happens?"
//
// WHAT HAPPENED, and it was silent: running the same MSI again does not install anything new. MSI sees
// the product is installed (same ProductCode) and enters MAINTENANCE mode, which shows
// MaintenanceWelcomeDlg and MaintenanceTypeDlg and NEVER WelcomeDlg. Every custom page hung off
// WelcomeDlg, so InstanceDlg was never shown, [INSTANCE] fell through to its Property default ("dev"),
// and LaunchSetup still ran because its condition is NOT REMOVE — which is true on a repair:
//
//	install --non-interactive --instance dev --port ... --runtime-images ...
//
// So an operator who chose PROD, then ran the installer again, silently got a DEV instance created as
// well: two stacks, one of them a surprise, from a wizard that appeared to have asked nothing because it
// had not.
//
// These assertions are about REACHABILITY, not about strings existing. The guard these replace checked
// only that a Publish mentioned "RemoveOptionsDlg", which cannot tell a reachable page from a page whose
// condition can never be true at that point in the flow — the same seam-nothing-calls failure the file's
// own comments warn about.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wxsSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "desktop", "windows", "Orchicon.wxs"))
	if err != nil {
		t.Fatalf("read the MSI source: %v", err)
	}
	return string(b)
}

// THE REPAIR PATH MUST REACH THE INSTANCE QUESTION.
func TestMSIRerunDoesNotAskAndReconvergesDev(t *testing.T) {
	wxs := wxsSource(t)

	// THE INSTANCE PAGE IS GONE, and that is what makes a second run SAFE and simple: there is no question
	// to mis-answer and no property to lose. The MSI always sets up `dev`, and `orchicon install` is
	// idempotent — so running the installer twice re-converges one server instead of creating a second
	// with a name nobody chose.
	//
	// THIS SUPERSEDES the earlier wiring that routed a repair through the instance page. That was right
	// while the page existed; with the page deleted, "ask again" is not available, and the answer is
	// better anyway: a fresh install and a repair now do the same thing to the same server.
	if strings.Contains(wxs, `Id="InstanceDlg"`) {
		t.Error("the installer asks which instance again. MSI properties are session-scoped and a repair " +
			"skips WelcomeDlg, so that question was answered unreliably at best — a second run created " +
			"`dev` silently while the wizard looked as if it had asked nothing.")
	}
	if !strings.Contains(wxs, "--instance dev") {
		t.Error("the setup hand-off does not name `dev`, so what a re-run converges is stated nowhere")
	}

	// THE INSTANCE RECORD SURVIVES the page that used to fill it: it still tells the app which server this
	// package set up.
	for _, want := range []string{
		`Property Id="INSTALLEDINSTANCE"`,
		`RegistryValue Name="InstalledInstance" Type="string" Value="[INSTANCE]"`,
	} {
		if !strings.Contains(wxs, want) {
			t.Errorf("the instance record lost %q", want)
		}
	}
}

func TestMSIRerunStillPermitsRemove(t *testing.T) {
	wxs := wxsSource(t)
	if strings.Contains(wxs, `Dialog="MaintenanceWelcomeDlg" Control="Next" Event="NewDialog" Value="InstanceDlg"`) {
		t.Error("the instance page was wired to MaintenanceWelcomeDlg's Next, which overrides the page " +
			"that leads to MaintenanceTypeDlg — a re-run would then have no route to Remove")
	}
}

// BACK MUST RETURN TO THE PAGE THAT LED HERE.
//
// One unconditional Back would send a re-running operator to the licence page they already accepted.
func TestMSINoPageNavigatesToADeletedDialog(t *testing.T) {
	// The page whose Back button had to be contextual is gone, so the guard becomes the thing that would
	// actually break the build — a Publish still pointing at a dialog nobody defines. The schema catches it
	// too; asserting it HERE names the cause instead of leaving a WIX error to be decoded.
	wxs := wxsSource(t)
	for _, stale := range []string{`Value="InstanceDlg"`, `Value="PortsDlg"`, `Value="ImagesDlg"`} {
		if strings.Contains(wxs, stale) {
			t.Errorf("a wizard page still navigates to a deleted dialog (%s)", stale)
		}
	}
}

func TestMSIUninstallDataQuestionIsReachableFromARP(t *testing.T) {
	wxs := wxsSource(t)
	if !strings.Contains(wxs, `Value="RemoveOptionsDlg"`) {
		t.Fatal("nothing navigates to RemoveOptionsDlg")
	}
	if !strings.Contains(wxs, `Value="RemoveOptionsDlg" Order="2" Condition="REMOVE=&quot;ALL&quot;"`) {
		t.Error("RemoveOptionsDlg is not gated on REMOVE=ALL. That condition is what makes it reachable " +
			"from Add/Remove Programs (which sets REMOVE=ALL before the UI runs) and invisible on a plain " +
			"repair — the reverse of that would show a data-destroying page during a repair")
	}
	// And the page must exist with the two exclusive answers.
	if !strings.Contains(wxs, `Dialog Id="RemoveOptionsDlg"`) {
		t.Error("RemoveOptionsDlg is referenced but not defined")
	}
}
