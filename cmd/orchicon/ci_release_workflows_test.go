package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ci_release_workflows_test.go — the release and canary workflows must agree on what they build.
//
// THE CANARY IS A SECOND COPY OF THE RELEASE MATRIX, deliberately: `release.yml` IS the release path, and
// refactoring it into a reusable workflow to share four lines would put risk on the one thing that must
// not break. The cost of duplicating it is that the two can drift — and a canary that builds a different
// platform set (or a differently shaped archive) than the release tests a DIFFERENT PATH than the one that
// ships, which is the failure mode that makes a test worthless rather than loud.
//
// So the agreement is asserted here instead of left to care, the same way TestRuntimeVariantRefMatches
// ReleaseWorkflow pins the installer's image refs against release.yml and TestInstallPortDefaultsMatch
// ContainerScript pins the port table against scripts/container.sh.

// matrixPlatforms reads the `- os: <x>` / `arch: <y>` pairs out of a workflow's build matrix, in order.
// Deliberately a line scanner rather than a YAML parse: the property is "the same platforms, listed the
// same way", and a scanner says exactly that without depending on a YAML library's shape.
func matrixPlatforms(t *testing.T, path string) [][2]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// AN `os:` LINE IS FOLLOWED BY ITS `arch:` LINE, in both files — the pairing is asserted rather
	// than the two keys being scanned independently, which would pick up the `GOARCH` that appears in
	// the build steps of both workflows.
	var out [][2]string
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if len(f) != 2 || f[0] != "os:" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			g := strings.Fields(strings.TrimPrefix(strings.TrimSpace(lines[j]), "- "))
			if len(g) == 0 {
				continue
			}
			if len(g) == 2 && g[0] == "arch:" {
				out = append(out, [2]string{f[1], g[1]})
			}
			break
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: found no matrix platforms — did the matrix shape change?", path)
	}
	return out
}

func TestCanaryMatchesReleaseMatrix(t *testing.T) {
	release := matrixPlatforms(t, "../../.github/workflows/release.yml")
	canary := matrixPlatforms(t, "../../.github/workflows/canary.yml")
	if !reflect.DeepEqual(release, canary) {
		t.Errorf("the canary and the release build different platform sets:\n  release: %v\n  canary:  %v\n"+
			"A canary that builds something other than what ships tests a different install path than the "+
			"one users get.", release, canary)
	}
	// WINDOWS IS IN BOTH, and the exclusion that used to live here is dead. It read "nothing consumes a
	// Windows artifact (the stack runs in WSL2 there, and the installer installs the Linux binary into the
	// distro)" — which stopped being true when the plane became a HOST process: on Windows `orchicon
	// install` and `orchicon serve` run a NATIVE orchicon.exe, and WSL2 is left with only the runtime
	// CONTAINERS, which need a Linux kernel. So the release publishes
	// orchicon_<version>_windows_amd64.zip, and a release that stopped building it would silently take
	// Windows support away from a platform the plane now serves.
	//
	// windows/arm64 is deliberately ABSENT from the release matrix: Windows on ARM runs the amd64 zip under
	// emulation, so a second artifact would be published for nobody. It is still in `make cross-compile`'s
	// CROSS_PLATFORMS, so an arm64 break fails the gate rather than a release — that difference between the
	// SHIP list and the COMPILE list is the decision asserted here.
	wantWindows := [2]string{"windows", "amd64"}
	gotWindows := false
	for _, p := range release {
		if p == wantWindows {
			gotWindows = true
		}
		if p[0] == "windows" && p != wantWindows {
			t.Errorf("release.yml builds %v — only windows/amd64 ships; windows/arm64 is compiled by "+
				"`make cross-compile` but not published (Windows on ARM runs the amd64 zip under emulation)", p)
		}
	}
	if !gotWindows {
		t.Errorf("release.yml no longer builds %v — Windows is a SHIPPED platform (the plane runs "+
			"natively there; WSL2 is only for runtime containers). If that decision has been reversed, "+
			"update canary.yml, Makefile's CROSS_PLATFORMS and ARCHITECTURE.md in the same change", wantWindows)
	}
}

// TestReleaseWorkflowPackagesTheWindowsZip — a matrix entry alone does NOT produce the file AC2 promises.
// The target, the `.exe` suffix and the zip branch are three separate wirings in release.yml (and two in
// canary.yml), each of which can rot on its own: a `windows` matrix entry whose build step loses its
// `.exe` branch packages a binary Windows cannot run, and one whose archive selector is `tar.gz` publishes
// an archive the documented install command does not fetch. Asserted by string because the alternative is
// a release run — the point is to fail before one.
func TestReleaseWorkflowPackagesTheWindowsZip(t *testing.T) {
	release := readWorkflow(t, "../../.github/workflows/release.yml")
	canary := readWorkflow(t, "../../.github/workflows/canary.yml")

	// TWICE: the build step names the binaries and the package step copies them again. One without the
	// other is a broken asset, not a partially-complete one.
	if n := strings.Count(release, `[ "${{ matrix.os }}" = "windows" ] && EXT=".exe"`); n != 2 {
		t.Errorf("release.yml release:119-141: found %d `EXT=\".exe\"` branches, want 2 (build + package) — "+
			"a matrix entry without them builds a binary named `orchicon` for Windows", n)
	}
	for _, want := range []struct{ what, sub string }{
		{"release.yml:76-87 the matrix target", "- os: windows"},
		{"release.yml:76-87 the archive selector (zip, not tar.gz)", "archive: zip"},
		{"release.yml:132 the published asset name", `ASSET="orchicon_${VERSION_NUM}_${{ matrix.os }}_${{ matrix.arch }}"`},
		{"release.yml:141 the zip packaging branch", `zip -r "${ASSET}.zip" "$ASSET"`},
		{"canary.yml the same matrix target", "- os: windows"},
		{"canary.yml the zip packaging branch", `zip -r "${ASSET}.zip" "$ASSET"`},
		{"canary.yml the artifact upload glob", "orchicon_*.zip"},
		{"canary.yml the prerelease publish glob", "dist/*.zip"},
	} {
		src := release
		if strings.HasPrefix(want.what, "canary.yml") {
			src = canary
		}
		if !strings.Contains(src, want.sub) {
			t.Errorf("%s is missing %q — the Windows zip would be built but never published, or published "+
				"under a name the install command does not resolve", want.what, want.sub)
		}
	}
}

func readWorkflow(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// THE STABLE ASSET NAME IS A CONTRACT WITH THE DOCUMENTED COMMAND, not a style choice: the canary's whole
// usability rests on `-Version canary` resolving to these exact files, so a version number creeping into
// the asset name would break the published instructions silently.
func TestCanaryAssetsUseTheStableName(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/canary.yml")
	if err != nil {
		t.Fatalf("read canary.yml: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		`ASSET="orchicon_canary_${{ matrix.os }}_${{ matrix.arch }}"`,
		`tar -czf "${ASSET}.tar.gz" "$ASSET"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("canary.yml no longer builds %s — the documented `-Version canary` command depends on "+
				"that exact asset name", want)
		}
	}
	// The prerelease flag is what keeps `releases/latest` (and therefore the normal install) untouched.
	for _, want := range []string{"gh release create canary --prerelease", "gh release edit canary --prerelease"} {
		if !strings.Contains(src, want) {
			t.Errorf("canary.yml no longer marks the release as a prerelease (%q) — without it the canary "+
				"becomes `releases/latest` and every existing install would start resolving to develop", want)
		}
	}
}

// ---------------------------------------------------------------------------
// DESKTOP PACKAGING (macOS .app, Windows WebView2, Linux WebKitGTK).
//
// The three desktop jobs are asserted here for the same reason the canary matrix is: a
// packaging step that is not checked is a packaging step that rots silently until a release
// produces an artifact nobody can install. Each assertion below names the acceptance
// criterion it protects.
// ---------------------------------------------------------------------------

const releaseWorkflowPath = "../../.github/workflows/release.yml"

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestReleaseMatrixIsStillCliOnly pins the decision that the desktop builds are SEPARATE
// single-OS jobs rather than matrix entries.
//
// This is not style. matrixPlatforms scans EVERY `- os:` line in the file, so a desktop
// matrix would be read as a CLI platform and TestCanaryMatchesReleaseMatrix would fail with
// a message about the canary — pointing at the wrong file. The clean separation is also what
// keeps the CLI release independent of the desktop builds (AC6).
func TestReleaseMatrixIsStillCliOnly(t *testing.T) {
	got := matrixPlatforms(t, releaseWorkflowPath)
	// windows/amd64 IS a CLI platform now: the plane and its host services run as native host
	// processes, so a released orchicon.exe is what a Windows user installs. What this guard is
	// about is unchanged — no DESKTOP entry here, because the app is CGO + a native webview.
	want := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the release build matrix is no longer the CLI-only set:\n  got:  %v\n  want: %v\n"+
			"Desktop builds belong in their own single-OS jobs (they are not CLI platforms and "+
			"a matrix would break TestCanaryMatchesReleaseMatrix).", got, want)
	}
}

// TestReleasePublishesDesktopArtifacts asserts AC1 (macOS signed + notarised + stapled),
// AC2 (Windows installer carries the WebView2 Evergreen bootstrapper), AC3 (Linux declares
// WebKitGTK) and AC4 (a desktop artifact per platform, from the right runner).
func TestReleasePublishesDesktopArtifacts(t *testing.T) {
	src := mustRead(t, releaseWorkflowPath)
	var mac, win, linux = -1, -1, -1
	jobs := jobsIn(src)
	for i, j := range jobs {
		if !strings.Contains(j.body, "wails build") {
			continue
		}
		switch {
		case strings.Contains(j.body, "runs-on: macos-"):
			mac = i
		case strings.Contains(j.body, "runs-on: windows-"):
			win = i
		case strings.Contains(j.body, "runs-on: ubuntu-"):
			linux = i
		}
	}
	for _, c := range []struct {
		idx  int
		what string
	}{
		{mac, "a desktop job on a macOS runner (`runs-on: macos-…`) does not exist — a signed, " +
			"notarised .app CANNOT be produced on ubuntu-latest (AC1)"},
		{win, "a desktop job on a Windows runner (`runs-on: windows-…`) does not exist — the NSIS " +
			"installer and its WebView2 bootstrapper need Windows (AC2)"},
		{linux, "a desktop job on an Ubuntu runner (`runs-on: ubuntu-…`) does not exist (AC3)"},
	} {
		if c.idx < 0 {
			t.Fatal(c.what)
		}
	}

	// AC1: the job builds a UNIVERSAL .app and runs the sign → notarise → staple →
	// assert chain. The chain itself is asserted against the SCRIPT it calls, not
	// against this file: a substring check on the workflow would be satisfied by a
	// comment mentioning `spctl`, which is exactly the kind of vacuous pass these
	// guards exist to avoid.
	macBody := jobs[mac].body
	for _, want := range []string{"-platform darwin/universal", "deploy/desktop/macos/sign-and-notarize.sh"} {
		if !strings.Contains(macBody, want) {
			t.Errorf("the macOS desktop job does not contain %q — without the full sign → notarise → "+
				"staple chain Gatekeeper blocks the .app on a clean Mac (AC1)", want)
		}
	}
	notarize := mustRead(t, "../../deploy/desktop/macos/sign-and-notarize.sh")
	for _, want := range []string{
		"codesign",              // signs with the Developer ID identity
		"notarytool submit",     // notarisation, with --wait so a rejection fails the job
		"stapler staple",        // the ticket travels in the bundle, so first launch works offline
		"spctl -a -vvv",         // the assertion: fails the job if Gatekeeper would still block it
		"entitlements.plist",    // the JIT entitlements WKWebView/JavaScriptCore need under hardened runtime
		": \"${MACOS_CERT_P12:", // a missing secret must fail, not ship an unsigned app
	} {
		if !strings.Contains(notarize, want) {
			t.Errorf("deploy/desktop/macos/sign-and-notarize.sh does not contain %q — without it the .app "+
				"is either unsigned (Gatekeeper blocks it) or notarised-but-unstapled (first launch fails "+
				"when Apple is unreachable) (AC1)", want)
		}
	}

	// AC2: the installer carries the bootstrapper; the flag is the whole provisioning story.
	winBody := jobs[win].body
	for _, want := range []string{"-webview2 download", "-nsis"} {
		if !strings.Contains(winBody, want) {
			t.Errorf("the Windows desktop job does not pass %q — a Windows 10 user would get a blank "+
				"window instead of a provisioned WebView2 runtime (AC2)", want)
		}
	}

	// AC3: the .deb declares the webview, and something produces both Linux artifacts.
	linuxBody := jobs[linux].body
	for _, want := range []string{"libwebkit2gtk", "nfpm", ".deb", "AppImage"} {
		if !strings.Contains(linuxBody, want) {
			t.Errorf("the Linux desktop job does not mention %q — the .deb must declare the WebKitGTK "+
				"dependency and the AppImage is the escape hatch for distros that dropped it (AC3)", want)
		}
	}

	// THE EMBED TRAP, again: the desktop app embeds the same frontend, so a job that builds
	// the app before the frontend ships a window with no UI in it.
	for _, i := range []int{mac, win, linux} {
		frontend := strings.Index(jobs[i].body, "npm run build")
		app := strings.Index(jobs[i].body, "wails build")
		if frontend < 0 {
			t.Errorf("desktop job %q never builds the frontend — the app embeds frontend/dist, and "+
				"`//go:embed all:frontend/dist` matches the tracked .gitkeep happily, so this would "+
				"compile and ship an app with no UI", jobs[i].name)
			continue
		}
		if frontend > app {
			t.Errorf("desktop job %q builds the app BEFORE the frontend — the embed cannot see it", jobs[i].name)
		}
	}
}

// TestReleaseDownloadsDesktopArtifactsOptional asserts AC6: desktop packaging can never
// block the CLI release.
//
// The release job takes BOTH its inputs and its `ASSETS` list from one download-artifact +
// `find artifacts` pair, so a desktop artifact can only join the release by a SECOND, optional
// download — and only if the `needs:` list does not mention a desktop job, because a failed
// dependency skips the job entirely rather than running it without those artifacts.
func TestReleaseDownloadsDesktopArtifactsOptional(t *testing.T) {
	src := mustRead(t, releaseWorkflowPath)
	if !strings.Contains(src, "pattern: 'orchicon-desktop-*'") {
		t.Error("release.yml never downloads the desktop artifacts (no `pattern: 'orchicon-desktop-*'`) — " +
			"the packages would be built and then left in the run, never published (AC4)")
	}
	for _, name := range []string{"orchicon-desktop-linux", "orchicon-desktop-darwin", "orchicon-desktop-windows"} {
		if !strings.Contains(src, "name: "+name) {
			t.Errorf("release.yml does not upload an artifact named %q — the download pattern and the "+
				"upload names are one contract", name)
		}
	}

	var release *job
	jobs := jobsIn(src)
	for i := range jobs {
		if jobs[i].name == "release" {
			release = &jobs[i]
		}
	}
	if release == nil {
		t.Fatal("release.yml has no `release:` job — did it get renamed?")
	}
	var needs string
	for _, l := range strings.Split(release.body, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "needs:") {
			needs = t
			break
		}
	}
	if needs == "" {
		t.Fatal("the release job has no needs: line")
	}
	for _, desktop := range []string{"desktop-linux", "desktop-macos", "desktop-windows"} {
		if strings.Contains(needs, desktop) {
			t.Errorf("the release job needs %q (%q): a failed desktop build would then SKIP the whole "+
				"release, so a webkit or signing problem could stop the CLI binaries from shipping (AC6)",
				desktop, needs)
		}
	}
	for _, want := range []string{"build", "guard"} {
		if !strings.Contains(needs, want) {
			t.Errorf("the release job's needs: line lost %q (%q)", want, needs)
		}
	}
	if !strings.Contains(release.body, "continue-on-error: true") {
		t.Error("the release job downloads desktop artifacts WITHOUT `continue-on-error: true` — any " +
			"desktop build failure would then fail the release job and take the CLI artifacts with it (AC6)")
	}
}

// TestReleaseUploadsEachDesktopAssetOnce — the release must never hand `gh release create`
// the same ASSET NAME twice.
//
// WHY THIS IS NOT HYPOTHETICAL, and why it is the AC6 failure in its least obvious form: the
// release job's FIRST `download-artifact` carries no `pattern`, so it already pulls EVERY
// artifact of the run — the CLI archives AND the three `orchicon-desktop-*` ones — into
// `artifacts/`. The optional desktop download below then puts the same files in
// `desktop-artifacts/`, so a naive `find artifacts desktop-artifacts` yields each desktop
// package twice. `gh release create` names an asset after its BASENAME, and the second upload
// of one name fails the whole release with HTTP 422 "already_exists" — with all three desktop
// builds GREEN, i.e. in the happy path. Packaging must not break the release, so the asset
// list has to be deduplicated.
func TestReleaseUploadsEachDesktopAssetOnce(t *testing.T) {
	var release *job
	jobs := jobsIn(mustRead(t, releaseWorkflowPath))
	for i := range jobs {
		if jobs[i].name == "release" {
			release = &jobs[i]
		}
	}
	if release == nil {
		t.Fatal("release.yml has no `release:` job — did it get renamed?")
	}
	start := strings.Index(release.body, "ASSETS=$(")
	if start < 0 {
		t.Fatal("the release job no longer builds an ASSETS= list — this guard cannot see what gets uploaded")
	}
	rest := release.body[start:]
	end := strings.Index(rest, `if [ -z "$ASSETS" ]`)
	if end < 0 {
		t.Fatal("could not find the end of the ASSETS= assignment (the `if [ -z \"$ASSETS\" ]` guard)")
	}
	assets := rest[:end]

	// The premise: BOTH download directories are searched, and every desktop package format
	// (plus the CLI tarballs) is matched. A format dropped here ships nothing and says nothing.
	for _, want := range []string{"artifacts", "desktop-artifacts", "*.tar.gz", "*.zip", "*.deb",
		"*.AppImage", "*_installer.exe"} {
		if !strings.Contains(assets, want) {
			t.Errorf("the release ASSETS list does not mention %q — that artifact would be built and "+
				"then silently left in the run instead of published (AC4)", want)
		}
	}

	// And the property: one entry per basename. The awk is the whole fix — without it the
	// desktop packages are uploaded twice and `gh release create` fails the release.
	if !strings.Contains(assets, "seen[$NF]++") {
		t.Error("the release ASSETS list is NOT deduplicated by basename. The first download has no " +
			"pattern, so it already contains the orchicon-desktop-* artifacts; the optional desktop " +
			"download repeats them, and `gh release create` fails with HTTP 422 already_exists when one " +
			"asset name is passed twice. With all three desktop builds green, that breaks the release " +
			"that AC6 says they must never break. Pipe the find through `awk -F/ '!seen[$NF]++'`.")
	}
}

// TestDesktopWebviewDependencyDeclared pins the two halves of AC3, because either half alone
// is worthless: a .deb that declares the dependency but an app that fails opaquely when it is
// missing is half a fix, and a launcher that explains the problem but a package that pulls in
// nothing is the other half.
func TestDesktopWebviewDependencyDeclared(t *testing.T) {
	nfpm := mustRead(t, "../../deploy/desktop/nfpm.yaml")
	if !strings.Contains(nfpm, "libwebkit2gtk-4.0-37") {
		t.Error("deploy/desktop/nfpm.yaml does not declare libwebkit2gtk-4.0-37 — the .deb would " +
			"install a binary that cannot start (AC3)")
	}
	if !strings.Contains(nfpm, "/usr/bin/orchicon-desktop") {
		t.Error("deploy/desktop/nfpm.yaml does not install the launcher at /usr/bin/orchicon-desktop — " +
			"without it the WebKitGTK check cannot run before the dynamic loader does (AC3)")
	}

	launcher := mustRead(t, "../../deploy/desktop/linux/orchicon-desktop")
	for _, want := range []string{"ldconfig", "libwebkit2gtk-4.0.so.37", "sudo apt install libwebkit2gtk-4.0-37"} {
		if !strings.Contains(launcher, want) {
			t.Errorf("deploy/desktop/linux/orchicon-desktop does not contain %q — the launcher must DETECT "+
				"the missing webview and name the command that fixes it, or the user gets an opaque "+
				"loader error (or nothing at all from a menu launch) (AC3)", want)
		}
	}

	apprun := mustRead(t, "../../deploy/desktop/appimage/AppRun")
	if !strings.Contains(apprun, "webkit") || !strings.Contains(apprun, "ldconfig") {
		t.Error("deploy/desktop/appimage/AppRun no longer checks for a webview — an AppImage built " +
			"without its bundled libraries must say so, not fail with a loader error (AC3)")
	}
}

// TestDesktopSigningChainIsInTheScript — the sign → notarise → staple → assert chain must live in the
// SCRIPT the macOS job runs, and the job must actually RUN it.
//
// The distinction is the whole point: a substring check against release.yml would be satisfied by a YAML
// COMMENT that mentions `spctl` while the job shipped an unsigned .app. So this reads
// deploy/desktop/macos/sign-and-notarize.sh for the chain, and release.yml only for the invocation itself
// (a `bash deploy/desktop/macos/…` line, which is a run: command, not prose).
func TestDesktopSigningChainIsInTheScript(t *testing.T) {
	script := mustRead(t, "../../deploy/desktop/macos/sign-and-notarize.sh")

	// ORDER MATTERS: signing must precede submission, and the staple must precede the assertion — a
	// notarised-but-unstapled app fails on the user's first launch whenever Apple is unreachable.
	order := []string{"codesign --force", "notarytool submit", "stapler staple", "spctl -a -vvv"}
	prev := -1
	for _, want := range order {
		i := strings.Index(script, want)
		if i < 0 {
			t.Fatalf("deploy/desktop/macos/sign-and-notarize.sh does not contain %q — the .app would reach "+
				"users unsigned or unstapled, and Gatekeeper blocks it on a clean Mac (AC1)", want)
		}
		if i < prev {
			t.Errorf("%q appears BEFORE the preceding step in sign-and-notarize.sh — the chain must be "+
				"sign → notarise → staple → assert, in that order (AC1)", want)
		}
		prev = i
	}

	// A missing secret must FAIL the job. Shipping an unsigned .app because CI had no certificate is
	// precisely the failure AC1 describes, and it would be silent.
	for _, secret := range []string{"MACOS_CERT_P12", "MACOS_CERT_PASSWORD", "MACOS_NOTARY_APPLE_ID",
		"MACOS_NOTARY_TEAM_ID", "MACOS_NOTARY_PASSWORD"} {
		if !strings.Contains(script, ": \"${"+secret+":?") {
			t.Errorf("sign-and-notarize.sh does not guard ${%s} with `: \"${…:?}\"` — an unset secret would "+
				"not fail the job, so it would ship an unsigned .app (AC1)", secret)
		}
	}

	// And the workflow must RUN it. `bash deploy/desktop/macos/sign-and-notarize.sh` inside a run: block is
	// an invocation; the string on its own could be a comment, which is why the script is read above.
	src := mustRead(t, releaseWorkflowPath)
	invoked := false
	for _, j := range jobsIn(src) {
		if strings.Contains(j.body, "runs-on: macos-") && strings.Contains(j.body, "bash deploy/desktop/macos/sign-and-notarize.sh") {
			invoked = true
		}
	}
	if !invoked {
		t.Error("no macOS job RUNS `bash deploy/desktop/macos/sign-and-notarize.sh` — the signing chain " +
			"exists but nothing calls it, so the published .app is unsigned (AC1)")
	}
}

// TestCanaryPublishesAnImageThatIsNotLatest — the canary must ship a CONTAINER IMAGE, and must never touch
// the floating release tag.
//
// WHY IT NEEDS ONE AT ALL: with binaries only, `-Version canary` installed a canary BINARY into a stack
// running the last RELEASED image, so a test of "does re-installing converge the container?" had nothing
// newer to converge to — which is exactly how the operator's Windows test came back inconclusive.
//
// AND WHY NOT :latest: that tag is the published release's, and repointing it at develop would change what
// every existing install pulls on its next start. The canary is additive or it is a liability.
func TestCanaryPublishesAnImageThatIsNotLatest(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/canary.yml")
	if err != nil {
		t.Fatalf("read canary.yml: %v", err)
	}
	src := string(b)

	for _, want := range []string{
		// The image is built from the same Dockerfile and pushed under a canary-only tag.
		"deploy/container/Dockerfile",
		"ghcr.io/beardedparrott/orchicon:canary",
		"docker push ghcr.io/beardedparrott/orchicon:canary",
		// Pushing to GHCR needs the scope; without it the job 403s and the canary silently ships binaries
		// with no image.
		"packages: write",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("canary.yml no longer %q — a canary without its own image cannot exercise the container half of an install", want)
		}
	}
	// NEVER the floating release tag.
	if strings.Contains(src, "orchicon:latest") {
		t.Error("canary.yml references orchicon:latest — that tag belongs to the published release, and pointing it at develop would change what every existing install pulls")
	}
}

// TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst — the silent-embed trap.
//
// THE FRONTEND IS EMBEDDED WITH `//go:embed all:frontend/dist`, and `frontend/dist/.gitkeep` is tracked
// EXACTLY SO THE EMBED ALWAYS HAS A TARGET. The consequence is brutal: a job that skips the frontend build
// COMPILES PERFECTLY and produces a valid archive whose binary serves NO UI — the embed matched the
// placeholder and nothing else. Nothing fails anywhere. The defect is visible only to whoever installs it
// and opens the URL.
//
// That is exactly what the canary did: its binary job packaged a frontend-less binary, a canary install
// replaced the operator's working host plane with it, and the browser got `{"error":"missing credentials"}`
// — the request fell through to the auth-guarded catch-all, because there was no static asset to serve.
//
// So the rule is asserted over EVERY job in BOTH workflows rather than for the one that broke: a job that
// builds ./cmd/orchicon must build the frontend earlier in the same job. The set is enumerated by scanning
// the files, so a job added later is covered the moment it exists.
func TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst(t *testing.T) {
	for _, wf := range []string{"../../.github/workflows/release.yml", "../../.github/workflows/canary.yml"} {
		b, err := os.ReadFile(wf)
		if err != nil {
			t.Fatalf("read %s: %v", wf, err)
		}
		for _, job := range jobsIn(string(b)) {
			if !strings.Contains(job.body, "./cmd/orchicon") {
				continue // does not build the binary: not this rule's business
			}
			if !strings.Contains(job.body, "go build") {
				continue
			}
			frontend := strings.Index(job.body, "npm run build")
			binary := strings.Index(job.body, "go build")
			if frontend < 0 {
				t.Errorf("%s job %q builds ./cmd/orchicon without building the frontend — it will compile and "+
					"package a binary with NO UI, because go:embed matches the tracked frontend/dist/.gitkeep "+
					"and nothing else. Nothing else in the pipeline will notice.", wf, job.name)
				continue
			}
			if frontend > binary {
				t.Errorf("%s job %q builds the binary BEFORE the frontend, so the embed cannot see it", wf, job.name)
			}
		}
	}
}

// job is one workflow job, kept as its raw block so the ORDER of its steps can be inspected — which is the
// property that matters here, not merely presence.
type job struct {
	name string
	body string
}

// jobsIn splits a workflow's `jobs:` mapping into its top-level jobs by indentation (a job name sits at two
// spaces, which is how these files are written and why a line scan is honest here).
func jobsIn(src string) []job {
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "jobs:" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []job
	cur := -1
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l != "" && !strings.HasPrefix(l, " ") {
			break // left the jobs mapping
		}
		if strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "    ") && strings.HasSuffix(l, ":") {
			if cur >= 0 {
				out = append(out, job{name: strings.TrimSuffix(strings.TrimSpace(lines[cur]), ":"),
					body: strings.Join(lines[cur+1:i], "\n")})
			}
			cur = i
		}
	}
	if cur >= 0 {
		out = append(out, job{name: strings.TrimSuffix(strings.TrimSpace(lines[cur]), ":"),
			body: strings.Join(lines[cur+1:], "\n")})
	}
	return out
}

// ---------------------------------------------------------------------------
// THE PRUNE WORKFLOW REPORTED SUCCESS WHILE DELETING NOTHING, FOR ITS WHOLE LIFE.
//
// `prune-releases.yml` enforces the release cap. Its job has no `actions/checkout` (it deletes
// releases; it never needed the source), so `gh` could not work out WHICH repository it was
// talking to and every `gh release list` failed with "fatal: not a git repository". That
// failure was invisible twice over:
//
//   - `set -e` did not catch it, because the failing command was not the last in the pipeline
//     and bash returns a pipeline's LAST status without `pipefail`;
//   - so the empty list flowed into a `while` loop that did nothing, and the step exited 0.
//
// Measured consequence: 13 releases against `MAX_RELEASES: 5`, with a green run every time.
//
// These two assertions cover the CLASS rather than the file, over every workflow that exists —
// because the next job to call `gh` without a checkout, or to pipe a fallible command, will make
// exactly the same mistake. Both scanners strip comments first: the notes explaining this bug
// quote `gh release list` and mention pipelines, and a detector tripped by its own documentation
// would be worse than none.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no workflow files found — this guard would pass vacuously")
	}
	return paths
}

// stripComment removes a line's YAML/shell comment (and trims it).
func stripComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// TestEveryWorkflowThatRunsGhCanResolveTheRepo pins the first half: a workflow invoking `gh`
// must either check the repo out or tell gh which repo it is (`GH_REPO`).
func TestEveryWorkflowThatRunsGhCanResolveTheRepo(t *testing.T) {
	for _, path := range workflowFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// COMMENTS ARE STRIPPED BEFORE EVERY CHECK HERE, and it is not tidiness: the note this
		// change adds to prune-releases.yml QUOTES both "GH_REPO" and "actions/checkout" while
		// explaining the bug, so a raw substring test kept passing after the real env var was
		// deleted — a detector tripped by its own documentation. Caught by negative control.
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			code = append(code, stripComment(line))
		}
		body := strings.Join(code, "\n")
		usesGh := false
		for _, l := range code {
			if strings.HasPrefix(l, "gh ") || strings.Contains(l, "| gh ") {
				usesGh = true
				break
			}
		}
		if !usesGh {
			continue
		}
		if !strings.Contains(body, "actions/checkout") && !strings.Contains(body, "GH_REPO") {
			t.Errorf("%s runs `gh` but gives it no repository context: with no actions/checkout,\n"+
				"gh cannot resolve the repo and EVERY gh call fails with \"fatal: not a git "+
				"repository\".\nAdd `GH_REPO: ${{ github.repository }}` to the step's env (or check "+
				"the repo out).", filepath.Base(path))
		}
	}
}

// isPipe reports whether a line contains a shell PIPE — a `|` that is not part of `||`.
//
// THE DISTINCTION IS NOT PEDANTRY. `||` is logical OR, and `cmd || true` is the standard way to
// tolerate a failure, which is the OPPOSITE of what this guard is looking for: the first draft
// of it counted `cp README.md "$ASSET/" 2>/dev/null || true` as a pipeline and demanded
// pipefail for a block that has no pipeline at all. A detector that cries wolf on a common
// idiom is a detector people turn off.
func isPipe(l string) bool {
	for i := 0; i < len(l); i++ {
		if l[i] != '|' {
			continue
		}
		if (i > 0 && l[i-1] == '|') || (i+1 < len(l) && l[i+1] == '|') {
			continue // half of a `||`
		}
		return true
	}
	return false
}

// TestEveryWorkflowPipelineUsesPipefailAtLeastWhereItMatters pins the second half: a `run:`
// block containing a shell pipeline must set `pipefail`, or a failing command's status is
// discarded in favour of the last command's — which is how a hard failure becomes a green run.
func TestEveryWorkflowPipelineUsesPipefailAtLeastWhereItMatters(t *testing.T) {
	for _, path := range workflowFiles(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); i++ {
			// A multi-line `run: |` block. Its body is the following lines, more indented.
			if !strings.HasSuffix(strings.TrimSpace(lines[i]), "run: |") {
				continue
			}
			indent := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
			var body []string
			for j := i + 1; j < len(lines); j++ {
				l := lines[j]
				if strings.TrimSpace(l) == "" {
					body = append(body, "")
					continue
				}
				if len(l)-len(strings.TrimLeft(l, " ")) <= indent {
					break // dedented: the block ended
				}
				body = append(body, stripComment(l))
			}
			hasPipe, hasPipefail := false, false
			for _, l := range body {
				if isPipe(l) {
					hasPipe = true
				}
				if strings.Contains(l, "pipefail") {
					hasPipefail = true
				}
			}
			if hasPipe && !hasPipefail {
				t.Errorf("%s line %d: a `run: |` block contains a pipeline but does not set "+
					"`pipefail`.\nWithout it the block's status is its LAST command's, so a "+
					"failing `gh`/`curl`/`docker` at the head of the pipe is swallowed and the "+
					"step reports success.\nUse `set -euo pipefail`.", filepath.Base(path), i+1)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// THE DESKTOP APP — a job that is like no other in these workflows.
//
// It is CGO + a native webview per OS, so it is not a cross-compile, and it has
// two traps specific to it:
//
//  1. `matrixPlatforms` above pairs an `os:` line with a following `arch:` line
//     and `t.Fatalf`s when a workflow it reads has no such pairs. The desktop
//     matrix is `os:`-ONLY, so it must NOT be routed through that scanner — a
//     separate scanner reads it.
//  2. `TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst` matches
//     "./cmd/orchicon" as a SUBSTRING, which also matches
//     "./cmd/orchicon-desktop". That test skips jobs with no `go build`, and
//     the desktop job builds with `wails build` — so the ordering rule needs its
//     own assertion here rather than riding on that skip.

// desktopMatrixOSes reads the desktop job's runner list.
func desktopMatrixOSes(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, job := range jobsIn(string(b)) {
		if job.name != "desktop" {
			continue
		}
		for _, line := range strings.Split(job.body, "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "os:") || !strings.Contains(trimmed, "[") {
				continue
			}
			list := trimmed[strings.Index(trimmed, "[")+1:]
			list = strings.TrimSuffix(strings.TrimSpace(list), "]")
			var out []string
			for _, part := range strings.Split(list, ",") {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	return nil
}

// TestReleaseBuildsDesktopAppPerOS — the app ships from the release, or it does
// not ship at all.
func TestReleaseBuildsDesktopAppPerOS(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	var found *job
	jobs := jobsIn(string(b))
	for i := range jobs {
		if jobs[i].name == "desktop" {
			found = &jobs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("release.yml has no `desktop` job — the desktop app would never be built for a release")
	}
	for _, want := range []struct{ token, why string }{
		{"actions/upload-artifact", "the built app has to be published as an artifact"},
		{"wails build", "the app is packaged by the Wails CLI, not by `go build`"},
		{"libwebkit2gtk-4.1-dev", "Linux needs the 4.1 webview development package"},
		{"webkit2_41", "wails picks the WebKitGTK ABI by BUILD TAG: without `webkit2_41` its cgo files ask " +
			"pkg-config for webkit2gtk-4.0, which no current Ubuntu ships, so the job installs 4.1 and then " +
			"fails to build"},
	} {
		if !strings.Contains(found.body, want.token) {
			t.Errorf("the desktop job does not mention %q — %s", want.token, want.why)
		}
	}
}

// TestDesktopMatrixCoversLinuxMacOSWindows — AC1's build matrix. Three operating
// systems, each on its own runner, because the webview is a per-OS native
// library.
func TestDesktopMatrixCoversLinuxMacOSWindows(t *testing.T) {
	got := desktopMatrixOSes(t, "../../.github/workflows/release.yml")
	want := []string{"ubuntu-latest", "macos-latest", "windows-latest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the desktop matrix runs on %v, want %v — the app must build on Linux, macOS and Windows", got, want)
	}
}

// TestDesktopJobBuildsFrontendBeforeBinary — the silent-embed trap, asserted for
// the job that `TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst` cannot see
// (it has no `go build`, so that scanner skips it).
func TestDesktopJobBuildsFrontendBeforeBinary(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	for _, job := range jobsIn(string(b)) {
		if job.name != "desktop" {
			continue
		}
		frontend := strings.Index(job.body, "npm run build")
		binary := strings.Index(job.body, "wails build")
		if frontend < 0 {
			t.Fatal("the desktop job never builds the frontend — go:embed would match frontend/dist/.gitkeep " +
				"and the app would open an empty window")
		}
		if binary >= 0 && frontend > binary {
			t.Fatal("the desktop job builds the app BEFORE the frontend, so the embed cannot see the bundle")
		}
		return
	}
	t.Fatal("no desktop job found")
}

// TestDesktopNotInCrossPlatforms — the brief's explicit constraint: the desktop
// app is NOT a CGO_ENABLED=0 cross-compile and must not be folded into the loop
// that is.
func TestDesktopNotInCrossPlatforms(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	src := string(b)
	i := strings.Index(src, "CROSS_PLATFORMS")
	if i < 0 {
		t.Fatal("Makefile has no CROSS_PLATFORMS")
	}
	// A make list continues across lines ending in a backslash; stop at the first
	// line that does not, so the scan cannot wander into the desktop targets.
	lines := strings.Split(src[i:], "\n")
	var listBuf strings.Builder
	for _, line := range lines {
		listBuf.WriteString(line)
		listBuf.WriteString("\n")
		if !strings.HasSuffix(strings.TrimSpace(line), "\\") {
			break
		}
	}
	list := listBuf.String()
	if strings.Contains(list, "desktop") {
		t.Fatalf("CROSS_PLATFORMS mentions the desktop app:\n%s\nIt is CGO + a native webview and cannot be "+
			"produced by the CGO_ENABLED=0 cross-compile loop — it builds per OS in release.yml instead", list)
	}
	// And the per-OS targets must exist, or the release job has nothing to call.
	for _, want := range []string{"desktop-build:", "desktop-test:", "desktop-test-live:"} {
		if !strings.Contains(src, want) {
			t.Errorf("Makefile has no %q target", want)
		}
	}
}

// TestDesktopBuildRunsInTheWailsProjectDirectory — the Wails CLI has NO project-dir
// flag: `wails build` loads ./wails.json from its WORKING DIRECTORY
// (wails/v2/internal/project.Load -> os.Getwd). wails.json lives in
// cmd/orchicon-desktop, so a job that runs the CLI from the repo root dies on a
// missing project file and the desktop artifact is never produced — the failure
// this assertion exists to prevent.
func TestDesktopBuildRunsInTheWailsProjectDirectory(t *testing.T) {
	const projectDir = "cmd/orchicon-desktop"
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	for _, job := range jobsIn(string(b)) {
		if job.name != "desktop" {
			continue
		}
		if !strings.Contains(job.body, "wails build") {
			t.Fatal("the desktop job no longer runs `wails build` — the app is packaged by the CLI")
		}
		if !strings.Contains(job.body, "working-directory: "+projectDir) &&
			!strings.Contains(job.body, "cd "+projectDir) {
			t.Fatalf("the desktop job does not run `wails build` from %s. The Wails CLI resolves its project "+
				"from the working directory (os.Getwd + ./wails.json) and has no project-dir flag, so from the "+
				"repo root the step fails with a missing wails.json", projectDir)
		}
		// The directory the job names must actually hold the Wails project file.
		if _, err := os.Stat(filepath.Join("../../", projectDir, "wails.json")); err != nil {
			t.Fatalf("%s/wails.json: %v — the working directory this job names has no Wails project file", projectDir, err)
		}
		// And the artifact it publishes must be where the CLI writes it: BuildDir
		// defaults to `build` INSIDE the project directory.
		if !strings.Contains(job.body, projectDir+"/build/bin/") {
			t.Errorf("the desktop job does not publish %s/build/bin/ — that is where `wails build` puts the binary", projectDir)
		}
		return
	}
	t.Fatal("no desktop job found")
}

// A DOWNLOADED BINARY THAT CANNOT RUN IS NOT A RELEASE, AND THE VERIFICATION STORY MUST NOT DEPEND ON
// WHICH INSTALLER THE USER RAN.
//
// There is no code-signing certificate behind this project, so there is no signed macOS binary and the
// Windows .exe an install places is unsigned. What replaces them is a PRECISE instruction
// plus a checksum the download can be held to — and both must be in place on every platform, in both
// installers, with the same words. These three assertions are that, as tests rather than as intentions.
// ---------------------------------------------------------------------------

const (
	guidanceBegin = "# >>> verification-guidance"
	guidanceEnd   = "# <<< verification-guidance"
)

// guidanceBlock returns the lines between the guidance markers, markers included.
//
// It fails loudly when a marker is missing: a block that silently vanished would pass a naive
// "both files contain the same thing (nothing)" comparison, which is the failure mode of every
// copy-paste equality check.
func guidanceBlock(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == guidanceBegin {
			in = true
		}
		if !in {
			continue
		}
		out = append(out, line)
		if strings.TrimSpace(line) == guidanceEnd {
			return out
		}
	}
	t.Fatalf("%s has no %q … %q block — the verification guidance is what a user has instead of a signed "+
		"binary, so it must exist and be comparable", path, guidanceBegin, guidanceEnd)
	return nil
}

// TestInstallersPrintTheSameVerificationGuidance pins ACM-4: the outcome must not depend on which
// installer was used. The two installers are separate programs in separate languages, so the only way
// they can be held to the same words is to compare the words.
func TestInstallersPrintTheSameVerificationGuidance(t *testing.T) {
	sh := guidanceBlock(t, "../../scripts/install.sh")
	ps := guidanceBlock(t, "../../scripts/install.ps1")
	if !reflect.DeepEqual(sh, ps) {
		t.Errorf("the installers print different verification guidance.\n  scripts/install.sh:\n%s\n  "+
			"scripts/install.ps1:\n%s\nA user who ran the PowerShell installer would take a different "+
			"security instruction away than one who ran the shell installer.",
			strings.Join(sh, "\n    "), strings.Join(ps, "\n    "))
		return
	}
	// AND IT MUST ACTUALLY SAY THE THINGS. An identical block that had been emptied would satisfy the
	// comparison above and tell the user nothing, so the substance is asserted too: the exact macOS
	// command, the fact that it does not disable Gatekeeper, and the Windows SmartScreen route out.
	body := strings.Join(sh, "\n")
	for _, want := range []string{
		"xattr -d com.apple.quarantine",
		"It does not disable Gatekeeper",
		"curl sets no quarantine bit",
		"Run anyway",
		"Windows protected your PC",
		"Unblock-File -LiteralPath",
		"SHA256SUMS",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the verification guidance no longer mentions %q — that line is the instruction a "+
				"user acts on, not decoration", want)
		}
	}
}

// TestReleasePublishesChecksums pins both halves of the checksum story, because the halves fail
// independently and the SECOND one fails silently.
//
// `artifacts/` is flat (`merge-multiple: true`), the checksums are generated there — and then the upload
// manifest is the `find` in the Create GitHub Release step. Generating the file without adding it to that
// find produces a perfectly good SHA256SUMS that is published to nobody, and every installer would then
// quietly skip verification forever ("older releases predate it"). So the find is asserted, not assumed.
func TestReleasePublishesChecksums(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, "> SHA256SUMS") || !strings.Contains(src, "sha256sum") {
		t.Error("release.yml no longer generates a SHA256SUMS file — the installers have nothing to verify " +
			"the download against, and the check they print would never fire")
	}
	findLine := ""
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "ASSETS=$(find") {
			findLine = line
			break
		}
	}
	if findLine == "" {
		t.Fatal("release.yml has no `ASSETS=$(find ...)` upload manifest — did the release step change shape?")
	}
	if !strings.Contains(findLine, "-name 'SHA256SUMS'") {
		t.Errorf("release.yml generates SHA256SUMS but does NOT list it in the upload manifest:\n  %s\n"+
			"The find IS the manifest, so the file is produced and never published, and every installer "+
			"silently falls back to no verification.", strings.TrimSpace(findLine))
	}
	// The consumer half: both installers must actually check it.
	if !strings.Contains(readFile(t, "../../scripts/install.sh"), "verify_checksum") {
		t.Error("scripts/install.sh no longer calls verify_checksum — the published checksums would be decoration")
	}
	if !strings.Contains(readFile(t, "../../scripts/install.ps1"), "SHA256SUMS") {
		t.Error("scripts/install.ps1 has no SHA256SUMS check — a Windows install would be unverified while a " +
			"Linux/macOS install is verified")
	}
}

// TestMacOSSigningJobRunsOnAMacOSRunner pins ACM-2 — and it is written as a CLASS check, because the
// failure it guards is silent: `codesign` does not exist on ubuntu-latest, so a signing step that drifted
// onto a Linux runner fails to sign, and the release still publishes an unsigned archive that everyone
// downstream calls "signed".
func TestMacOSSigningJobRunsOnAMacOSRunner(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	jobs := jobsIn(string(b))
	var signing *job
	for i := range jobs {
		if jobs[i].name == "sign-macos" {
			signing = &jobs[i]
		}
	}
	if signing == nil {
		t.Fatal("release.yml has no `sign-macos` job — the signed-artifact path is the thing that makes a " +
			"downloaded macOS binary runnable without the quarantine override, and it must be designed now " +
			"even while it is dormant")
	}
	for _, want := range []string{"runs-on: macos-14", "MACOS_SIGNING_ENABLED", "notarytool", "stapler"} {
		if !strings.Contains(signing.body, want) {
			t.Errorf("the sign-macos job no longer contains %q", want)
		}
	}
	// THE CLASS: nothing that signs or notarises may run anywhere but a macOS runner.
	//
	// COMMENTS ARE STRIPPED FIRST, and it is load-bearing: `jobsIn` attributes the comment block that
	// explains a job to the job ABOVE it, so the `build` job's body carries the paragraph that mentions
	// `codesign` and `notarytool` while explaining why the signing job exists. A detector tripped by its
	// own documentation is worse than none.
	for _, j := range jobs {
		var code []string
		for _, line := range strings.Split(j.body, "\n") {
			code = append(code, stripComment(line))
		}
		body := strings.Join(code, "\n")
		if !strings.Contains(body, "codesign") && !strings.Contains(body, "notarytool") {
			continue
		}
		if !strings.Contains(body, "runs-on: macos-") {
			t.Errorf("release.yml job %q signs or notarises but does not run on a macOS runner:\n"+
				"`codesign`/`notarytool` are macOS-only, so this job would fail — or, worse, be "+
				"rewritten into a cross-compile that publishes an unsigned artifact called signed.", j.name)
		}
	}
}

// TestReleaseInstallsTheSignedMacOSArchivesOverTheUnsignedOnes pins the half of ACM-2 that stays broken
// even once a signing job exists: the signed archives must be the ones the release PUBLISHES.
//
// The release job merges every artifact into ONE flat directory and writes SHA256SUMS there from a
// non-recursive glob. Two regressions therefore pass every other test while shipping an unsigned binary
// called signed:
//
//   - an upload path with `**` in it, so the signed archives land in a SUBDIRECTORY next to the unsigned
//     archives of the same name — the glob, the checksums and `gh release` all resolve to the flat
//     unsigned copy; and
//   - a release job that never downloads the signed artifact explicitly, leaving which copy survives the
//     merge to the (unspecified) artifact order.
//
// So both are asserted. Comments are stripped first: the explanation above names the very patterns the
// check looks for, and a detector tripped by its own documentation is worse than none.
func TestReleaseInstallsTheSignedMacOSArchivesOverTheUnsignedOnes(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	jobs := jobsIn(string(b))
	code := func(j job) string {
		var out []string
		for _, line := range strings.Split(j.body, "\n") {
			out = append(out, stripComment(line))
		}
		return strings.Join(out, "\n")
	}
	var signing, release *job
	for i := range jobs {
		switch jobs[i].name {
		case "sign-macos":
			signing = &jobs[i]
		case "release":
			release = &jobs[i]
		}
	}
	if signing == nil {
		t.Fatal("release.yml has no `sign-macos` job")
	}
	if release == nil {
		t.Fatal("release.yml has no `release` job — did it get renamed?")
	}

	signingCode := code(*signing)
	if strings.Contains(signingCode, "**/") {
		t.Errorf("the sign-macos upload uses a recursive path:\n%s\nAn artifact uploaded with `**` keeps "+
			"its subdirectories, so the signed archives land beside the unsigned ones instead of replacing "+
			"them and every published checksum still describes the UNSIGNED download.", signingCode)
	}
	if !strings.Contains(signingCode, "path: signed/*") {
		t.Errorf("the sign-macos job no longer stages its signed archives flat (`path: signed/*`), which is "+
			"what makes them replace the unsigned ones at the artifact root:\n%s", signingCode)
	}

	releaseCode := code(*release)
	if !strings.Contains(releaseCode, "sign-macos") {
		t.Error("the release job does not depend on sign-macos, so the signed archives may not exist yet " +
			"when it collects them — a release that races the job that signs its artifacts publishes the " +
			"unsigned build")
	}
	if !strings.Contains(releaseCode, "pattern: orchicon-darwin-signed") {
		t.Error("the release job never downloads `orchicon-darwin-signed` explicitly. The artifacts are " +
			"merged flat, so the signed and unsigned macOS archives have the SAME file names and the merge " +
			"order is unspecified: without this download the unsigned copy can be the one published, " +
			"checksums and all, while the run reports success")
	}
}
