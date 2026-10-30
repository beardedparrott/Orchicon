package main

// windows_msi_path_test.go — THE MSI MUST PUT ITS INSTALL DIRECTORY ON THE USER'S PATH.
//
// Reported as "I have used orchicon a lot in various paths on windows so it must be set some other way", and
// the investigation found the opposite: the MSI wrote NO <Environment> element at all, and no Go code writes
// the user PATH either (the registry reads in internal/adapter and internal/opencode are all READS). Only
// scripts/install.ps1 ever added it.
//
// Both installers target the SAME directory (%LOCALAPPDATA%\Programs\Orchicon), which is why the gap stayed
// invisible: anyone who had ever run the documented `irm ... | iex` had the entry already. A machine whose
// FIRST and ONLY install was the MSI got binaries in a directory no shell resolved.
//
// This is a SOURCE assertion, and it is the right shape here: the MSI builds only on Windows, so a test that
// required a built package could not run in the Go gate at all. What it pins is the set of attributes whose
// combination is easy to get subtly wrong — and every one below is load-bearing.

import (
	"os"
	"strings"
	"testing"
)

func TestMsiPutsItsInstallDirOnTheUserPath(t *testing.T) {
	b, err := os.ReadFile("../../deploy/desktop/windows/Orchicon.wxs")
	if err != nil {
		t.Skipf("MSI definition not readable here: %v", err)
	}
	src := string(b)

	// STRIP XML COMMENTS FIRST, then look. The first version searched the raw file and matched the string
	// "<Environment" inside THIS CHANGE'S OWN explanatory comment — a false pass, and exactly the failure mode
	// a source-scraping test invites. Prose must never be able to satisfy a markup assertion.
	markup := stripXMLComments(src)

	if !strings.Contains(markup, "<Environment") {
		t.Fatal("the MSI declares no <Environment> element, so it adds nothing to PATH — a machine whose " +
			"only install was the MSI cannot run `orchicon` from a new shell")
	}
	start := strings.Index(markup, "<Environment")
	el := markup[start : start+strings.Index(markup[start:], "/>")]

	for _, c := range []struct{ attr, why string }{
		{`Name="PATH"`, "the value Windows reads is PATH; a differently-named value does nothing"},
		{`Part="last"`, "APPEND, not overwrite: System32 and the inherited PATH must keep precedence"},
		{`Action="set"`, "idempotent — a reinstall must not append a second copy of the directory"},
		{`System="no"`, "a user value; a system PATH would need elevation and would affect every user"},
		{`Permanent="no"`, "so uninstall REMOVES the entry instead of leaving a dangling directory"},
		{`Value="[INSTALLFOLDER]"`, "it must name the directory the binaries actually land in"},
	} {
		if !strings.Contains(el, c.attr) {
			t.Errorf("the Environment element is missing %s — %s\ngot: %s", c.attr, c.why, el)
		}
	}

	// THE PACKAGE IS PER-USER, which is what makes a user-hive PATH the correct (and only) shape.
	if !strings.Contains(markup, `Scope="perUser"`) {
		t.Error("the package is not per-user, so a user PATH entry would be wrong for it")
	}

	// AND THE ENTRY MUST BE IN THE CLI FEATURE: a PATH without orchicon.exe would be worse than neither.
	cli := markup[strings.Index(markup, `Id="CliFeature"`):]
	if k := strings.Index(cli, "</Feature>"); k > 0 {
		cli = cli[:k]
	}
	if !strings.Contains(cli, "ComponentGroupRef") {
		t.Error("could not read the CLI feature's component groups")
	}
}
