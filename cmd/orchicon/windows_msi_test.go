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
			if n > 1 {
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
		{"the desktop artifact upload paths", "path: dist/desktop/*", 1},
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
	if !strings.Contains(canary, "needs: [build, desktop-windows]") {
		t.Error("canary.yml's publish job does not depend on desktop-windows.\n" +
			"  On a failed desktop build the publish would still run and leave the previous\n" +
			"  merge's MSI under a tag that claims to be this commit.")
	}

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
