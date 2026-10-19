package main

// windows_msi_test.go — THE MSI MUST CARRY THE WHOLE PRODUCT, AND NOTHING CAN DRIFT.
//
// The MSI is the one artifact this project cannot build or install in its own test
// environment: WiX is a .NET tool, this host has no dotnet, and the artifact only
// exists on windows-latest. So the failure modes that matter are the ones a
// BUILD would not catch but an INSTALL would:
//
//  1. A PARTIAL PRODUCT. The MSI's components are individual files, so a
//     component quietly dropped from the .wxs produces an MSI that installs
//     cleanly and is simply missing the TUI — or the plane itself. Nothing fails;
//     the user runs `orch` and it is not there.
//  2. ELEVATION. Orchicon's model is "the plane and its services are host
//     processes of the user who installs them". A per-machine package would put
//     the binaries under one identity and the services under another.
//  3. A SETUP THAT CANNOT PROMPT. The wrapper must claim a human is present, or
//     the port questions never appear — the hazard install.ps1 documents at
//     length, and the reason the wizard's answer travels as an env var.
//  4. A RELEASE THAT DOES NOT PUBLISH IT. Building the MSI and then leaving it in
//     the run is the "artifact built and silently not published" failure the
//     release ASSETS list exists to prevent.
//
// Asserted against the real files, because every one of these is a wiring
// question and no Go code can be executed to answer it.

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readMSIFile(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func wxs() string { return filepath.Join("..", "..", "deploy", "desktop", "windows", "Orchicon.wxs") }

// TestMSIIsWellFormedXML — an unparseable .wxs fails on windows-latest, six
// minutes into a release. Catch it here, where it costs nothing.
func TestMSIIsWellFormedXML(t *testing.T) {
	body := readMSIFile(t, wxs())

	var doc struct {
		XMLName xml.Name
		Package []struct {
			Name  string `xml:"Name,attr"`
			Scope string `xml:"Scope,attr"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("deploy/desktop/windows/Orchicon.wxs is not well-formed XML: %v", err)
	}
	if len(doc.Package) != 1 {
		t.Fatalf("the .wxs declares %d <Package> elements, want exactly 1", len(doc.Package))
	}
	if doc.Package[0].Scope != "perUser" {
		t.Errorf("Package Scope = %q, want \"perUser\".\n"+
			"A per-machine package installs the binaries as one identity and runs the services as\n"+
			"another — the one arrangement this product does not support — and it forces a UAC prompt.",
			doc.Package[0].Scope)
	}
}

// TestMSICarriesEveryShippedBinary is the partial-product guard.
//
// The MSI installs three executables and one setup wrapper. A missing component
// produces a successful install of a product that cannot do its job, which is
// precisely the class of failure a build cannot see.
func TestMSICarriesEveryShippedBinary(t *testing.T) {
	body := readMSIFile(t, wxs())

	// KEYED ON THE Source= WIRING, NOT ON THE FILENAME. Every one of these names also
	// appears in the file's own prose — the header and the Feature descriptions say
	// "orch.exe" and "orchicon-desktop.exe" — so a filename match keeps passing after
	// the COMPONENT is deleted. (Found exactly that way: removing the <Component> for
	// orch.exe left a grep hit in a comment and this test still went green.)
	for _, want := range []struct{ src, why string }{
		{`Source="$(var.SourceDir)\orchicon.exe"`, "the control plane, the CLI and the setup step itself"},
		{`Source="$(var.SourceDir)\orch.exe"`, "the TUI — without it the package has no remote client"},
		{`Source="$(var.SourceDir)\orchicon-desktop.exe"`, "the desktop app the MSI exists to deploy"},
		{`Source="$(var.SourceDir)\orchicon-setup.cmd"`, "the hand-off to `orchicon install`; without it the MSI installs binaries and never sets them up"},
	} {
		if !strings.Contains(body, want.src) {
			t.Errorf("Orchicon.wxs has no component with %s (%s).\n"+
				"A component missing from the .wxs yields an MSI that INSTALLS CLEANLY and is simply\n"+
				"missing that piece — no error, no warning, just a product that cannot do its job.\n"+
				"(Matched on Source=, not the filename: every one of these is also named in prose.)",
				want.src, want.why)
		}
	}
}

// TestMSIPublishesTheVersionAndIcon — the two things a malformed package gets
// wrong silently: a missing Version makes WiX produce an unversioned package that
// can never upgrade, and a missing icon leaves a blank entry in Add/Remove
// Programs.
func TestMSIPublishesTheVersionAndIcon(t *testing.T) {
	body := readMSIFile(t, wxs())

	for _, want := range []string{
		`Version="$(var.Version)"`,
		`SourceFile="$(var.SourceDir)\orchicon-desktop.ico"`,
		`ARPPRODUCTICON`,
		`MajorUpgrade`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Orchicon.wxs does not contain %q.\n"+
				"Version drives both the ARP entry and MajorUpgrade; the icon is the Add/Remove\n"+
				"Programs glyph; without MajorUpgrade an install over an older version leaves two\n"+
				"entries and two sets of files.", want)
		}
	}
}

// TestSetupWrapperClaimsAHuman — the wizard's answers have to reach the child, and
// the child has to be allowed to ask.
func TestSetupWrapperClaimsAHuman(t *testing.T) {
	wrapper := readMSIFile(t, "..", "..", "deploy", "desktop", "windows", "orchicon-setup.cmd")

	// THE ONE THAT MATTERS: without this the port questions never appear, because
	// installIsInteractive() checks the installer's OWN stdout for a terminal.
	if !strings.Contains(wrapper, `set "ORCHICON_INSTALL_INTERACTIVE=1"`) {
		t.Error("orchicon-setup.cmd does not set ORCHICON_INSTALL_INTERACTIVE=1.\n" +
			"The installer only prompts when it can see a terminal of its own, so the wizard's\n" +
			"setup would run with NO port questions and NO runtime-image offer — silently taking\n" +
			"every default.")
	}
	// The wizard's multi-select travels as %2 → this env var. Without it, a choice
	// made in the UI is discarded by the non-interactive path.
	if !strings.Contains(wrapper, "ORCHICON_INSTALL_RUNTIME_IMAGES") {
		t.Error("orchicon-setup.cmd does not pass ORCHICON_INSTALL_RUNTIME_IMAGES, so a " +
			"runtime image selected in the wizard would be silently ignored")
	}
	// It must DELEGATE to the real installer rather than reimplementing it.
	if !strings.Contains(wrapper, `orchicon.exe" install`) {
		t.Error("orchicon-setup.cmd does not run `orchicon install` — the setup must delegate, " +
			"not carry a second implementation that can drift from the CLI")
	}
	// A failure must not look like a hang: the window stays open.
	if !strings.Contains(wrapper, "pause") {
		t.Error("orchicon-setup.cmd does not pause, so the console vanishes on completion and " +
			"the operator cannot read what happened — the classic Windows installer complaint")
	}
}

// TestReleaseBuildsAndPublishesTheMSI — built but not published is the failure the
// ASSETS list exists to prevent.
func TestReleaseBuildsAndPublishesTheMSI(t *testing.T) {
	rel := readMSIFile(t, "..", "..", ".github", "workflows", "release.yml")

	if !strings.Contains(rel, "wix build deploy/desktop/windows/Orchicon.wxs") {
		t.Error("release.yml never runs `wix build` on the .wxs — the MSI would not exist")
	}
	if !strings.Contains(rel, "-o \"dist/desktop/orchicon_${v}_windows_amd64.msi\"") {
		t.Error("release.yml does not name the MSI output the same way the other artifacts are " +
			"named, so it may not be collected")
	}
	// THE ASSETS LIST: without .msi here the file is built, uploaded as a workflow
	// artifact, and then silently left out of the release.
	if !strings.Contains(rel, "-name '*.msi'") {
		t.Error("the release ASSETS find does not include '*.msi'. The package would be built " +
			"and then silently NOT published — the exact failure the ASSETS list exists to catch.")
	}
}

// TestCICoversTheWindowsDesktopBuild — this path had never run.
//
// `orchicon-desktop.exe` was unexercised code before this job: the only thing that
// built it was release.yml's desktop-windows job, which fires on a main tag and had
// not run since windows/amd64 joined the matrix. Making a release its first test is
// how a release ships a broken installer.
func TestCICoversTheWindowsDesktopBuild(t *testing.T) {
	ci := readMSIFile(t, "..", "..", ".github", "workflows", "ci.yml")

	if !strings.Contains(ci, "windows-desktop-msi:") {
		t.Fatal("ci.yml has no windows-desktop-msi job — the Windows desktop app and the MSI " +
			"would be exercised for the first time DURING a release")
	}
	// It must actually build the app, not just the Go packages.
	if !strings.Contains(ci, "wails build -platform windows/amd64") {
		t.Error("the CI job does not run `wails build` for windows, so it would not catch a " +
			"break in the Windows desktop app")
	}
	// And it must refuse a hollow MSI: WiX can exit 0 having packaged nothing.
	if !strings.Contains(ci, "-lt 50MB") {
		t.Error("the CI job does not check the MSI's size. WiX exits 0 for an empty package, so " +
			"a 40 KB MSI would pass every other check in the run.")
	}
}

// TestInstallPS1PrefersTheMSIWithoutBreakingUnattended — the one-liner should fire
// the installer, but `-Yes` and `-NoSetup` must keep the path they have.
func TestInstallPS1PrefersTheMSIWithoutBreakingUnattended(t *testing.T) {
	ps := readMSIFile(t, "..", "..", "scripts", "install.ps1")

	if !strings.Contains(ps, "_windows_${arch}.msi") {
		t.Fatal("install.ps1 never looks for a published MSI, so the one-line install would keep " +
			"hand-assembling files even once a release publishes a package")
	}
	// THE GUARD ON THE GUARD: the MSI branch must exclude every unattended case.
	// An MSI wizard cannot be driven by -Yes, and -NoSetup means "binaries only".
	for _, want := range []string{"-Yes", "-NoSetup", "ORCHICON_INSTALL_ARCHIVE"} {
		if !strings.Contains(ps, want) {
			t.Errorf("install.ps1's MSI branch does not account for %s — an unattended run would "+
				"be sent down the wizard path, which opens a console nothing can answer", want)
		}
	}
	// The exclusion must be a single expression, so one condition cannot be edited
	// without the others being visible next to it.
	if !strings.Contains(ps, "$useMsi = -not ($Yes -or $NoSetup -or $env:ORCHICON_INSTALL_ARCHIVE)") {
		t.Error("install.ps1's `$useMsi` is not the single stated exclusion of -Yes, -NoSetup and " +
			"the offline archive seam; a case-by-case edit is how one of them gets dropped")
	}
	// It must VERIFY what the MSI installed rather than trusting its exit code.
	if !strings.Contains(ps, "version 2>&1") {
		t.Error("install.ps1 does not run the installed binary to verify the MSI, so a package " +
			"that exits 0 without installing anything would be reported as success")
	}
}

// TestWiXVersionIsPinnedBelowSix — the build must not float into a licensing decision.
//
// WHAT HAPPENED. `dotnet tool install --global wix` with no --version resolved to v7,
// and the job died with
//
//	error WIX7015: You must accept the Open Source Maintenance Fee (OSMF) EULA to use
//	WiX Toolset v7.
//
// That is a LICENSING decision, and a build must never make one implicitly by floating
// to latest. It is pinned, and pinned below 6, in both the workflows and the Makefile —
// and this fails if the two ever disagree, which is how one of them silently starts
// resolving to a version the other does not use.
func TestWiXVersionIsPinnedBelowSix(t *testing.T) {
	want := wixPinnedVersion(t)

	// v6 introduced the fee, so anything >= 6 is not buildable without accepting it.
	var major int
	if _, err := fmt.Sscanf(strings.SplitN(want, ".", 2)[0], "%d", &major); err != nil {
		t.Fatalf("cannot parse the WiX version %q", want)
	}
	if major >= 6 {
		t.Errorf("WIX_VERSION is %q.\n"+
			"WiX v6+ requires accepting the Open Source Maintenance Fee EULA, so every build\n"+
			"fails with \"error WIX7015\". Pin a version below 6 and treat raising it as the\n"+
			"licensing decision it is.", want)
	}

	// ONE VERSION, EVERYWHERE. A workflow on a different version than the Makefile is
	// how a local build and CI start producing different packages.
	mk := readMSIFile(t, "..", "..", "Makefile")
	if !strings.Contains(mk, "WIX_VERSION    ?= "+want) {
		t.Errorf("the Makefile does not pin WIX_VERSION to %q", want)
	}
	for _, wf := range []string{"ci.yml", "release.yml"} {
		body := readMSIFile(t, "..", "..", ".github", "workflows", wf)
		if !strings.Contains(body, "dotnet tool install --global wix --version "+want) {
			t.Errorf("%s does not install wix --version %s.\n"+
				"An UNPINNED install resolves to v7 and fails the build; a differently-pinned one\n"+
				"produces a package the Makefile would not.", wf, want)
		}
		// And it must not ALSO contain an unpinned install that would win.
		if strings.Contains(body, "dotnet tool install --global wix\n") {
			t.Errorf("%s contains an UNPINNED `dotnet tool install --global wix`, which resolves "+
				"to v7 and fails with the OSMF EULA error", wf)
		}
	}
}

// wixPinnedVersion reads the version the Makefile pins.
func wixPinnedVersion(t *testing.T) string {
	t.Helper()
	mk := readMSIFile(t, "..", "..", "Makefile")
	for _, line := range strings.Split(mk, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "WIX_VERSION") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}
	t.Fatal("the Makefile declares no WIX_VERSION")
	return ""
}

// TestWiXBuildEnablesTheUIExtension — a .wxs that uses ui:WixUI cannot be built
// without it, and the failure is at build time on windows-latest:
//
//	error WIX0200: The Package element contains an unhandled extension element
//	'WixUI'. Please ensure that the extension for elements in the
//	'http://wixtoolset.org/schemas/v4/wxs/ui' namespace has been provided.
//
// The xmlns declaration makes the file WELL-FORMED and is not enough — the
// extension must be enabled on the command line. That pairing is asserted here so
// it cannot be dropped from one of the three places that build the MSI.
func TestWiXBuildEnablesTheUIExtension(t *testing.T) {
	body := readMSIFile(t, wxs())
	if !strings.Contains(body, "ui:WixUI") {
		t.Fatal("the .wxs no longer uses ui:WixUI, so this test's premise is gone")
	}
	for _, where := range []struct{ file string }{
		{filepath.Join("..", "..", ".github", "workflows", "ci.yml")},
		{filepath.Join("..", "..", ".github", "workflows", "release.yml")},
		{filepath.Join("..", "..", "Makefile")},
	} {
		b, err := os.ReadFile(where.file)
		if err != nil {
			t.Fatalf("read %s: %v", where.file, err)
		}
		if !strings.Contains(string(b), "-ext WixToolset.UI.wixext") {
			t.Errorf("%s builds the .wxs without `-ext WixToolset.UI.wixext`.\n"+
				"The .wxs uses ui:WixUI, which is an EXTENSION element: the xmlns makes the\n"+
				"file well-formed and does not enable it. The build fails with WIX0200.",
				filepath.Base(where.file))
		}
	}
}
