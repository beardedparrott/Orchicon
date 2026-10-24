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
	"regexp"
	"sort"
	"strconv"
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

// TestSetupRunsTheInstalledExeDirectly — the hand-off must be ONE process with ONE plain-word
// argument: no `cmd.exe`, no `start`, no wrapper script, no quoted arguments.
//
// THE HISTORY IS THE REASON, and every step of it was a quoting bug:
//
//	"[#SetupCmdFile]" "[INSTANCE]" "[RUNTIMEIMAGES]"  — RUNTIMEIMAGES is unset BY DESIGN (an
//	  empty default would skip the setup's own image prompt), so that token became an EMPTY
//	  QUOTED STRING. Crossing MSI's command-line building, `cmd /c` and `start`, a quote was
//	  eaten per hop and cmd was handed the wrapper's PATH as a COMMAND NAME:
//	      'C:\...\orchicon-setup.cmd"  "dev" "' is not recognized as an internal or external command
//	…then a bare `orchicon-setup.cmd` from the working directory: fixed the quoting, kept three
//	  processes and a file to get wrong.
//
// Now `FileRef` names the File row, so MSI writes the command line itself from the binary it
// just installed, and the only argument is `install --instance <name>` — which has no
// empty-value case.
func TestSetupRunsTheInstalledExeDirectly(t *testing.T) {
	body := stripXMLComments(readMSIFile(t, wxs()))

	ca := regexp.MustCompile(`<CustomAction Id="LaunchSetup"[\s\S]*?/>`).FindString(body)
	if ca == "" {
		t.Fatal("no LaunchSetup custom action — the setup hand-off is gone")
	}
	// It must run a FILE from the package, not a shell.
	if !strings.Contains(ca, `FileRef="OrchiconExeFile"`) {
		t.Error("LaunchSetup does not use FileRef=\"OrchiconExeFile\".\n" +
			"  FileRef makes MSI build the command line from the installed file itself; anything\n" +
			"  else means assembling a path, which is the class of bug this fixes.")
	}
	for _, forbidden := range []struct{ what, sub string }{
		{"a shell (cmd.exe)", "cmd.exe"},
		{"`start`", "start "},
		{"a wrapper script", "orchicon-setup.cmd"},
		{"a quoted path", "&quot;[#"},
		{"a path assembled from a directory", "[INSTALLFOLDER]orchicon"},
	} {
		if strings.Contains(ca, forbidden.sub) {
			t.Errorf("LaunchSetup still contains %s (%q):\n  %s\n"+
				"  Every historical failure here was this chain losing a quote. One process and\n"+
				"  one plain-word argument has no quoting to lose.", forbidden.what, forbidden.sub, ca)
		}
	}
	// AND IT NAMES THE INSTANCE LITERALLY (design of record, D2): the wizard no longer asks, so the
	// hand-off sets up `dev` — the one server a fresh machine gets.
	if !strings.Contains(ca, "--instance dev") {
		t.Errorf("LaunchSetup does not pass `--instance dev`:\n  %s\n"+
			"  The setup must know which server it is creating, and the wizard no longer asks.", ca)
	}
}

func TestSetupRelaunchesIntoItsOwnConsole(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "console_windows.go")

	for _, want := range []struct{ what, sub string }{
		{"it asks Windows for a NEW console", "CREATE_NEW_CONSOLE"},
		{"it re-executes this same binary", "os.Executable()"},
		{"it marks the child so it cannot relaunch for ever", "consoleRelaunchEnv"},
		{"it does not wait for the child (the installer must not hang)", "cmd.Process.Release()"},
	} {
		if !strings.Contains(src, want.sub) {
			t.Errorf("console_windows.go does not show that %s (%q)", want.what, want.sub)
		}
	}

	// DETACHED_PROCESS in the relaunch flags would silently produce NO window — the exact
	// failure it exists to prevent. It is legitimately used for the runtime DAEMON, so this
	// check is scoped to relaunchInOwnConsole's own body.
	start := strings.Index(src, "func relaunchInOwnConsole()")
	if start < 0 {
		t.Fatal("relaunchInOwnConsole is gone")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("could not delimit relaunchInOwnConsole")
	}
	block := src[start : start+end]
	if strings.Contains(block, "detachedProcess") || strings.Contains(block, "DETACHED_PROCESS") {
		t.Error("relaunchInOwnConsole sets DETACHED_PROCESS, which SUPPRESSES the console it is " +
			"asking for. CREATE_NEW_CONSOLE alone is the correct flag.")
	}
	if !strings.Contains(block, "CREATE_NEW_CONSOLE") {
		t.Error("relaunchInOwnConsole does not set CREATE_NEW_CONSOLE")
	}

	// AND THE INSTALL PATH MUST CALL IT, or none of the above ever runs.
	install := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")
	if !strings.Contains(install, "ensureInteractiveConsole(") {
		t.Error("runInstall never calls ensureInteractiveConsole — the setup would run " +
			"console-less inside the MSI and its prompts would go nowhere")
	}
}

// TestBurnBundleWrapsTheMSI — the setup EXPERIENCE a person runs.
//
// A bare MSI is the right unit of deployment (Intune, SCCM, Group Policy and `msiexec /qn` all
// speak it) but a poor experience for a human: it draws the wizard and then hands the
// interesting part — a long download with prompts — to a console it cannot report on. A Burn
// bundle owns the UI and chains the MSI, so progress and the log belong to the wizard.
//
// IT DOES NOT REPLACE THE MSI. Both artifacts are asserted, not just the new one.
func TestBurnBundleWrapsTheMSI(t *testing.T) {
	bundle := stripXMLComments(readMSIFile(t, "..", "..", "deploy", "desktop", "windows", "OrchiconSetup.wxs"))

	for _, want := range []struct{ what, sub string }{
		{"the MSI is chained in (the bundle wraps it, it does not replace it)", "<MsiPackage"},
		// THE MSI'S OWN UI IS NOT SHOWN, and in WiX v4+ that is the DEFAULT plus the only
		// option: `DisplayInternalUI` was REMOVED (v5 rejects it with WIX0004 — it "has
		// fundamentally changed and requires BootstrapperApplication support"). So the guard
		// asserts the ABSENCE of the attribute rather than its value, because its presence is
		// now a build error and its behaviour lives in the BA.
		{"the instance reaches the MSI (as the setup's default)", `<MsiProperty Name="INSTANCE"`},
		{"the instance reaches the MSI", `<MsiProperty Name="INSTANCE"`},
		{"the licence page uses the SAME RTF as the MSI", `LicenseFile="$(var.LicenseRtf)"`},
		{"a standard BA theme that can show RTF (the terms we actually ship)", `Theme="rtfLicense"`},
	} {
		if !strings.Contains(bundle, want.sub) {
			t.Errorf("OrchiconSetup.wxs does not show that %s (%q)", want.what, want.sub)
		}
	}

	// THE SETUP MUST ACTUALLY RUN. `SKIPSETUP=1` in the bundle is the defect this cost an
	// install to find: the MSI's last step never fired, so a fresh install finished in seconds,
	// started no plane, and left the desktop app opening on "the control plane is not reachable".
	// An installer that leaves the operator with binaries and a list of commands has not
	// installed the product.
	if strings.Contains(bundle, `Name="SKIPSETUP"`) {
		t.Error("OrchiconSetup.wxs passes SKIPSETUP to the MSI, which means the setup NEVER RUNS.\n" +
			"  That is how a fresh install came to finish in seconds with no plane and no prompts.\n" +
			"  The bundle must let the MSI's hand-off open its console and set the plane up.")
	}

	// AND THE REMOVED ATTRIBUTE MUST NOT COME BACK — it reads as the obvious way to do this and
	// fails the build six minutes into a Windows run.
	if strings.Contains(bundle, "DisplayInternalUI") {
		t.Error("OrchiconSetup.wxs sets DisplayInternalUI. WiX v4 REMOVED that attribute (v5 " +
			"rejects it: WIX0004 \"unexpected attribute\"). In v4+ the bootstrapper application " +
			"decides, and the standard BA does not show a package's internal UI.")
	}

	// THE INSTANCE VARIABLE MUST BE DECLARED. `[Instance]` in an MsiProperty resolves against
	// bundle variables, and an UNDECLARED one resolves to an empty string — so every install
	// would silently become whatever the MSI defaults to, whatever was asked for.
	if !strings.Contains(bundle, `<Variable Name="Instance"`) {
		t.Error("OrchiconSetup.wxs references [Instance] but never declares it. An undeclared " +
			"variable resolves to EMPTY, so the wizard's instance choice would be silently dropped.")
	}

	// THE UPGRADE CODE MUST DIFFER FROM THE MSI'S, or Windows treats the bundle as an upgrade of
	// the package and removes the files out from under it.
	msiBody := stripXMLComments(readMSIFile(t, wxs()))
	msiUpgrade := regexp.MustCompile(`UpgradeCode="([^"]+)"`).FindStringSubmatch(msiBody)
	bundleUpgrade := regexp.MustCompile(`UpgradeCode="([^"]+)"`).FindStringSubmatch(bundle)
	if msiUpgrade == nil || bundleUpgrade == nil {
		t.Fatal("could not read both UpgradeCodes")
	}
	if msiUpgrade[1] == bundleUpgrade[1] {
		t.Errorf("the bundle and the MSI share UpgradeCode %s. A bundle and a package are "+
			"different products to Windows; sharing one makes the bundle \"upgrade\" the MSI and "+
			"delete its files.", msiUpgrade[1])
	}

	// AND EVERY SITE THAT BUILDS IT MUST INSTALL THE BAL EXTENSION FIRST — `-ext` resolves
	// through WiX's own store, never NuGet, and Bal is a SEPARATE package from the UI extension
	// the MSI build already installs.
	for _, site := range []struct{ what, path string }{
		{"the Makefile", filepath.Join("..", "..", "Makefile")},
		{"ci.yml", filepath.Join("..", "..", ".github", "workflows", "ci.yml")},
		{"release.yml", filepath.Join("..", "..", ".github", "workflows", "release.yml")},
		{"canary.yml", filepath.Join("..", "..", ".github", "workflows", "canary.yml")},
	} {
		src := readMSIFile(t, site.path)
		for _, want := range []struct{ what, sub string }{
			{"builds the bundle", "OrchiconSetup.wxs"},
			// THE PACKAGE ID AND THE DLL NAME MUST MATCH. `wix extension add -g <id>/<ver>`
			// resolves `-ext <id>` to <id>/<ver>/wixext5/<id>.dll, so an id whose DLL is named
			// differently cannot be found — exactly how this failed first:
			//   error WIX0144: The extension 'WixToolset.Bal.wixext' could not be found.
			//          Checked paths: …\WixToolset.Bal.wixext\5.0.2\wixext5\WixToolset.Bal.wixext.dll
			// …while that package ships WixToolset.BootstrapperApplications.wixext.dll.
			{"names the bundle extension on the build", "-ext WixToolset.BootstrapperApplications.wixext"},
			{"installs it first (or -ext cannot resolve it)", "WixToolset.BootstrapperApplications.wixext/"},
		} {
			if !strings.Contains(src, want.sub) {
				t.Errorf("%s does not show that it %s (%q)", site.what, want.what, want.sub)
			}
		}
	}

	// THE MISMATCHED ID MUST NOT COME BACK. It LOOKS like the right package (it is the one
	// documented as "the Bal extension") and its DLL is named differently, so WiX reports
	// WIX0144 and the build dies six minutes in.
	for _, site := range []string{"Makefile", ".github/workflows/ci.yml",
		".github/workflows/canary.yml", ".github/workflows/release.yml"} {
		var body string
		if site == "Makefile" {
			body = readMSIFile(t, "..", "..", "Makefile")
		} else {
			body = readMSIFile(t, "..", "..", ".github", "workflows", filepath.Base(site))
		}
		if strings.Contains(body, "WixToolset.Bal.wixext") {
			t.Errorf("%s references WixToolset.Bal.wixext. That PACKAGE ships "+
				"WixToolset.BootstrapperApplications.wixext.dll, so `-ext WixToolset.Bal.wixext` "+
				"fails with WIX0144 (WiX looks for a DLL named after the id). Use the "+
				"BootstrapperApplications id.", site)
		}
	}

	// THE RELEASE MUST PUBLISH IT. A bundle built and left in the run is the "artifact built and
	// silently not published" failure the release ASSETS list exists to prevent.
	rel := readMSIFile(t, "..", "..", ".github", "workflows", "release.yml")
	if !strings.Contains(rel, "-name 'OrchiconSetup_*.exe'") {
		t.Error("the release ASSETS find does not include 'OrchiconSetup_*.exe' — the bundle would " +
			"be built and then silently left out of the release")
	}
}

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
	for _, wf := range []string{"ci.yml", "release.yml", "canary.yml"} {
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
		body := string(b)
		if !strings.Contains(body, "-ext WixToolset.UI.wixext") {
			t.Errorf("%s builds the .wxs without `-ext WixToolset.UI.wixext`.\n"+
				"The .wxs uses ui:WixUI, which is an EXTENSION element: the xmlns makes the\n"+
				"file well-formed and does not enable it. The build fails with WIX0200.",
				filepath.Base(where.file))
		}
		// AND IT MUST BE INSTALLED FIRST. `-ext` resolves a package name through WiX's
		// own extension store, NOT NuGet, so naming it is not enough:
		//   error WIX0144: The extension 'WixToolset.UI.wixext' could not be found.
		//                 Checked paths: WixToolset.UI.wixext
		// KEYED ON THE SUBCOMMAND, not on how the binary is invoked: the Makefile calls
		// $(WIX) rather than the literal name, so a "wix extension add" needle fails
		// against a correctly-wired Makefile.
		if !strings.Contains(body, "extension add -g WixToolset.UI.wixext") {
			t.Errorf("%s never runs `wix extension add -g WixToolset.UI.wixext`.\n"+
				"`-ext` does not restore from NuGet — it looks the package up in WiX's own\n"+
				"extension store, so the extension has to be installed before the build can\n"+
				"name it (WIX0144).", filepath.Base(where.file))
		}
	}
}

// TestMSIDeclaresNoDuplicateIdentifiers — WiX rejects a repeated Id outright, and the
// one that bit was a property the UI set already defines:
//
//	error WIX0091: Duplicate Property with identifier 'ARPNOMODIFY' found.
//
// That cost a full CI round (six minutes) to learn, on a runner, for something a
// parser can see instantly — which is the whole point of this file. The check is per
// ELEMENT TYPE, because that is how MSI scopes identity.
func TestMSIDeclaresNoDuplicateIdentifiers(t *testing.T) {
	body := readMSIFile(t, wxs())

	type node struct {
		XMLName xml.Name
		Id      string `xml:"Id,attr"`
	}
	var doc struct {
		XMLName xml.Name
		Nodes   []node `xml:",any"`
	}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Walk every element type that carries an Id, at any depth.
	seen := map[string]map[string]int{} // element -> id -> count
	var walk func(nodes []node)
	walk = func(nodes []node) {
		for _, n := range nodes {
			if n.Id != "" {
				if seen[n.XMLName.Local] == nil {
					seen[n.XMLName.Local] = map[string]int{}
				}
				seen[n.XMLName.Local][n.Id]++
			}
		}
	}
	_ = walk

	// encoding/xml's ",any" only reaches the top level, so do the sweep on the raw
	// text instead: every Id="..." occurrence, grouped by the tag that carries it.
	tagRE := regexp.MustCompile(`<([A-Za-z][\w.]*)[^>]*\sId="([^"]+)"`)
	counts := map[string]map[string]int{}
	for _, m := range tagRE.FindAllStringSubmatch(body, -1) {
		el, id := m[1], m[2]
		if counts[el] == nil {
			counts[el] = map[string]int{}
		}
		counts[el][id]++
	}
	if len(counts) == 0 {
		t.Fatal("found no Id attributes — the matcher is wrong and this test would pass vacuously")
	}

	var dups []string
	for el, ids := range counts {
		for id, n := range ids {
			// CONTROL IDs ARE SCOPED PER DIALOG. Every dialog has its own Back/Next/Cancel/
			// Title/Description, so finding those three times is correct — WiX keys Control by
			// (Dialog_, Control), not by Control alone. Only the globally-unique identifiers are
			// a duplicate at ×2.
			if n > 1 && el != "Control" {
				dups = append(dups, fmt.Sprintf("%s/@%s ×%d", el, id, n))
			}
		}
	}
	sort.Strings(dups)
	if len(dups) > 0 {
		t.Errorf("Orchicon.wxs declares duplicate identifiers: %v\n"+
			"WiX rejects these outright (WIX0091) — and one of them was a Property the\n"+
			"WixUI_InstallDir set already defines (ARPNOMODIFY), so declaring it here was both\n"+
			"redundant and fatal.", dups)
	}
}

// TestCanaryPublishesTheWindowsDesktopApp — CANARY IS THE DEVELOP INSTALL PATH, and for a
// Windows operator "the installer" now includes the desktop app.
//
// WHY THIS JOB HAD TO EXIST. Canary published the CLI and the TUI only, so the sole place an
// MSI had ever been built was a pull-request CI run: an artifact that expires, hangs off a
// branch that is then deleted, and is therefore not an install path an operator can be handed.
// Canary exists so a REAL installer is exercised against DEVELOP before anything is released.
//
// THE ASSERTIONS ARE KEYED ON THE WIRING LINES, NOT ON THE NAMES. That distinction is not
// pedantry: the publish NOTES in the same file list `orchicon_canary_windows_amd64.msi`, so a
// guard that merely looked for that string would still pass after the build step lost it — a
// trap this file's sibling `errfix` controls hit, where a deleted component still matched
// because its name survived in a comment. Each case below therefore carries a COUNT, and the
// count is what fails when the wiring goes.
func TestCanaryPublishesTheWindowsDesktopApp(t *testing.T) {
	canary := readMSIFile(t, "..", "..", ".github", "workflows", "canary.yml")
	release := readMSIFile(t, "..", "..", ".github", "workflows", "release.yml")

	// THE ASSET NAMES ARE A CROSS-FILE CONTRACT: install.ps1 BUILDS the name it will fetch and
	// canary.yml must publish exactly that. Asserted against install.ps1's own templates rather
	// than against a literal copied here, so a rename on EITHER side fails this test instead of
	// silently producing a published installer that no install path resolves.
	installPS1 := readMSIFile(t, "..", "..", "scripts", "install.ps1")
	for _, tpl := range []struct{ sub, why string }{
		{`$msiAsset = "orchicon_${versionNum}_windows_${arch}.msi"`,
			"the MSI name the interactive install probes for (install.ps1 -Version canary)"},
		{`$stem = "orchicon_desktop_$($ver -replace '^v','')_windows_amd64"`,
			"the desktop installer stem install.ps1 -Desktop resolves"},
	} {
		if !strings.Contains(installPS1, tpl.sub) {
			t.Errorf("install.ps1 no longer builds %s.\n"+
				"  looked for: %s\n"+
				"  That template is %s — if it moved, this guard and canary.yml's asset names\n"+
				"  must move with it, or the canary publishes an installer nothing fetches.",
				tpl.why, tpl.sub, tpl.why)
		}
	}

	for _, want := range []struct {
		what  string
		sub   string
		count int
	}{
		{"the desktop job itself", "\n  desktop-windows:\n", 1},
		{"the MSI build step", "wix build deploy/desktop/windows/Orchicon.wxs", 1},
		// install.ps1 probes `orchicon_<version>_windows_<arch>.msi` with version = "canary",
		// so the published file must carry the WORD canary, not a run number.
		{"the MSI output name install.ps1 probes", `-o "dist/desktop/orchicon_canary_windows_amd64.msi"`, 1},
		{"the NSIS installer name install.ps1 -Desktop resolves",
			`"dist/desktop/orchicon_desktop_canary_windows_amd64_installer.exe"`, 1},
		{"the portable desktop zip",
			`"dist/desktop/orchicon_desktop_canary_windows_amd64.zip"`, 1},
		// BOTH publish paths, or a canary that creates the release carries the desktop app and
		// one that updates it does not — the two diverge on the second merge.
		{"the desktop assets on the UPLOAD path",
			"gh release upload canary dist/*.tar.gz dist/*.zip dist/*.msi dist/*.exe --clobber", 1},
		{"the desktop assets on the CREATE path",
			"dist/*.tar.gz dist/*.zip dist/*.msi dist/*.exe", 2},
		// The publish job's download pattern is `canary-*`; an artifact named anything else is
		// built, uploaded, and then never reaches the release.
		{"an artifact name the publish pattern collects", "name: canary-windows-desktop", 1},
		// TWO now: desktop-windows and desktop-macos both upload from dist/desktop.
		{"the desktop artifact upload paths", "path: dist/desktop/*", 2},
	} {
		if n := strings.Count(canary, want.sub); n != want.count {
			t.Errorf("canary.yml: %s appears %d time(s), want %d — %q.\n"+
				"  Without it the desktop app is either not built, or built and never published, or\n"+
				"  published under a name the installer does not resolve.",
				want.what, n, want.count, want.sub)
		}
	}

	// THE PUBLISH JOB MUST WAIT FOR THE DESKTOP BUILD. `gh release upload --clobber` replaces
	// matching names and leaves everything else, so a publish that ran while the desktop job
	// failed would keep the PREVIOUS merge's installer under a tag claiming this commit.
	// A canary that lies about its own contents is worse than no canary.
	// (the dependency is asserted as a SET elsewhere in this test; this older literal is gone
	// because macOS joined the list)

	// THE MSI VERSION MUST BE NUMERIC AND MUST INCREASE.
	//
	// Numeric because an MSI ProductVersion is major.minor.build — "canary" is rejected outright.
	// Increasing because the .wxs declares <MajorUpgrade>: with a fixed 0.0.0 every canary would be
	// the SAME product version, so reinstalling after the next merge would not upgrade anything —
	// Windows would report "already installed" or leave the previous build in place, which is
	// precisely the re-test canary exists to make possible. github.run_number is monotonic per
	// workflow and stays far below WiX's 65535 ceiling for the build field.
	for _, want := range []struct{ what, sub string }{
		{"a NUMERIC MSI version", `-d "Version=0.0.${{ github.run_number }}"`},
		{"the Wails CLI pinned to the release's version", "wails@v2.17.0"},
		{"the WebView2 bootstrapper (so Windows 10 provisions itself)", "-webview2 download"},
	} {
		if !strings.Contains(canary, want.sub) {
			t.Errorf("canary.yml is missing %s (%q)", want.what, want.sub)
		}
	}
	for _, bad := range []string{
		`-d "Version=canary"`, // not a valid ProductVersion: WiX refuses it
		`-d "Version=0.0.0"`,  // fixed: <MajorUpgrade> then never upgrades across merges
	} {
		if strings.Contains(canary, bad) {
			t.Errorf("canary.yml passes %q to wix build.\n"+
				"  An MSI ProductVersion must be numeric AND must increase, or successive canaries\n"+
				"  collide: 'already installed', or the previous build left in place.", bad)
		}
	}

	// THE WAILS PIN MUST MATCH THE RELEASE'S, for the same reason the WiX pin must: a canary built
	// by a different Wails produces an app packaged by a different toolchain than the one that ships.
	if !strings.Contains(release, "wails@v2.17.0") {
		t.Error("release.yml no longer installs wails@v2.17.0 — if the release moved, canary.yml must " +
			"move with it or the two build the desktop app with different toolchains")
	}
}

// TestEveryNSISBuildInstallsMakensis — `-nsis` is a request to RUN makensis, and makensis is
// not on the windows-latest image.
//
// Wails does not fail without it. It prints
//
//	Warning: Cannot create installer: makensis not found
//
// and EXITS 0, so the build step goes green and the installer simply is not produced. That is
// a green build which shipped nothing — the same silent-degradation shape as the frontend
// `.gitkeep` embed, and it is exactly how this was found: the canary's first run of `-nsis`
// (the first run of it in the project's history) failed one step later, in the naming step,
// with "no NSIS installer found". release.yml carried the identical defect and had never run
// because windows/amd64 joined the matrix on 2026-10-18 and no tag has been cut since.
//
// THE TRIGGER IS A LINE THAT ACTUALLY RUNS IT, not the string "-nsis" anywhere in the file.
//
// Two rounds of tightening got here, both forced by a control that did not bite:
//
//  1. containment (`strings.Contains(body, "-nsis")`) PASSED after the build flag was deleted,
//     because these workflows now discuss makensis at length in comments. So the scan went
//     line-wise and asked for `wails build` and `-nsis` on the same line.
//  2. THAT STILL PASSED, because the assert step's own error message —
//     echo "error: wails build -nsis produced no installer ..."
//     — contains both tokens. A detector defeated by the diagnostic it prints.
//
// So the line must BEGIN with the command. `wails build …` is an invocation; `echo "… wails
// build -nsis …"` is prose that happens to quote one, and a `#` comment is prose outright.
func nsisBuildLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "wails build") {
			continue
		}
		if strings.Contains(trimmed, "-nsis") {
			out = append(out, trimmed)
		}
	}
	return out
}

// TestEveryNSISBuildInstallsMakensis: EVERY workflow that asks for `-nsis` must also install
// NSIS, put it on PATH for the build step, and assert the artifact came out. The third part is
// not redundant with the first two — a makensis that is present and still emits nothing is the
// same failure.
func TestEveryNSISBuildInstallsMakensis(t *testing.T) {
	workflows := []string{"ci.yml", "release.yml", "canary.yml"}
	builds := map[string]bool{}
	for _, wf := range workflows {
		body := readMSIFile(t, "..", "..", ".github", "workflows", wf)
		if len(nsisBuildLines(body)) == 0 {
			continue // this workflow does not build an NSIS installer
		}
		builds[wf] = true
		for _, want := range []struct{ what, sub string }{
			{"installs NSIS (makensis is not on the runner image)",
				"choco install nsis"},
			{"puts makensis on PATH for the BUILD step (an installer's PATH change does not " +
				"reach the current session)", `C:\Program Files (x86)\NSIS`},
			{"fails loudly when makensis is missing (Wails only WARNS and exits 0)",
				"command -v makensis"},
			{"asserts the installer actually came out, not just the exit code",
				"*installer*.exe"},
		} {
			if !strings.Contains(body, want.sub) {
				t.Errorf("%s runs `-nsis` but %s.\n"+
					"  looked for: %s\n"+
					"  Without it the job goes GREEN with no installer in it: Wails prints\n"+
					"  \"Warning: Cannot create installer: makensis not found\" and exits 0.",
					wf, want.what, want.sub)
			}
		}
	}
	// CI CARRIES THE COVERAGE CLAIM. Release and canary run on a tag and on a merge to
	// develop; a pull request — where this class of fault should be found — only runs CI.
	// If ci.yml stops building the installer, the next makensis-class regression first
	// surfaces on a merge to develop, which is how this one surfaced.
	if !builds["ci.yml"] {
		t.Error("ci.yml no longer runs `wails build ... -nsis`, so no pull request exercises the " +
			"NSIS installer. Release and canary are the only remaining runs, i.e. the fault " +
			"first surfaces after it has been merged.")
	}
	if len(builds) == 0 {
		t.Error("no workflow runs `-nsis` — if the NSIS installer was dropped deliberately, " +
			"delete this test and the Install NSIS steps with it; if not, the installer is gone")
	}
}

// rtfPlainText reduces an RTF file to its visible text so a test can compare it with a plain
// source. Deliberately small: drop the font/colour tables by NAME, turn the two line controls
// into newlines, strip the remaining control words, and unescape what RTF escapes.
//
// NOT a general group stripper, and that matters: a loop that removes {…} groups one layer at a
// time removes the DOCUMENT group last and takes every character with it — which reads as "the
// RTF is empty" and would make this guard fail on a correct file. The tables are named instead,
// because they are the only groups that must go.
func rtfPlainText(src string) string {
	s := strings.ReplaceAll(src, `\par`, "\n")
	s = strings.ReplaceAll(s, `\line`, "\n")
	// Escaped literals, before the control-word pass can see them.
	s = strings.ReplaceAll(s, `\\`, "\x00")
	s = strings.ReplaceAll(s, `\{`, "{")
	s = strings.ReplaceAll(s, `\}`, "}")
	// The groups that carry no visible text, removed whole (they contain no nested braces).
	for _, table := range []string{`\fonttbl`, `\colortbl`, `\*`} {
		re := regexp.MustCompile(`(?s)\{` + regexp.QuoteMeta(table) + `[^{}]*\}`)
		for {
			n := re.ReplaceAllString(s, "")
			if n == s {
				break
			}
			s = n
		}
	}
	// Any other control word (\fs20, \uc1, \b, \pard …), with an optional signed parameter.
	s = regexp.MustCompile(`\\[a-zA-Z]+-?[0-9]*`).ReplaceAllString(s, "")
	// Structural braces, then the backslash placeholder.
	s = strings.ReplaceAll(s, "{", "")
	s = strings.ReplaceAll(s, "}", "")
	s = strings.ReplaceAll(s, "\x00", `\`)
	return s
}

// stripXMLComments removes <!-- … --> so a guard can look for an ELEMENT without matching the
// prose that explains why that element is wrong.
//
// THIS IS NOT PEDANTRY. The Orchicon.wxs now carries a comment quoting the exact
// <Property Id="WixUILicenseRtf" …/> element this file forbids; a guard that scanned the raw
// text matched the comment and failed a correct file. That is the third time in this file's
// history a check was satisfied (or defeated) by prose, so comment-stripping is the default.
// stripBatchComments removes REM/:: comment lines so an assertion about what a .cmd
// DOES is not satisfied by a comment describing it.
//
// THIS IS NOT HYPOTHETICAL: the guard for the silent registry read looked for `2^>nul`
// in the whole file, and it stayed green after that token was deleted from the `reg
// query` line — because the comment explaining it still said `2^>nul`. A detector
// satisfied by prose is a detector that has stopped detecting, which by now is this
// file's most-repeated lesson.
func stripBatchComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(trimmed, "REM ") || strings.HasPrefix(trimmed, "REM\t") || trimmed == "REM" ||
			strings.HasPrefix(trimmed, "::") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func stripXMLComments(s string) string {
	re := regexp.MustCompile(`(?s)<!--.*?-->`)
	for {
		n := re.ReplaceAllString(s, "")
		if n == s {
			return s
		}
		s = n
	}
}

// collapseSpace makes two texts comparable regardless of where either wrapped.
func collapseSpace(s string) string {
	// The license file uses a dash where the RTF needs plain ASCII, and a non-breaking
	// space survives copy-paste from a browser. Normalising BOTH sides means the guard
	// compares words, not byte-for-byte typography.
	s = strings.ReplaceAll(s, "\u2014", "-")
	s = strings.ReplaceAll(s, "\u2013", "-")
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.Join(strings.Fields(s), " ")
}

// TestMSILicensePageCarriesOurLicenseText — the wizard's license page must show OUR terms
// with OUR information on it.
//
// TWO DEFECTS, reported together from a real install:
//
//  1. THE PAGE SHOWED LOREM IPSUM. `WixUILicenseRtf` is a BUILD-TIME variable, not an MSI
//     Property. Declaring <Property Id="WixUILicenseRtf" Value="..."/> looks right and does
//     nothing, so the UI extension kept its built-in placeholder. Every build site must pass
//     `-d WixUILicenseRtf=...`.
//  2. THE RTF SAID APACHE 2.0. The repository is NOT Apache-licensed — LICENSE is a custom
//     non-commercial grant. A wizard page that misstates the terms is worse than a placeholder,
//     because it looks authoritative.
//
// So this asserts three things: the build sites wire the variable, the RTF carries no
// foreign license, and every line of LICENSE actually appears on the page.
func TestMSILicensePageCarriesOurLicenseText(t *testing.T) {
	// (1) EVERY SITE THAT BUILDS THE MSI MUST PASS IT AS A BIND VARIABLE.
	//
	// THREE WAYS TO GET THIS WRONG, and this fix went through all three:
	//   <Property Id="WixUILicenseRtf" .../>   an MSI property — the UI extension never reads it
	//   -d WixUILicenseRtf=...                 a PREPROCESSOR variable — also never read, because
	//                                          the dialog declares !(wix.WixUILicenseRtf=...), a
	//                                          BIND variable; the build succeeds, silently keeping
	//                                          the placeholder. Measured: the MSI still contained
	//                                          "Lorem ipsum" after the -d version shipped.
	//   -bindvariable WixUILicenseRtf=...           correct — what WiX's own UI tests use
	//                                          -bindvariable only, NOT -bindvar, and
	//                                          WiX's own UI test uses -bv.
	for _, site := range []struct{ what, sub string }{
		{"the Makefile", "-bindvariable WixUILicenseRtf=$(abspath $(MSI_SRC_DIR))/LICENSE.rtf"},
		{"release.yml", `-bindvariable "WixUILicenseRtf=$srcAbs\LICENSE.rtf"`},
		{"ci.yml", `-bindvariable "WixUILicenseRtf=$srcAbs\LICENSE.rtf"`},
		{"canary.yml", `-bindvariable "WixUILicenseRtf=$srcAbs\LICENSE.rtf"`},
	} {
		var body string
		if site.what == "the Makefile" {
			body = readMSIFile(t, "..", "..", "Makefile")
		} else {
			body = readMSIFile(t, "..", "..", ".github", "workflows", site.what)
		}
		if !strings.Contains(body, site.sub) {
			t.Errorf("%s does not pass %s.\n"+
				"  WixUILicenseRtf is a BUILD-TIME variable, not a Property: without it the wizard\n"+
				"  silently renders WiX's lorem-ipsum placeholder. That is the reported bug.", site.what, site.sub)
		}
	}

	// (1b) THE TWO NO-OP FORMS MUST NOT COME BACK. Both compile, both look right, neither
	// reaches the dialog — so neither fails until a user opens the wizard.
	for _, site := range []string{"Makefile", ".github/workflows/release.yml",
		".github/workflows/ci.yml", ".github/workflows/canary.yml"} {
		var body string
		if site == "Makefile" {
			body = readMSIFile(t, "..", "..", "Makefile")
		} else {
			body = readMSIFile(t, "..", "..", ".github", "workflows", filepath.Base(site))
		}
		if regexp.MustCompile(`-d\s+"?WixUILicenseRtf=`).MatchString(body) {
			t.Errorf("%s passes WixUILicenseRtf with -d.\n"+
				"  -d defines a PREPROCESSOR variable; the license page declares a BIND variable\n"+
				"  (!(wix.WixUILicenseRtf=...)), so -d is silently ignored and the wizard keeps\n"+
				"  WiX's lorem-ipsum placeholder. Use -bindvariable (or -bv).", site)
		}
	}

	// (2) ... AND THE PROPERTY FORM MUST NOT COME BACK.
	wxsBody := stripXMLComments(readMSIFile(t, wxs()))
	if regexp.MustCompile(`<Property\s+Id="WixUILicenseRtf"`).MatchString(wxsBody) {
		t.Error("Orchicon.wxs declares <Property Id=\"WixUILicenseRtf\"> again. That does NOTHING —\n" +
			"  the UI extension reads a build-time variable and falls back to a lorem-ipsum\n" +
			"  placeholder when it is unset, which is exactly the reported symptom.")
	}

	// (3) NOT APACHE. The repository's LICENSE is a custom non-commercial grant.
	rtfRaw := readMSIFile(t, "..", "..", "deploy", "desktop", "windows", "LICENSE.rtf")
	page := collapseSpace(rtfPlainText(rtfRaw))
	for _, foreign := range []string{"apache license", "apache.org/licenses", "version 2.0"} {
		if strings.Contains(strings.ToLower(page), foreign) {
			t.Errorf("the wizard's license page mentions %q. Orchicon is NOT Apache-licensed:\n"+
				"  see ./LICENSE. A page that misstates the terms looks authoritative and is worse\n"+
				"  than the placeholder it replaced.", foreign)
		}
	}

	// (4) EVERY LINE OF LICENSE IS ON THE PAGE, so the two cannot drift.
	lic := collapseSpace(readMSIFile(t, "..", "..", "LICENSE"))
	for _, line := range strings.Split(lic, ". ") {
		line = strings.TrimSpace(line)
		if len(line) < 12 {
			continue // a fragment, not a claim
		}
		if !strings.Contains(page, line) {
			t.Errorf("deploy/desktop/windows/LICENSE.rtf does not carry this LICENSE text:\n  %q\n"+
				"  The wizard page is the only place a user is shown the terms, so it must be the\n"+
				"  same terms. Regenerate the RTF from ./LICENSE.", line)
		}
	}

	// (5) AND IT IS ACTUALLY INFORMATIVE, which is the other half of the report ("we should put
	// some kind of actual information in there"). A page that is only legalese tells the
	// operator nothing about what they just installed.
	// EVERY BINARY BY NAME, not one of them: a first cut asserted only "orchicon.exe", so
	// deleting the whole inventory still passed — the word survives elsewhere on the page.
	// A section that is meant to tell the operator what they just installed is only tested by
	// requiring what it lists.
	for _, want := range []struct{ what, sub string }{
		{"the control plane binary", "orchicon.exe"},
		{"the TUI binary", "orch.exe"},
		{"the desktop binary", "orchicon-desktop.exe"},
		{"that it is per-user / needs no administrator", "administrator"},
		{"where setup continues", "orchicon install"},
		{"that setup is idempotent and resumable", "idempotent"},
		{"where the data lives", "data"},
		{"WebView2 (the runtime the desktop app needs)", "WebView2"},
		{"the authoritative copy of the terms", "LICENSE"},
	} {
		if !strings.Contains(page, want.sub) {
			t.Errorf("the wizard's license page does not mention %s (%q) — it should say what was\n"+
				"  installed and what happens next, not only the terms.", want.what, want.sub)
		}
	}
}

// TestSetupHandoffCarriesNoDataThroughTheShell — the Finish-page hand-off must put
// NOTHING on the command line except a title and a bare filename.
//
// THE REPORT: the installer finished and opened a console saying
//
//	'C:\Users\justi\AppData\Local\Programs\Orchicon\orchicon-setup.cmd"  "dev" "'
//	is not recognized as an internal or external command
//
// The action used to end with  "[#SetupCmdFile]" "[INSTANCE]" "[RUNTIMEIMAGES]".
// RUNTIMEIMAGES has no default — it must be UNSET rather than "none", or the setup's own
// runtime-image prompt is skipped — so that last token resolved to an EMPTY QUOTED STRING.
// The command then crossed MSI's command-line building, `cmd /c` and `start`, and a quote
// was eaten at each end: cmd was handed the PATH as a COMMAND NAME.
//
// Three shells is too many to thread quoted data through, so none is threaded. The
// wizard's answers go into the registry (see TestSetupConfigReachesTheWrapper) and the
// wrapper is named bare, in the working directory Directory="INSTALLFOLDER" already sets.
func TestWizardArtIsOurs(t *testing.T) {
	body := stripXMLComments(readMSIFile(t, wxs()))

	for _, want := range []struct {
		varID string
		w, h  int
		file  string
	}{
		// THESE ARE THE MSI WIZARD'S OWN CONTROL SIZES, not the Burn theme's. They were 493x58 /
		// 493x312 — the sizes the BURN standard BA draws into, which is a DIFFERENT wizard — so
		// the artwork was being scaled into a smaller box and looked squashed and misaligned:
		//
		//   LicenseAgreementDlg: <Control Id="BannerBitmap" Width="370" Height="44" />
		//   WelcomeDlg:         <Control Id="Bitmap"       Width="370" Height="234" />
		{"WixUIBannerBmp", 370, 44, "wizard-banner.bmp"},
		{"WixUIDialogBmp", 370, 234, "wizard-dialog.bmp"},
	} {
		// 1. the .wxs points at it
		if !strings.Contains(body, `<WixVariable Id="`+want.varID+`"`) {
			t.Errorf("Orchicon.wxs does not set %s — WiXUI falls back to its own red/black "+
				"default art, which is what the operator saw (and saw flicker).", want.varID)
			continue
		}
		// 2. the file exists, is a real BMP, and is the size the dialog draws into
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "desktop", "windows", want.file))
		if err != nil {
			t.Errorf("missing %s: %v", want.file, err)
			continue
		}
		if len(raw) < 54 || raw[0] != 'B' || raw[1] != 'M' {
			t.Errorf("%s is not a BMP (a Windows bitmap starts with the bytes 'BM')", want.file)
			continue
		}
		w := int(raw[18]) | int(raw[19])<<8 | int(raw[20])<<16 | int(raw[21])<<24
		h := int(raw[22]) | int(raw[23])<<8 | int(raw[24])<<16 | int(raw[25])<<24
		if h < 0 {
			h = -h // a top-down BMP stores a negative height
		}
		if w != want.w || h != want.h {
			t.Errorf("%s is %dx%d; the dialog draws %s into %dx%d and will stretch anything else",
				want.file, w, h, want.varID, want.w, want.h)
		}
		// 3. it is 24-bit, not a palettised or alpha bitmap: WiX renders an alpha BMP on
		//    an unpredictable background.
		if bits := int(raw[28]) | int(raw[29])<<8; bits != 24 {
			t.Errorf("%s is %d bits per pixel, want 24 (an alpha BMP renders on an "+
				"unpredictable background)", want.file, bits)
		}
	}

	// 4. EVERY BUILD SITE MUST STAGE THEM. WixVariable resolves through $(var.SourceDir),
	//    so an unstaged file is a build error on windows-latest — six minutes away.
	for _, site := range []struct{ what, path string }{
		{"the Makefile", filepath.Join("..", "..", "Makefile")},
		{"ci.yml", filepath.Join("..", "..", ".github", "workflows", "ci.yml")},
		{"release.yml", filepath.Join("..", "..", ".github", "workflows", "release.yml")},
		{"canary.yml", filepath.Join("..", "..", ".github", "workflows", "canary.yml")},
	} {
		src := readMSIFile(t, site.path)
		for _, f := range []string{"wizard-banner.bmp", "wizard-dialog.bmp"} {
			if !strings.Contains(src, f) {
				t.Errorf("%s does not stage %s into the MSI source dir. WixVariable resolves it "+
					"through $(var.SourceDir), so the build fails on windows-latest instead of here.",
					site.what, f)
			}
		}
	}
}

// TestWindowsReadmeDoesNotPromiseAPlaneItCannotStart — the shipped README claimed the desktop
// app "starts one for you if none is running". It does not: cmd/orchicon-desktop has no
// plane-starting code at all. An operator reads that file precisely when something is wrong, so
// a promise in it is not marketing — it is the thing they will act on, and it sent one of them
// looking at the app instead of at the plane.
//
// Asserted on the FILE because the claim is prose: no unit test of the shell can see it, and
// the false sentence survived several releases.
func TestWindowsReadmeDoesNotPromiseAPlaneItCannotStart(t *testing.T) {
	readme := readMSIFile(t, "..", "..", "deploy", "desktop", "windows", "README-WINDOWS.txt")

	if regexp.MustCompile(`(?i)starts one for\s+you|starts a plane (for you )?if`).MatchString(readme) {
		t.Error("README-WINDOWS.txt promises the desktop app starts a plane. It does not — there is " +
			"no plane-starting code in cmd/orchicon-desktop. Name the command instead " +
			"(orchicon install / orchicon start).")
	}
	// And the honest version must actually be there, or deleting the sentence would pass.
	for _, want := range []struct{ what, sub string }{
		{"that it does not start one", "DOES NOT START ONE"},
		{"how to install/serve one", "orchicon install"},
		{"how to start one later", "orchicon start"},
	} {
		if !strings.Contains(readme, want.sub) {
			t.Errorf("README-WINDOWS.txt does not say %s (%q) — the app cannot work without a plane, "+
				"and this file is what an operator reads when it does not.", want.what, want.sub)
		}
	}

	// AND THE SAME CLAIM WAS IN THE MAIN DOCS, WHICH THIS GUARD DID NOT COVER.
	//
	// The sentence "if none is running it starts one for you — which takes a few seconds on
	// first launch" appeared verbatim in README.md and USERGUIDE.md while nothing in
	// internal/desktop or cmd/orchicon-desktop starts a plane. Fixing only the file this test
	// watched is exactly how a guard gives false comfort, so the sweep now covers every doc a
	// user is likely to read first.
	for _, doc := range []string{"README.md", "USERGUIDE.md"} {
		body := readMSIFile(t, "..", "..", doc)
		if regexp.MustCompile(`(?i)starts one for\s+you|starts a plane (for you )?if`).MatchString(body) {
			t.Errorf("%s promises the desktop app starts a plane. It does not — there is no "+
				"plane-starting code in cmd/orchicon-desktop. Name the command instead "+
				"(orchicon install / orchicon start).", doc)
		}
	}
}

// TestInstallerKeepsTheMSILog — the one record of what Windows Installer did must survive the run.
//
// install.ps1 wrote the MSI log into its scratch directory and deleted that directory seconds
// later, so a report of a wizard failure arrived with NOTHING to read. That was felt directly:
// "orchicon-setup.cmd could not be found" came with no log, and the question "did the file fail
// to install, or was it launched wrongly?" could not be answered from the evidence.
func TestInstallerKeepsTheMSILog(t *testing.T) {
	ps1 := readMSIFile(t, "..", "..", "scripts", "install.ps1")
	if !strings.Contains(ps1, `"/l*v", "`+"`"+`"$msiLog`+"`"+`""`) {
		t.Error("install.ps1 no longer passes /l*v $msiLog to msiexec — the MSI log is how a wizard " +
			"failure is diagnosed")
	}
	if !strings.Contains(ps1, `GetTempPath()) "orchicon-msi.log"`) {
		t.Error("the MSI log is not written to a STABLE path. It used to live in $tmpdir, which is " +
			"deleted at the end of the run — so the log explaining a failure was thrown away with it.")
	}
	if strings.Contains(ps1, `Join-Path $tmpdir 'msi.log'`) {
		t.Error("the MSI log is written into $tmpdir again — that directory is deleted before the " +
			"operator can read anything in it")
	}
	if !strings.Contains(ps1, "Windows Installer log: $msiLog") {
		t.Error("the MSI log path is not PRINTED — a path nobody is shown is a path nobody sends")
	}
}

// TestCIInstallsTheMSIPackage — the package had only ever been BUILT, so its first real run was
// on an operator's machine, where the wizard's last step could not find orchicon-setup.cmd.
//
// Building it proves the .wxs compiles and the cab is populated. It does not prove InstallFiles
// puts the files where the custom action looks — only running `msiexec /i` proves that.
//
// THE ASSERTIONS ARE SCOPED TO THE STEP, and that is not tidiness. Checking the whole file first
// let TWO of these controls pass after their flag was deleted, because the sibling UNINSTALL step
// uses the same tokens ('/qn', and the path): the guard was satisfied by a different occurrence
// than the one doing the installing. A file-wide containment check cannot tell those apart, so
// this one slices the step out by name and asserts inside it.
func TestCIInstallsTheMSIPackage(t *testing.T) {
	ci := readMSIFile(t, "..", "..", ".github", "workflows", "ci.yml")
	step := workflowStep(t, ci, "Install the MSI per-user")
	for _, want := range []struct{ what, sub string }{
		{"installs the package", "'/i',"},
		{"unattended, so it cannot block on a wizard", "'/qn',"},
		{"with the setup hand-off suppressed (it would download ~540 MB)", "'SKIPSETUP=1'"},
		// THE FILE THE WIZARD'S LAST STEP RUNS. It used to be the orchicon-setup.cmd wrapper;
		// the hand-off now uses FileRef="OrchiconExeFile" and runs orchicon.exe itself, so the
		// wrapper is gone (see TestSetupRunsTheInstalledExeDirectly) and asserting it would
		// assert a file that must not exist.
		{"verifies the binary the hand-off runs, by name", "'orchicon.exe'"},
		{"fails the job when the install fails", "ExitCode -ne 0"},
	} {
		if !strings.Contains(step, want.sub) {
			t.Errorf("the CI step \"Install the MSI per-user\" is missing %s (%q).\n"+
				"  Until this step existed the MSI was only ever BUILT; the first time it was\n"+
				"  actually installed was on an operator's Windows box, which is how the missing\n"+
				"  orchicon-setup.cmd reached a user before it reached us.", want.what, want.sub)
		}
	}
	// AND IT IS UNINSTALLED, in its own step, so the runner is left clean and the uninstall
	// path is exercised too.
	un := workflowStep(t, ci, "Uninstall the MSI")
	for _, want := range []string{"'/x',", "orchicon.exe"} {
		if !strings.Contains(un, want) {
			t.Errorf("the CI uninstall step does not mention %q — the uninstall path is as much a "+
				"part of the package as the install, and nothing else exercises it", want)
		}
	}
}

// workflowStep extracts one named step's text (from its `- name:` to the next step), so an
// assertion lands on the step that does the work rather than on any other step that happens to
// share a token.
func workflowStep(t *testing.T, workflow, namePrefix string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, "- name:") && strings.Contains(line, namePrefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no workflow step named %q — if it was renamed, this test must be updated "+
			"with it", namePrefix)
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if strings.HasPrefix(trimmed, "- name:") || strings.HasPrefix(trimmed, "- uses:") {
			end = j
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestWorkflowPwshStepsParse — every `shell: pwsh` block in the workflows must PARSE.
//
// A POWERSHELL PARSER ERROR IS NOT A TEST FAILURE, IT IS A JOB FAILURE, and it fails the whole
// script before a single line of it runs. That is how this guard came to exist: the CI step that
// installs the MSI ended with
//
//	Write-Host "installed payload verified in $dir:"
//
// and PowerShell read `$dir:` as a SCOPE/DRIVE-qualified variable — `$env:` and `$script:` are
// legitimate, so the form looks entirely ordinary — and refused to parse the file:
//
//	ParserError: Variable reference is not valid. ':' was not found after the variable name.
//
// The install never ran. The step failed in one second on a line that had already been reviewed
// and read perfectly.
//
// This is a TOKENISER, not a linter: it reuses the same rule PowerShell applies (a `$name:`
// reference must name a real drive or scope), so it can run here with no pwsh on the runner.
func TestWorkflowPwshStepsParse(t *testing.T) {
	// Real PowerShell drives/scopes. `$env:` is the common one; `$script:`/`$global:` etc. are
	// scopes. Anything else followed by ':' is the defect.
	validPrefixes := map[string]bool{
		"env": true, "script": true, "global": true, "local": true, "private": true,
		"using": true, "variable": true, "function": true, "alias": true,
		// real PSDrives installed by default
		"HKLM": true, "HKCU": true, "cert": true, "wsman": true, "Alias": true,
		"Env": true, "Function": true, "Variable": true,
	}
	// $NAME: where NAME is an identifier immediately followed by a colon INSIDE a double-quoted
	// string — which is where the subexpression is ambiguous. `$x:` outside a string is a
	// different (also broken) form; this catches both.
	ref := regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*):`)

	for _, wf := range []string{"ci.yml", "canary.yml", "release.yml"} {
		body := readMSIFile(t, "..", "..", ".github", "workflows", wf)
		for i, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			// A COMMENT IS NOT CODE, and this scan has to say so explicitly: the comment that
			// DOCUMENTS this very defect quotes the broken form, so scanning raw text flags the
			// explanation and fails a correct file. That is the fourth time in this file's
			// history prose has defeated a check; the default is now to strip it first.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			// GitHub expressions are substituted before pwsh ever sees the script, so
			// `${{ github.run_number }}` is not PowerShell and must not be flagged.
			scan := regexp.MustCompile(`\$\{\{[^}]*\}\}`).ReplaceAllString(trimmed, "GH_EXPR")
			for _, m := range ref.FindAllStringSubmatch(scan, -1) {
				if validPrefixes[m[1]] {
					continue
				}
				t.Errorf("%s:%d has `$%s:` — PowerShell parses that as a scope/drive-qualified\n"+
					"  variable and FAILS TO PARSE the whole script, so the step never runs:\n"+
					"      %s\n"+
					"  Write ${%s} instead.", wf, i+1, m[1], trimmed, m[1])
			}
		}
	}
}

// TestSetupHandoffRunsAfterTheFilesExist — the hand-off must be scheduled After="InstallFinalize",
// not After="InstallFiles".
//
// MSI RUNS THE EXECUTE SEQUENCE TWICE. The IMMEDIATE pass walks it and builds a script; the
// DEFERRED pass runs that script and copies the files. An IMMEDIATE custom action runs in the
// immediate pass — so one placed After="InstallFiles" runs when InstallFiles has merely been
// SCHEDULED, and the payload is not on disk yet.
//
// That is the reported bug, and it is why the report looked self-contradictory. The wizard
// reached Finish, the hand-off fired, and Windows raised "cannot find
// C:\Users\...\Orchicon\orchicon-setup.cmd" — while every later inspection, including this
// repository's own CI install, finds the file there (2428 bytes, same directory). Nothing was
// missing; the launch simply happened first.
//
// InstallFinalize is after the deferred script completes, so the payload is committed by the
// time the action runs. A DEFERRED action would also work in principle, but a deferred one
// cannot read [INSTANCE]/[RUNTIMEIMAGES] without CustomActionData, so the sequence placement is
// the right lever for this action.
func TestSetupHandoffRunsAfterTheFilesExist(t *testing.T) {
	body := stripXMLComments(readMSIFile(t, wxs()))

	if strings.Contains(body, `After="InstallFiles"`) {
		t.Error("a custom action is scheduled After=\"InstallFiles\".\n" +
			"  An IMMEDIATE action placed there runs during the immediate pass, before the\n" +
			"  deferred script has written anything — which is how the setup hand-off came to\n" +
			"  launch before orchicon-setup.cmd existed. Schedule it After=\"InstallFinalize\".")
	}
	if !strings.Contains(body, `<Custom Action="LaunchSetup" After="InstallFinalize"`) {
		t.Error("the setup hand-off is not scheduled After=\"InstallFinalize\".\n" +
			"  It needs the payload on disk, and only the deferred pass puts it there.")
	}
}

// TestEngineCallsAreBounded — an interactive install must never wait for ever on a broken
// container engine.
//
// REPORTED FROM A REAL INSTALL: the setup printed
//
//	== resolving the plane residency and host ports
//
// and then stopped. That phase's first act is rung 0 — "is this instance already running?" —
// which shells out to the engine (`docker inspect`). `exec.Command` has NO deadline, and on
// Windows a Docker Desktop pipe that EXISTS while its daemon is wedged does not fail, it
// BLOCKS. The operator had exactly that state (the log above it says "failed to connect to the
// docker API at npipe:////./pipe/dockerDesktopLinuxEngine").
//
// A bounded call degrades a broken engine to "no container is running", which lands the install
// on the host path — the supported shape — instead of hanging with nothing on screen.
func TestEngineCallsAreBounded(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "install_engine.go")

	if !strings.Contains(src, "func engineCommand(") {
		t.Fatal("engineCommand is gone — the engine call sites have moved, and this guard with them")
	}
	body := src[strings.Index(src, "func engineCommand("):]
	body = body[:strings.Index(body, "\n}")+2]

	if strings.Contains(body, "exec.Command(") && !strings.Contains(body, "exec.CommandContext(") {
		t.Error("engineCommand uses exec.Command, which has NO deadline. On a wedged engine pipe " +
			"the install blocks for ever — that is the reported hang at \"resolving the plane " +
			"residency and host ports\". Use exec.CommandContext with a bound.")
	}
	if !strings.Contains(body, "WithTimeout") {
		t.Error("engineCommand does not bound its context — a wedged engine must degrade to " +
			"\"no container is running\", not hang the install.")
	}
	if !strings.Contains(src, "engineCommandTimeout") {
		t.Error("no engineCommandTimeout constant — the bound must be named so it can be reasoned about")
	}
}

// TestInstallAsksWhichInstance — the installer must PUT the instance question to a human.
//
// The instance decides the whole stack: its ports, its data directory, its plane. Until now the
// only thing that ever asked was scripts/install.ps1; `orchicon install` — which is what the MSI
// hand-off and the Burn bundle actually run — silently took the default. So a person who
// installed from the wizard got `dev` without being told, which is the opposite of an installer
// that takes someone all the way to a working product.
func TestInstallAsksWhichInstance(t *testing.T) {
	install := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")
	if !strings.Contains(install, "askInstance(") {
		t.Error("runInstall never calls askInstance — the wizard's setup would silently install " +
			"the default instance without asking")
	}

	// AND IT MUST NOT BLOCK A HEADLESS RUN. The prompt is only reached with a terminal, and its
	// reader must fall back rather than wait — the same contract askInstallPort keeps.
	ports := readMSIFile(t, "..", "..", "cmd", "orchicon", "install_ports.go")
	i := strings.Index(ports, "func askInstance(")
	if i < 0 {
		t.Fatal("askInstance is gone")
	}
	body := ports[i:]
	body = body[:strings.Index(body, "\n}")+2]
	for _, want := range []struct{ what, sub string }{
		{"it keeps the decided value when nothing can be read", "return current, false"},
		{"ENTER takes the default", `if name == "" {`},
		{"it validates the answer", "validateInstance(name)"},
		{"it gives up rather than loop", "attempt < 3"},
	} {
		if !strings.Contains(body, want.sub) {
			t.Errorf("askInstance does not show that %s (%q)", want.what, want.sub)
		}
	}

	// THE READER IS UNBUFFERED ON A TERMINAL, deliberately: resolveInstallPorts wraps the same
	// *os.File in its own bufio.Reader, and a second buffered reader would read ahead and swallow
	// the keystrokes meant for the port prompts.
	if !strings.Contains(body, "*os.File") {
		t.Error("askInstance does not special-case *os.File — a buffered reader over the same " +
			"terminal as the port prompts can consume input intended for them")
	}
}

// TestSetupConsoleChildGetsWorkingStdio — the relaunched setup must be able to PRINT and ASK.
//
// CREATE_NEW_CONSOLE gives the child a console. It does NOT give it handles: os/exec gives a
// child whose Stdout/Stderr are nil the NUL DEVICE, so an install that "has a console" still
// writes every prompt into nowhere and reads EOF for every answer. It then finishes in seconds
// having asked nothing — which is reported as "it never prompted me for instance or any other
// options" rather than as an I/O fault.
//
// Inheriting the parent's handles is no better: msiexec has no console, so those are invalid or
// NUL too. The only handle that means anything is the child's OWN console, so the child opens
// CONIN$/CONOUT$ for itself.
//
// Asserted on the source because the failure is invisible on any host that has a console —
// which is every host this test could otherwise run on.
func TestSetupConsoleChildGetsWorkingStdio(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "console_windows.go")

	// (1) the reattach exists and uses the documented device names
	for _, want := range []struct{ what, sub string }{
		{"it opens its own console for input", `"CONIN$"`},
		{"it opens its own console for output", `"CONOUT$"`},
		{"it replaces os.Stdin", "os.Stdin = in"},
		{"it replaces os.Stdout", "os.Stdout = out"},
		{"it replaces os.Stderr", "os.Stderr = out"},
	} {
		if !strings.Contains(src, want.sub) {
			t.Errorf("console_windows.go does not show that %s (%q).\n"+
				"  Without it the relaunched setup inherits the NUL device, so its prompts go\n"+
				"  nowhere and its reads return EOF — an install that finishes instantly having\n"+
				"  asked nothing.", want.what, want.sub)
		}
	}

	// (2) THE CHILD MUST CALL IT. A helper nothing invokes is the "dead structure with a
	// finished shape" failure: the code reads complete and the behaviour is unchanged.
	child := src[strings.Index(src, `if os.Getenv(consoleRelaunchEnv) == "1" {`):]
	child = child[:strings.Index(child, "\n\t}")]
	if !strings.Contains(child, "reattachConsoleIO()") {
		t.Error("the relaunched child does NOT call reattachConsoleIO, so it keeps the NUL " +
			"handles and cannot prompt. This is the reported 'asked me nothing'.")
	}

	// (3) AND SO MUST THE CASE WHERE A CONSOLE ALREADY EXISTED, or an operator running
	// `msiexec /i` from a shell loses the setup's output to the same NUL device.
	if hasIdx := strings.Index(src, "if hasConsole() {"); hasIdx >= 0 {
		block := src[hasIdx:]
		block = block[:strings.Index(block, "\n\t}")]
		if !strings.Contains(block, "reattachConsoleIO()") {
			t.Error("the already-has-a-console path does not call reattachConsoleIO — the setup " +
				"would print into the NUL device there too")
		}
	} else {
		t.Error("no hasConsole() branch found — the relaunch logic changed shape")
	}
}

// TestPromptsAreWrittenToAStreamThatCanBeWritten — the port prompt must not be written to a
// console INPUT handle.
//
// THE REPORT: "After choosing dev as the instance, it just hangs and doesn't do anything else."
//
// On Windows promptTerminal() cannot open /dev/tty (it does not exist), so it falls back to
// os.Stdin when that is a character device — which, after the setup reattaches CONIN$, it is.
// The caller then did `if tty != nil { promptOut = tty }`, i.e. it wrote the port banner to
// CONIN$: an INPUT handle. The text goes nowhere, and resolveInstallPorts blocks reading an
// answer to a question nobody can see. The install stops dead immediately after the instance
// prompt — exactly where it was reported to stop.
//
// The distinction that matters is `ttyOwned`: only a /dev/tty we OPENED OURSELVES is
// read-write. In the os.Stdin fallback the console's OUTPUT is os.Stdout.
func TestPromptsAreWrittenToAStreamThatCanBeWritten(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")

	if !strings.Contains(src, "promptOut") {
		t.Fatal("no promptOut in install.go — the prompt-writing logic moved, and this guard with it")
	}
	// The old, wrong shape: any non-nil tty becomes the output.
	if strings.Contains(src, "if tty != nil {\n\t\tpromptOut = tty") {
		t.Error("promptOut is taken from `tty` whenever it is non-nil. On Windows that is os.Stdin " +
			"— a console INPUT handle — so the prompt is written where nobody can read it and the " +
			"install blocks. Gate it on ttyOwned (a /dev/tty we opened read-write).")
	}
	if !strings.Contains(src, "if ttyOwned {") {
		t.Error("promptOut is not gated on ttyOwned — only a read-write /dev/tty can carry the prompt")
	}

	// AND promptTerminal MUST KEEP REPORTING WHICH CASE IT IS IN. It returns (file, owned); a
	// refactor that drops the second value silently reintroduces the bug above.
	ports := readMSIFile(t, "..", "..", "cmd", "orchicon", "install_ports.go")
	if !strings.Contains(ports, "func promptTerminal() (*os.File, bool)") {
		t.Error("promptTerminal no longer reports whether the terminal is OURS — that flag is what " +
			"distinguishes a read-write /dev/tty from the os.Stdin fallback")
	}
}

// TestWizardAnswersReachTheInstallNonInteractively — the wizard's answers must ARRIVE, and the
// setup must not ask again.
//
// REPORTED: "Once I hit install, it still brought up the script and still asked me to fill in all
// of the same information which is redundant." The MSI had carried a SKIPCONFIG property for
// exactly this; NOTHING IN THE GO CODE EVER READ IT. The wizard collected everything and the setup
// then re-opened a console and put the same questions — so the pages were worse than useless, they
// were a second round of the same form.
//
// Two halves, and both are needed: the action passes --non-interactive, and the install HONOURS it
// (no console relaunch, no prompts). Either alone reproduces the bug.
func TestWizardAnswersReachTheInstallNonInteractively(t *testing.T) {
	wxsBody := stripXMLComments(readMSIFile(t, wxs()))
	ca := regexp.MustCompile(`<CustomAction Id="LaunchSetup"[\s\S]*?/>`).FindString(wxsBody)
	if ca == "" {
		t.Fatal("no LaunchSetup custom action")
	}
	if !strings.Contains(ca, "--non-interactive") {
		t.Error("LaunchSetup does not pass --non-interactive. The setup would open a console and " +
			"ask every question — which is the behaviour the wizard change exists to remove.")
	}

	// THE WIZARD ASKS NOTHING NOW, so the hand-off must carry no interpolated answer.
	//
	// This is the inverse of what it used to assert, and the inversion is the design: instance, ports and
	// images were three pages of questions a person installing Orchicon has no basis to answer, and the
	// port page in particular is what produced the 5433 bug (an install that renumbered an existing
	// instance's database because it offered "5432 in use — use 5433?"). The installer now installs
	// `dev` with defaults, and the Servers page changes any of it afterwards.
	for _, gone := range []string{"[INSTANCE]", "[CONTROLPORT]", "[POSTGRESPORT]", "[LOKIPORT]", "[TEMPOPORT]", "[RUNTIMEIMAGES]"} {
		if strings.Contains(ca, gone) {
			t.Errorf("LaunchSetup still interpolates %s:\n  %s\n"+
				"  No wizard page collects it any more, so it would reach the install as an EMPTY\n"+
				"  argument — which is exactly the lost-quote hazard this action was rewritten to avoid.", gone, ca)
		}
	}

	// AND THE INSTALL MUST STILL HONOUR --non-interactive. A flag nothing reads is the same defect one
	// layer down.
	install := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")
	if !strings.Contains(install, "flags.NonInteractive") {
		t.Error("install.go never consults flags.NonInteractive — the flag would be parsed and " +
			"ignored, and the console would still ask everything")
	}
}

func TestDialogTextHasNoStrayAccelerators(t *testing.T) {
	for _, n := range []int{1} {
		_ = n
	}
	body := readMSIFile(t, wxs())
	// Every Text="…" attribute in the dialog section.
	texts := regexp.MustCompile(`Text="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(texts) == 0 {
		t.Fatal("found no Text attributes — the matcher is wrong and this test would pass vacuously")
	}
	for _, m := range texts {
		v := m[1]
		if strings.HasPrefix(v, "&amp;") {
			t.Errorf("a Text attribute starts with an escaped ampersand: %q\n"+
				"  MSI reads a leading `&` as an ACCELERATOR marker and renders the next character\n"+
				"  underscored, which is what the operator saw as a stray `&`. Drop it, or write\n"+
				"  `&amp;&amp;` for a literal one.", v)
		}
	}
}

// TestImagePageOffersMoreThanTheExtremes — "none" and "all" covered the ends and nothing between,
// and "some" is the common case.
//
// REPORTED: "The image screen only has None or ALL images. We should try and see if we can do a
// checkbox type selector." The `--runtime-images 1,3` list form was built for this from the start;
// the page simply never used it.
//
// The risk this guards is subtler than "the boxes are missing": three ticked ticks that never
// compose RUNTIMEIMAGES install nothing and report nothing wrong.
func TestImagePageOffersMoreThanTheExtremes(t *testing.T) {
	// SUPERSEDED BY THE DESIGN, and the replacement is the useful guard: the images page is GONE.
	//
	// It asked which runtime images to pull before a machine had ever run Orchicon. Runtime images are for
	// per-workflow containers, they are gigabytes, and nothing needs one to have a working server — so the
	// question moved to where it belongs (the app, per project) and the installer stopped asking.
	//
	// The regression this catches is not "the labels are wrong" but "the page came back": a wizard that
	// asks a question nobody can answer is the class of defect this whole change removes.
	body := stripXMLComments(readMSIFile(t, wxs()))
	for _, gone := range []string{`Id="ImagesDlg"`, `Property="IMAGESMODE"`, `Property="IMAGE1"`} {
		if strings.Contains(body, gone) {
			t.Errorf("the installer asks about runtime images again (%s).\n"+
				"  A fresh install has no basis to answer, nothing needs an image to run, and each one\n"+
				"  is gigabytes to download. The app owns that choice now.", gone)
		}
	}
}

func TestInstallFailsWhenTheServicesDoNotComeUp(t *testing.T) {
	install := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")

	// (1) the return value must be USED.
	if strings.Contains(install, "waitServicesReady(instance, ports, os.Stdout)\n") {
		t.Error("install.go discards waitServicesReady's result again. That is the defect: the " +
			"install starts the plane over a stack that is not there and reports success.")
	}
	if !strings.Contains(install, "!waitServicesReady(") && !strings.Contains(install, "ready := waitServicesReady(") {
		t.Error("install.go does not branch on waitServicesReady — a services stack that never came " +
			"up would still be reported as a successful install")
	}
	if !strings.Contains(install, "servicesNotReadyError") {
		t.Error("install.go does not produce a services-not-ready error, so a failure would name nothing")
	}

	// (2) the error must CARRY THE CAUSE, not just say "failed".
	host := readMSIFile(t, "..", "..", "cmd", "orchicon", "install_host.go")
	for _, want := range []struct{ what, sub string }{
		{"it names the services log", `filepath.Join(hostDataDir, "services.log")`},
		// THE CALL, NOT THE DEFINITION. `"tailLines("` is satisfied by `func tailLines(` alone, so
		// emptying the call still passed — a control caught it. The argument list is what
		// distinguishes a function that is called from one that merely exists.
		{"it quotes the tail of that log", "tailLines(logPath"},
		{"it treats a MISSING log as its own finding", "did not start"},
		{"it says the install is resumable", "orchicon install"},
	} {
		if !strings.Contains(host, want.sub) {
			t.Errorf("servicesNotReadyError does not show that %s (%q)", want.what, want.sub)
		}
	}

	// (3) the supervisor must not outlive a failed critical chain.
	svc := readMSIFile(t, "..", "..", "cmd", "orchicon", "host_services.go")
	for _, comp := range []string{"postgres", "nats"} {
		i := strings.Index(svc, comp+" did not become ready")
		if i < 0 {
			t.Errorf("no %q failure branch in host_services.go — the supervisor would carry on "+
				"without it, holding the ports and the PID file while nothing works", comp)
			continue
		}
		branch := svc[i:]
		if end := strings.Index(branch, "\n\t}"); end > 0 {
			branch = branch[:end]
		}
		if !strings.Contains(branch, "return 1") {
			t.Errorf("%s failing does not stop the supervisor. A supervisor with no database "+
				"supervises nothing: it keeps the ports, keeps the PID file, and `orchicon status` "+
				"calls it running.", comp)
		}
	}
}

// TestTailLinesIsABoundedTail — the failure path must not try to read a 200 MB log into memory to
// show the last twenty-five lines.
func TestTailLinesIsABoundedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "services.log")

	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "line %d of a long services log\n", i)
	}
	b.WriteString("postgres did not become ready: initdb failed\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got := tailLines(path, 25)
	if !strings.Contains(got, "postgres did not become ready") {
		t.Error("tailLines did not keep the LAST line, which is the one that matters")
	}
	if strings.Contains(got, "line 0 of") {
		t.Error("tailLines returned the head of the file rather than its tail")
	}
	if n := len(strings.Split(got, "\n")); n > 26 {
		t.Errorf("tailLines returned %d lines, want at most 25", n)
	}
	if tailLines(filepath.Join(dir, "absent.log"), 25) != "" {
		t.Error("a missing log must return empty, not an error: this runs on a failure path and " +
			"must not replace the real error with a second one")
	}
}

// TestSupervisedChildrenGetNoConsoleWindow — the supervisor's children must not open windows.
//
// REPORTED: "It also popped up a TON of blank command prompt windows that wouldn't go away until I
// killed them… victoriametrics, otel, and nats. They seem to be trying to start over and over."
//
// That is exactly what the code did. The supervisor runs DETACHED (no console), and a
// console-subsystem child of a process with no console gets a BRAND NEW WINDOW. Every nats /
// victoriametrics / loki / tempo / otelcol start opened one, and reapRestart opened another on each
// restart (backoff 2s, 4s, 8s…), so a child that crashed on boot produced a window every couple of
// seconds for ever.
//
// CREATE_NO_WINDOW is the fix, and it is NOT DETACHED_PROCESS: these children stay attached to the
// supervisor (their stdout is piped to its log) and must die with it.
func TestSupervisedChildrenGetNoConsoleWindow(t *testing.T) {
	svc := readMSIFile(t, "..", "..", "cmd", "orchicon", "host_services.go")
	i := strings.Index(svc, "func (s *supervisor) start(")
	if i < 0 {
		t.Fatal("supervisor.start is gone — the child-spawn path moved, and this guard with it")
	}
	body := svc[i:]
	if end := strings.Index(body, "\n}"); end > 0 {
		body = body[:end]
	}
	// TWO SANCTIONED ROUTES, ONE OUTCOME. The attribute may be applied directly or by the spawn helper
	// that exists to apply it (quiet.go). What must NOT happen is a bare exec.CommandContext — which is
	// precisely what this guard originally caught, and what a later round of probes reintroduced
	// elsewhere. Asserting the ATTRIBUTE rather than one spelling of it is what keeps this guard
	// pointing at the behaviour instead of at a line of code.
	if !strings.Contains(body, "procwin.CommandContext(") && !strings.Contains(body, "setProcAttrNoWindow(cmd)") {
		t.Error("supervisor.start neither uses quietCommandContext nor calls setProcAttrNoWindow. Every " +
			"child it spawns on Windows opens its own console window, and reapRestart opens another on " +
			"every restart — reported as \"a TON of blank command prompt windows\".")
	}
	if strings.Contains(body, "exec.CommandContext(") && !strings.Contains(body, "procwin.CommandContext(") {
		t.Error("supervisor.start still spawns with a bare exec.CommandContext. On Windows a console " +
			"child of this DETACHED supervisor gets a brand new window — use procwin.CommandContext.")
	}

	// AND THE FLAG MUST BE CREATE_NO_WINDOW, not detached: a detached child would not be
	// attached to the supervisor's log pipe or its lifetime.
	win := readMSIFile(t, "..", "..", "cmd", "orchicon", "procattr_windows.go")
	if !strings.Contains(win, "createNoWindow = 0x08000000") {
		t.Error("procattr_windows.go does not define CREATE_NO_WINDOW (0x08000000)")
	}
	if !strings.Contains(win, "func setProcAttrNoWindow(cmd *exec.Cmd)") {
		t.Fatal("setProcAttrNoWindow is missing from the Windows half")
	}
	unix := readMSIFile(t, "..", "..", "cmd", "orchicon", "procattr_unix.go")
	if !strings.Contains(unix, "func setProcAttrNoWindow(*exec.Cmd)") {
		t.Error("the Unix half is missing, so the supervisor call would not compile there")
	}
}

// TestBothPlaneStartPathsMigrate — a plane must never come up against an empty database.
//
// REPORTED: after installing, starting the services by hand and running `orchicon`, every
// reconciler failed with `relation "work_items" does not exist (SQLSTATE 42P01)` and the web UI
// said "invalid credentials" — because no schema and therefore no users table existed.
//
// THE CAUSE. There are TWO ways to start this plane and only one of them migrated:
//
//	`serve` / `serve --detach`  -> the migration block in serve.go   (install, start, supervisor)
//	`orchicon` with no args     -> server.New() directly in main.go  (NOTHING)
//
// A migration that runs on one start path and not the other is worse than none, because it makes
// the schema depend on HOW the operator happened to launch the binary.
//
// Asserted on the source: the behaviour needs a real Postgres to exercise, and the defect is a
// missing call, which is visible statically.
func TestBothPlaneStartPathsMigrate(t *testing.T) {
	serve := readMSIFile(t, "..", "..", "cmd", "orchicon", "serve.go")
	main := readMSIFile(t, "..", "..", "cmd", "orchicon", "main.go")

	// ONE implementation, so the two cannot drift again.
	if !strings.Contains(serve, "func migrateOnBoot(") {
		t.Error("serve.go does not define migrateOnBoot — there is no single place both start " +
			"paths share, which is how they diverged in the first place")
	}
	// `serve` must still use it.
	if !strings.Contains(serve, "migrateOnBoot(ctx, cfg, log)") {
		t.Error("serve.go no longer calls migrateOnBoot, so `serve --detach` — the path install, " +
			"start and the supervisor all use — would stop migrating")
	}
	// AND SO MUST THE BARE PATH. This is the defect.
	if !strings.Contains(main, "migrateOnBoot(ctx, cfg, log)") {
		t.Error("main.go's no-subcommand path does not call migrateOnBoot. That plane comes up " +
			"against an EMPTY database: every reconciler fails with \"relation \\\"work_items\\\" " +
			"does not exist\" and the web UI rejects every login.")
	}
	// ORDER MATTERS: migrations before the server is constructed, or the reconcile loops start
	// scanning tables that do not exist yet.
	iMig := strings.Index(main, "migrateOnBoot(ctx, cfg, log)")
	iSrv := strings.Index(main, "server.New(cfg, log, nil)")
	if iMig >= 0 && iSrv >= 0 && iMig > iSrv {
		t.Error("main.go migrates AFTER constructing the server — the reconcilers would scan " +
			"tables that are not there yet")
	}
	// AND IT MUST BE FATAL ON FAILURE: a plane that cannot build its schema must not serve.
	if iMig >= 0 {
		tail := main[iMig:]
		if end := strings.Index(tail, "}"); end > 0 {
			tail = tail[:end]
		}
		if !strings.Contains(tail, "os.Exit(1)") {
			t.Error("main.go ignores a migration failure — the plane would serve from an " +
				"unmigrated database instead of stopping")
		}
	}
}

// TestDialogControlsDoNotOverlap — the wizard must not paint one control through another.
//
// REPORTED: "the images screen also has some weird graphical glitches to where you can't read
// all of the images." The hint Text sat at Y=150 and the third checkbox at Y=156, so the sentence
// was drawn straight through the last row. Nothing failed — MSI happily paints overlapping
// controls — and I cannot see a dialog from here, so this is checked geometrically instead.
//
// Only the controls that carry TEXT or INPUT are compared: the banner bitmap, its line and the
// bottom rule are full-width decorations that legitimately sit under everything else.
func TestDialogControlsDoNotOverlap(t *testing.T) {
	body := stripXMLComments(readMSIFile(t, wxs()))

	type rect struct {
		id         string
		x, y, w, h int
	}

	dialogRe := regexp.MustCompile(`(?s)<Dialog Id="([A-Za-z]+)"(.*?)</Dialog>`)
	// THE OPENING TAG IS ENOUGH, self-closing or not.
	//
	// This used to require "/>" so it matched only controls with no children — which silently excluded every
	// BUTTON, because a button carries its <Publish> children. The vacuity guard below is what caught it:
	// "found no dialogs to check — the matcher is wrong". A geometry check that skips the buttons is a check
	// that cannot see the controls an operator clicks.
	controlRe := regexp.MustCompile(`<Control Id="([A-Za-z0-9_]+)"([^>]*)>`)
	attr := func(s, name string) (int, bool) {
		m := regexp.MustCompile(name + `="([0-9]+)"`).FindStringSubmatch(s)
		if m == nil {
			return 0, false
		}
		n, _ := strconv.Atoi(m[1])
		return n, true
	}

	checked := 0
	for _, d := range dialogRe.FindAllStringSubmatch(body, -1) {
		dialog, rest := d[1], d[2]

		var rects []rect
		for _, c := range controlRe.FindAllStringSubmatch(rest, -1) {
			id, attrs := c[1], c[2]
			// Decorations: full-width art and lines sit beneath the content by design.
			switch id {
			case "BannerBitmap", "BannerLine", "BottomLine":
				continue
			}
			x, ok1 := attr(attrs, "X")
			y, ok2 := attr(attrs, "Y")
			w, ok3 := attr(attrs, "Width")
			h, ok4 := attr(attrs, "Height")
			if !ok1 || !ok2 || !ok3 || !ok4 {
				continue // e.g. a RadioButton nested inside its group, positioned relative
			}
			rects = append(rects, rect{id, x, y, w, h})
		}
		if len(rects) < 4 {
			continue // not one of ours
		}
		checked++

		for i := 0; i < len(rects); i++ {
			for j := i + 1; j < len(rects); j++ {
				a, b := rects[i], rects[j]
				// Strict inequality: touching edges are fine, sharing area is not.
				if a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h {
					t.Errorf("%s: controls %q (%d,%d %dx%d) and %q (%d,%d %dx%d) OVERLAP.\n"+
						"  MSI paints overlapping controls on top of each other, so one of them is\n"+
						"  unreadable — that is the reported \"weird graphical glitches\". No build\n"+
						"  fails for this; only the operator sees it.",
						dialog, a.id, a.x, a.y, a.w, a.h, b.id, b.x, b.y, b.w, b.h)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no dialogs to check — the matcher is wrong and this test would pass vacuously")
	}
}

// TestPortLabelsNameTheirDefault — "so people aren't guessing what they are setting".
//
// The port fields started as bare labels with empty boxes: an operator had to know that 8080 is
// dev's control port and 8092 is prod's. The default now appears IN the label, and it is three
// numbers because the default depends on the instance chosen on the previous page — dev, test and
// prod are offset by design so two stacks on one machine never collide.

// TestPortLabelsNameTheirDefault — "so people aren't guessing what they are setting".
//
// The port fields started as bare labels with empty boxes: an operator had to know that 8080 is
// dev's control port and 8092 is prod's. The default now appears IN the label, and it is three
// numbers because the default depends on the instance chosen on the previous page — dev, test and
// prod are offset by design so two stacks on one machine never collide.
func TestPortLabelsNameTheirDefault(t *testing.T) {
	// SUPERSEDED BY THE DESIGN. The port page is gone, and the guard that matters is that it stays gone
	// AND that the install is never handed a port it did not resolve itself.
	//
	// THE 5433 BUG IS WHY THIS IS ASSERTED RATHER THAN REMEMBERED. The wizard offered
	// "PostgreSQL: port 5432 is in use — suggesting 5433?", accepted its own suggestion on a
	// non-interactive run, and PERSISTED it: the operator's database moved, and every later start waited
	// on a port postgres would never bind.
	body := stripXMLComments(readMSIFile(t, wxs()))
	if strings.Contains(body, `Id="PortsDlg"`) {
		t.Error("the installer asks about ports again. Every port is derivable from the instance, and " +
			"asking is the page that produced the 5433 bug — an install that silently renumbered an " +
			"existing instance's database.")
	}
	ca := regexp.MustCompile(`<CustomAction Id="LaunchSetup"[\s\S]*?/>`).FindString(body)
	if strings.Contains(ca, "--port") {
		t.Errorf("LaunchSetup pins a port:\n  %s\n"+
			"  The install must resolve its own ports from the instance, so a re-run keeps the ones the\n"+
			"  instance already recorded.", ca)
	}
}

func TestImageLabelsNameTheRealVariants(t *testing.T) {
	// SUPERSEDED BY TestImagePageOffersMoreThanTheExtremes, which now asserts the page is absent.
	//
	// Kept as a NAMED function rather than deleted so the suite reports the intent explicitly: the labels
	// it used to check cannot exist on a page that does not, and a silent deletion would look like the
	// coverage was never there. If runtime images ever return to the installer, this is the name to
	// repoint.
	body := stripXMLComments(readMSIFile(t, wxs()))
	if strings.Contains(body, `Id="ImagesDlg"`) {
		t.Fatal("the image page is back. Repoint THIS guard at it (its old assertions checked the labels " +
			"named the real runtime variants) rather than deleting it — a page with invented labels is a " +
			"page nobody can act on.")
	}
}

func TestAPartialPortRecordIsNeverUsed(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "services.go")

	// THE CALL SITE, NOT THE HELPER'S EXISTENCE. A first draft of this guard only looked for
	// the helper being DEFINED, so reverting the call site left the helper in place and the
	// guard went green — a control caught it. The definition proves nothing; the USE does.
	if !strings.Contains(src, "err == nil && portsRecordIsComplete(rec)") {
		t.Error("services.go does not check whether the port record is COMPLETE before using it.\n" +
			"  A record that has postgres and nats but is missing the rest describes two\n" +
			"  instances at once, and the plane and the services then disagree on where the\n" +
			"  database is. That is the reported \"postgres :5433, nats :4222\".")
	}
	// AND THE CHECK MUST BE COMPLETENESS, not a spot check of two ports.
	//
	// It iterates everyServicePortEnv — EVERY port in the table, published or not. It used to
	// iterate activeInstallPorts, which filters out the five ports a host shape binds but does not
	// PUBLISH (Tempo's gRPC listener and its two OTLP receivers, the collector's health extension and
	// self-metrics exporter). No real ports.json ever named those, so the completeness test answered
	// false for every CORRECT install and the record was never trusted at all.
	if !strings.Contains(src, "for _, key := range everyServicePortEnv()") {
		t.Error("portsRecordIsComplete does not require EVERY port to be present.\n" +
			"  Two ports checked and the rest unchecked is the defect itself — and checking only the\n" +
			"  PUBLISHED ports is the same defect over a slightly larger set.")
	}
	if !strings.Contains(src, "if rec[key] <= 0 {") {
		t.Error("portsRecordIsComplete does not reject a missing port — an unset port reads as 0 and " +
			"must fail the completeness test, or a record describing two instances at once is used.")
	}
}

// TestStoppingTheServicesKillsTheirChildren — a stop must stop the DATABASE, not just the
// supervisor.
//
// REPORTED, in the services log of a real install:
//
//	started component=postgres pid=1604
//	FATAL:  lock file "postmaster.pid" already exists
//	HINT:  Is another postmaster (PID 26376) running in data directory "…/orchicon-dev/postgres"?
//	postgres exited during startup: exit status 1
//
// THE CHAIN. The services supervisor owns postgres as a CHILD process. The stop path terminated
// the supervisor BY PID — one process — so postgres was ORPHANED: still running, still holding
// the data directory's postmaster.pid lock and its port. The next install's postgres refused to
// start against a lock another postmaster still held.
//
// So the stop must kill the TREE. On Unix the group kill does it exactly; on Windows it is
// taskkill /T.
func TestStoppingTheServicesKillsTheirChildren(t *testing.T) {
	host := readMSIFile(t, "..", "..", "cmd", "orchicon", "install_host.go")
	if !strings.Contains(host, "terminateProcessTree(pid)") {
		t.Error("stopHostServices still terminates the supervisor alone. Its children — postgres, nats —\n" +
			"  survive it: postgres keeps the postmaster.pid lock and its port, and the next install's\n" +
			"  postgres refuses to start. That is the reported \"lock file postmaster.pid already\n" +
			"  exists\".")
	}

	win := readMSIFile(t, "..", "..", "internal", "planecontrol", "hostproc_windows.go")
	if !strings.Contains(win, "func TerminateProcessTree(") {
		t.Error("the Windows half has no TerminateProcessTree — there is no process-group signal on " +
			"Windows, so the tree kill has to be taskkill /T")
	}
	if !strings.Contains(win, `"/T"`) {
		t.Error("TerminateProcessTree does not use taskkill /T — without /T it kills the " +
			"supervisor and leaves its children, which is the defect")
	}
	// AND UNIX MUST STILL HAVE THE SEAM, or the call site will not compile there.
	unix := readMSIFile(t, "..", "..", "internal", "planecontrol", "hostproc_unix.go")
	if !strings.Contains(unix, "func TerminateProcessTree(") {
		t.Error("the Unix half is missing TerminateProcessTree, so the shared call site will not " +
			"compile on Unix")
	}
}

// TestAStalePostmasterLockIsClearedBeforeInitdb — a leftover lock must not stop the database.
//
// The decision is platform-NEUTRAL even though the sweep is only NEEDED on Windows: a bug here
// is a database that cannot start, and a test that only ran on the one platform nobody has in
// CI is a test nobody runs.
func TestAStalePostmasterLockIsClearedBeforeInitdb(t *testing.T) {
	lock := readMSIFile(t, "..", "..", "cmd", "orchicon", "postgres_lock.go")

	// The live/stale decision must consult LIVENESS, not mere existence: a lock naming a dead
	// process is a leftover, and a lock naming a live one is postgres's own reason to refuse.
	if !strings.Contains(lock, "pidAlive(pid)") {
		t.Error("the lock sweep does not check whether the named process is alive. Existence is " +
			"not staleness: postgres refuses to start for a LIVE postmaster on purpose.")
	}
	// And the sweep must run BEFORE initdb, or the new postgres still meets the lock.
	//
	// THE ORDER IS COMPARED IN THE FUNCTION THAT DOES BOTH, not in the file: a first draft
	// compared the FIRST index of each name in the whole file, so moving the sweep after
	// initdb — within the same function — still passed. A control caught that. The window
	// between the two calls is what matters.
	host := readMSIFile(t, "..", "..", "cmd", "orchicon", "host_services.go")
	pre := host[strings.Index(host, "func (s *supervisor) prepare("):]
	end := strings.Index(pre, "\nfunc ")
	if end > 0 {
		pre = pre[:end]
	}
	iLock := strings.Index(pre, "clearStalePostmasterLock(")
	iInit := strings.Index(pre, "s.initPostgres()")
	if iLock < 0 || iInit < 0 || iLock > iInit {
		t.Error("the stale-lock sweep does not run before initdb in the same function — the new " +
			"postgres would still meet the lock it is meant to be spared")
	}
}

// TestEveryBundleBuildNamesTheExtensionThatExists — the bootstrapper extension's PACKAGE id and
// its DLL name are not the same, and WiX resolves `-ext <id>` to <id>/<version>/wixext5/<id>.dll.
//
// The package is WixToolset.Bal.wixext, but the DLL inside it is named
// WixToolset.BootstrapperApplications.wixext.dll — so the id that RESOLVES is
// WixToolset.BootstrapperApplications.wixext, and naming the package id gives:
//
//	error WIX0144: The extension 'WixToolset.Bal.wixext' could not be found.
//	              Checked paths: …\WixToolset.Bal.wixext\5.0.2\wixext5\WixToolset.Bal.wixext.dll
//
// which is how the canary's first bundle build failed. The same trap applies to the UI
// extension, whose package and DLL names DO match, and to WixUILicenseRtf, which is a BIND
// variable and not a preprocessor one.
//
// Every site that builds a bundle must name the id that resolves, and must install that
// extension first — `-ext` never restores from NuGet.
func TestEveryBundleBuildNamesTheExtensionThatExists(t *testing.T) {
	const resolves = "WixToolset.BootstrapperApplications.wixext"
	const doesNotResolve = "WixToolset.Bal.wixext"

	for _, site := range []struct{ what, path string }{
		{"ci.yml", filepath.Join("..", "..", ".github", "workflows", "ci.yml")},
		{"release.yml", filepath.Join("..", "..", ".github", "workflows", "release.yml")},
		{"canary.yml", filepath.Join("..", "..", ".github", "workflows", "canary.yml")},
		{"OrchiconSetup.wxs", filepath.Join("..", "..", "deploy", "desktop", "windows", "OrchiconSetup.wxs")},
	} {
		body := readMSIFile(t, site.path)
		if !strings.Contains(body, "OrchiconSetup.wxs") && site.what != "OrchiconSetup.wxs" {
			continue // this site does not build a bundle
		}
		if strings.Contains(body, doesNotResolve) {
			t.Errorf("%s names %s, which does not resolve: the package's DLL is named\n"+
				"  %s.dll, and WiX looks for <id>/<version>/wixext5/<id>.dll.\n"+
				"  This is the WIX0144 that failed the canary's first bundle build.",
				site.what, doesNotResolve, resolves)
		}
		if !strings.Contains(body, resolves) {
			t.Errorf("%s does not name %s, the id that actually resolves", site.what, resolves)
		}
		// AND THE EXTENSION MUST BE INSTALLED FIRST: -ext never restores from NuGet.
		if !strings.Contains(body, "wix extension add") {
			t.Errorf("%s builds a bundle without `wix extension add` — -ext resolves through "+
				"WiX's own store, never NuGet, so the build cannot find the extension", site.what)
		}
	}
}

// TestInstallDoesNotFailOnAMissingRuntimeDaemon — the runtime daemon is OPTIONAL when the host
// has no container engine.
//
// MEASURED ON A REAL WINDOWS INSTALL, and it is the reason the install produced no plane at all:
//
//	== skipping the image pull — no container engine
//	orchicon: runtime images are only needed for per-workflow runtime containers, and there is
//	nothing on this host to run them in
//	== starting the runtime daemon
//	orchicon: error: runtime daemon did not become ready — see …\runtime-daemon.log
//
//	…and the daemon's log said: error: runtime engine "podman" not found on PATH.
//
// The install had ALREADY decided the feature was unavailable, and then refused to finish
// because the feature's helper could not start. The operator was left with a desktop app that
// could not reach anything, and the wizard reported success.
//
// AND THE PLANE DOES NOT NEED THE DAEMON. The server says so itself:
//
//	When the socket is absent (headless `orchicon serve`), the lifecycle is disabled and
//	execution stays in-process.
//
// So without an engine the daemon's absence is a DEGRADATION TO REPORT, never a reason to
// refuse to install.
func TestInstallDoesNotFailOnAMissingRuntimeDaemon(t *testing.T) {
	src := readMSIFile(t, "..", "..", "cmd", "orchicon", "install.go")

	// The failure must be CONDITIONAL on there being no engine — never unconditional.
	i := strings.Index(src, "if err := ensureInstallDaemon(socketDir); err != nil {")
	if i < 0 {
		t.Fatal("the install no longer starts the runtime daemon at that step — this guard must " +
			"follow the call to wherever it went")
	}
	block := src[i:]
	if end := strings.Index(block, "\n\t}"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "containerEngineAvailable()") {
		t.Error("a runtime daemon that cannot start still fails the install unconditionally.\n" +
			"  On a host with no container engine the daemon CANNOT start, and the install has\n" +
			"  already said the images are not needed — so this must be a DEGRADATION TO REPORT,\n" +
			"  not a refusal. Measured: an install that aborted here left no services and no plane.")
	}
	// AND the operator must be told what still works, so the message is not just an error.
	if !strings.Contains(block, "in-process") {
		t.Error("the degraded path does not say that work still runs in-process. The operator has " +
			"to know that the plane, the web UI and the TUI are unaffected — otherwise this reads " +
			"as a failed install.")
	}
}
