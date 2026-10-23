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
func TestMSIRerunAsksWhichInstance(t *testing.T) {
	wxs := wxsSource(t)

	// The instance page must be reachable from MaintenanceTypeDlg, which is the page a re-run lands on.
	if !strings.Contains(wxs, `Dialog="MaintenanceTypeDlg" Control="RepairButton" Event="NewDialog" Value="InstanceDlg"`) {
		t.Error("a re-run never reaches InstanceDlg: MSI enters maintenance mode, shows " +
			"MaintenanceWelcomeDlg/MaintenanceTypeDlg and skips WelcomeDlg, so [INSTANCE] silently " +
			"falls back to the property default and the setup runs against an instance the operator " +
			"never chose")
	}

	// And the pre-selection must come from the record, so the SAME instance can be re-applied.
	if !strings.Contains(wxs, `Property="INSTANCE" Value="[INSTALLEDINSTANCE]"`) {
		t.Error("a re-run does not pre-select the instance installed last, so the operator is shown a " +
			"default that may be a different instance than the package set up")
	}

	// The record has to exist, be written, and be searchable.
	for _, want := range []string{
		`Property Id="INSTALLEDINSTANCE"`,
		`RegistrySearch Id="SearchInstalledInstance"`,
		`RegistryValue Name="InstalledInstance" Type="string" Value="[INSTANCE]"`,
		`<ComponentGroupRef Id="InstanceRecordComponents" />`,
	} {
		if !strings.Contains(wxs, want) {
			t.Errorf("the instance record is incomplete, missing %q — without it a second run has nothing "+
				"to pre-select", want)
		}
	}
}

// REMOVE MUST STILL BE REACHABLE FROM A RE-RUN.
//
// The repair wiring deliberately hangs off MaintenanceTypeDlg's Repair button rather than
// MaintenanceWelcomeDlg's Next: overriding the welcome page would carry Remove down with it, and an
// operator who wants to uninstall must still get to the type page.
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
func TestMSIInstanceBackIsContextual(t *testing.T) {
	wxs := wxsSource(t)
	if !strings.Contains(wxs, `Dialog="InstanceDlg" Control="Back" Event="NewDialog" Value="MaintenanceTypeDlg" Order="1" Condition="Installed"`) {
		t.Error("InstanceDlg's Back has no maintenance arm, so a re-run's Back goes to the licence page " +
			"instead of the maintenance type page")
	}
	if !strings.Contains(wxs, `Dialog="InstanceDlg" Control="Back" Event="NewDialog" Value="LicenseAgreementDlg"`) {
		t.Error("InstanceDlg's Back lost its first-install arm (the licence page)")
	}
}

// THE UNINSTALL'S DATA QUESTION MUST BE REACHABLE FROM THE ARP PATH.
//
// This is the guard #815 was missing. It asserted that a Publish MENTIONED RemoveOptionsDlg, which
// cannot distinguish a reachable page from one whose condition is unsatisfiable at that point. The
// publish hangs off MaintenanceWelcomeDlg's Next with Condition REMOVE="ALL" — and that IS satisfiable,
// but only because Add/Remove Programs sets REMOVE=ALL on the command line BEFORE the UI runs. A
// plain double-click of the MSI does not, so it correctly falls through to MaintenanceTypeDlg instead.
//
// The reachability therefore depends on REMOVE=ALL being pre-set, which is the ARP route: asserted here
// so a future edit cannot move this page onto a path where that is never true.
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
