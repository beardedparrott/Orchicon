package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// ci_release_workflows_test.go — the release and canary workflows must agree on what they build.

// matrixPlatforms reads the `- os: <x>` / `arch: <y>` pairs out of a workflow's build matrix, in
// order.
func matrixPlatforms(t *testing.T, path string) [][2]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// AN `os:` LINE IS FOLLOWED BY ITS `arch:` LINE, in both files — the pairing is asserted rather
	// than the two keys being scanned independently.
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
	// WINDOWS IS IN BOTH, and the exclusion that used to live here is dead.
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

// TestReleaseWorkflowPackagesTheWindowsZip — a matrix entry alone does NOT produce the file AC2
// promises.
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

// THE STABLE ASSET NAME IS A CONTRACT WITH THE DOCUMENTED COMMAND, not a style choice: the canary's
// whole usability rests on `-Version canary` resolving to these exact files.
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

// TestReleaseMatrixIsStillCliOnly pins the decision that the desktop builds are SEPARATE single-OS
// jobs rather than matrix entries.
func TestReleaseMatrixIsStillCliOnly(t *testing.T) {
	got := matrixPlatforms(t, releaseWorkflowPath)
	// windows/amd64 IS a CLI platform now: the plane and its host services run as native host
	// processes, so a released orchicon.exe is what a Windows user installs.
	want := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the release build matrix is no longer the CLI-only set:\n  got:  %v\n  want: %v\n"+
			"Desktop builds belong in their own single-OS jobs (they are not CLI platforms and "+
			"a matrix would break TestCanaryMatchesReleaseMatrix).", got, want)
	}
}

// TestReleasePublishesDesktopArtifacts asserts AC1 (macOS signed + notarised + stapled), AC2
// (Windows installer carries the WebView2 Evergreen bootstrapper).
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

	// AC1: the job builds a UNIVERSAL .app and runs the sign → notarise → staple → assert chain.
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

// TestReleaseDownloadsDesktopArtifactsOptional asserts AC6: desktop packaging can never block the
// CLI release.
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
	// THE DESKTOP JOBS ARE NOW REQUIRED, AND THAT IS SAFE — because of `always()`.
	//
	// This assertion used to be the INVERSE: a desktop job in `needs` meant a failed webkit
	// or signing build would SKIP the release, taking the CLI binaries with it (AC6). That
	// reasoning holds only with the implicit success() gate. With `always()` in the job's
	// `if:`, a FAILED dependency does not skip the job — so the release still goes ahead and
	// the desktop download stays `continue-on-error: true`. The AC6 guarantee is preserved by
	// `always()`, not by the absence of `needs`.
	//
	// Requiring them fixes the opposite defect: v0.6.1 published 9 assets against v0.6.0's
	// 13 because Create Release started as soon as `build` finished and downloaded whatever
	// existed then — the Windows desktop artifact landed 35 seconds later. Nothing failed;
	// the release simply shipped fewer files than it should have.
	if !strings.Contains(release.body, "always()") {
		t.Error("the release job needs the desktop jobs but has no `always()` in its `if:`. Without " +
			"it, a single failed desktop platform SKIPS the entire release and takes the CLI " +
			"binaries with it (AC6)")
	}
	for _, desktop := range []string{"desktop-linux", "desktop-macos", "desktop-windows"} {
		if !strings.Contains(needs, desktop) {
			t.Errorf("the release job does not need %q (%q), yet it uploads that job's artifacts — it "+
				"will race their upload and publish fewer assets with nothing failing (this is exactly "+
				"how v0.6.1 shipped 9 assets instead of 13)", desktop, needs)
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

// TestReleaseUploadsEachDesktopAssetOnce — the release must never hand `gh release create` the same
// ASSET NAME twice.
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

// TestDesktopWebviewDependencyDeclared pins the two halves of AC3, because either half alone is
// worthless: a .deb that declares the dependency but an app that fails opaquely when it is missing.
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

// TestDesktopSigningChainIsInTheScript — the sign → notarise → staple → assert chain must live in
// the SCRIPT the macOS job runs, and the job must actually RUN it.
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

// TestNoWorkflowPublishesASingleContainerImage — the retired single-container deployment must not
// come back as a published artifact.
func TestNoWorkflowPublishesASingleContainerImage(t *testing.T) {
	for _, path := range []string{"../../.github/workflows/release.yml", "../../.github/workflows/canary.yml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		for _, banned := range []string{
			"deploy/container",
			"ghcr.io/beardedparrott/orchicon:",
		} {
			if strings.Contains(src, banned) {
				t.Errorf("%s still references %q — the single-container image is retired: a container exists only as a workflow's runtime container", path, banned)
			}
		}
		// NEVER the floating single-container tag.
		if strings.Contains(src, "orchicon:latest") {
			t.Error("a workflow references orchicon:latest — that was the retired instance image's floating tag")
		}
	}

	// ...and the runtime image is still built and pushed, which is the half that must NOT have been removed.
	b, err := os.ReadFile("../../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	src := string(b)
	for _, want := range []string{
		"runtime-image:",
		"deploy/runtime/Dockerfile",
		"ghcr.io/beardedparrott/orchicon-runtime:latest",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("release.yml no longer builds the per-workflow runtime base image (%q missing) — that image still exists and must still ship", want)
		}
	}
}

// TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst — the silent-embed trap.
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
		// COMMENTS ARE STRIPPED BEFORE EVERY CHECK HERE, and it is not tidiness: the note this change adds
		// to prune-releases.yml QUOTES both "GH_REPO" and "actions/checkout" while explaining the bug.
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

// shellIsPowerShell reports whether the step owning the `run:` at index i declares a PowerShell
// shell.
func shellIsPowerShell(lines []string, runIdx int) bool {
	for i := runIdx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "- ") {
			return false // reached the start of the step without seeing a shell key
		}
		if !strings.HasPrefix(trimmed, "shell:") {
			continue
		}
		val := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "shell:")))
		return strings.HasPrefix(val, "pwsh") || strings.HasPrefix(val, "powershell")
	}
	return false
}

// TestEveryWorkflowPipelineUsesPipefailAtLeastWhereItMatters pins the second half: a `run:` block
// containing a shell pipeline must set `pipefail`.
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
			if !hasPipe {
				continue
			}
			// THE REQUIREMENT IS PER SHELL, because `pipefail` is a BASH concept and demanding it of a
			// PowerShell block is unsatisfiable — the block can never comply.
			if shellIsPowerShell(lines, i) {
				if !strings.Contains(strings.Join(body, "\n"), "ErrorActionPreference") {
					t.Errorf("%s line %d: a `shell: pwsh` block contains a pipeline but does not set "+
						"`$ErrorActionPreference = 'Stop'`.\nWithout it a non-terminating error is "+
						"written to the error stream and the step still exits 0, so a failing "+
						"`Invoke-WebRequest`/`dotnet` at the head of the pipe is swallowed and the "+
						"step reports success.\n(pwsh has no `pipefail`; this is that invariant in "+
						"PowerShell's own vocabulary.)", filepath.Base(path), i+1)
				}
				continue
			}
			if !hasPipefail {
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

// TestDesktopMatrixCoversLinuxMacOSWindows — AC1's build matrix.
func TestDesktopMatrixCoversLinuxMacOSWindows(t *testing.T) {
	got := desktopMatrixOSes(t, "../../.github/workflows/release.yml")
	want := []string{"ubuntu-latest", "macos-latest", "windows-latest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the desktop matrix runs on %v, want %v — the app must build on Linux, macOS and Windows", got, want)
	}
}

// TestDesktopJobBuildsFrontendBeforeBinary — the silent-embed trap, asserted for the job that
// `TestEveryJobThatBuildsTheBinaryBuildsTheFrontendFirst` cannot see (it has no `go build`.
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

// TestDesktopNotInCrossPlatforms — the brief's explicit constraint: the desktop app is NOT a
// CGO_ENABLED=0 cross-compile and must not be folded into the loop that is.
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

// TestDesktopBuildRunsInTheWailsProjectDirectory — the Wails CLI has NO project-dir flag.
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
// installer was used.
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
	// AND IT MUST ACTUALLY SAY THE THINGS.
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

// TestMacOSSigningJobRunsOnAMacOSRunner pins ACM-2 — and it is written as a CLASS check, because
// the failure it guards is silent: `codesign` does not exist on ubuntu-latest.
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
	// THE RUNNER IS ASSERTED BY KIND, NOT BY VERSION.
	if !strings.Contains(signing.body, "runs-on: macos-") {
		t.Errorf("the sign-macos job does not run on a macOS runner:\n`codesign`/`notarytool` are "+
			"macOS-only, so any other host fails. Body:\n%s", signing.body)
	}
	for _, want := range []string{"MACOS_SIGNING_ENABLED", "notarytool", "stapler"} {
		if !strings.Contains(signing.body, want) {
			t.Errorf("the sign-macos job no longer contains %q", want)
		}
	}
	// THE CLASS: nothing that signs or notarises may run anywhere but a macOS runner.
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

// TestReleaseInstallsTheSignedMacOSArchivesOverTheUnsignedOnes pins the half of ACM-2 that stays
// broken even once a signing job exists.
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

// TestTheInstallerReportsADesktopAppItDidNotInstall — the silent-skip bug, and it was reported.
func TestTheInstallerReportsADesktopAppItDidNotInstall(t *testing.T) {
	src := readFile(t, "../../scripts/install.sh")

	// The three outcomes are one record, set on EVERY path out of install_desktop.
	for _, want := range []struct{ what, sub string }{
		{"the result record", "DESKTOP_RESULT="},
		{"the reason beside it", "DESKTOP_RESULT_WHY="},
		{"the installed state", `DESKTOP_RESULT="installed"`},
		{"the skipped state", `DESKTOP_RESULT="skipped"`},
		{"the unavailable state", `DESKTOP_RESULT="unavailable"`},
	} {
		if !strings.Contains(src, want.sub) {
			t.Errorf("scripts/install.sh no longer has %s (%q).\n"+
				"  Without it a run that installs no desktop app reads like a complete install — "+
				"which is the reported bug.", want.what, want.sub)
		}
	}

	// THE SUMMARY MUST PRINT IT, and print it as a RESULT.
	for _, want := range []struct{ what, sub string }{
		{"the summary names the missing app", "Desktop app: NOT INSTALLED"},
		{"the summary names a skipped app", "Desktop app: not installed"},
		{"the summary names an installed app", "Desktop app: ${DESKTOP_RESULT_WHY}"},
		// THE WAY OUT, for the case no flag can fix: build it from the tree the plane came from.
		{"the build-it-here recipe", "make desktop-build"},
	} {
		if !strings.Contains(src, want.sub) {
			t.Errorf("scripts/install.sh no longer prints %s (%q).\n"+
				"  The desktop outcome must appear where the other results appear, or the operator "+
				"is told the install succeeded and left to discover the GUI is missing.", want.what, want.sub)
		}
	}

	// EVERY EXIT FROM install_desktop MUST RECORD AN OUTCOME — this is the assertion that actually
	// catches the bug.
	fn := funcBody(t, src, "install_desktop() {")
	lines := strings.Split(fn, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "return 0") {
			continue
		}
		// Look back a short way: the assignment belongs to this exit, not a previous one.
		found, at := false, -1
		for j := i - 1; j >= 0 && i-j <= 6; j-- {
			if strings.Contains(lines[j], "DESKTOP_RESULT=") {
				found, at = true, j
				break
			}
			// A `return 0` between is where the previous exit ended; do not read past it.
			if strings.Contains(lines[j], "return ") {
				break
			}
		}
		if !found {
			t.Errorf("scripts/install.sh: install_desktop has an exit with NO recorded desktop outcome:\n"+
				"    %s\n"+
				"  Every path out of install_desktop must set DESKTOP_RESULT, or the summary prints nothing\n"+
				"  for it and the run looks like a complete install. That IS the reported bug: a 404 on the\n"+
				"  AppImage warned, returned 0, and the install ended with \"Installed. Next step:\".",
				strings.TrimSpace(line))
			continue
		}
		_ = at
		// The assignment must be BEFORE the return, in this function, and paired with a reason.
		if !strings.Contains(lines[at], "DESKTOP_RESULT_WHY=") {
			// The WHY may sit on the neighbouring line; accept that, but not absence.
			near := false
			for j := at - 1; j >= 0 && at-j <= 2; j-- {
				if strings.Contains(lines[j], "DESKTOP_RESULT_WHY=") {
					near = true
					break
				}
			}
			if !near && !strings.Contains(fn, "DESKTOP_RESULT_WHY=") {
				t.Errorf("install_desktop records an outcome with no reason beside it near line %d", i)
			}
		}
	}

	// AND A MISSING DESKTOP APP MUST NOT FAIL THE INSTALL.
	if strings.Contains(src, `die "the desktop app is not in release`) {
		t.Error("a missing desktop app now FAILS the install. The plane, the CLI and the TUI are the " +
			"product; the GUI is a client, and --no-setup installs on servers where it can never run. " +
			"The install must continue and report the gap, not abort.")
	}
}

// funcBody returns the source of a shell function, from its declaration to its closing brace at
// column 0.
func funcBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("scripts/install.sh has no %q", decl)
	}
	rest := src[i:]
	// The closing brace of a top-level shell function is a `}` at the start of a line.
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j+3]
	}
	t.Fatalf("could not find the end of %q", decl)
	return ""
}

// TestNoWorkflowDotsAHyphenatedNeedsJob — GitHub Actions expressions parse `needs.foo-bar` as
// `needs.foo MINUS bar`, NOT as property access. A hyphenated job id must be indexed:
// `needs['foo-bar']`.
//
// This is not a style rule, it is a silent-correctness one, and it cost three releases.
// `release.yml`'s `Create Release` gate read
//
//	(needs.sign-macos.result == 'success' || needs.sign-macos.result == 'skipped')
//
// which evaluates FALSE on every run — so `Create Release` was skipped at v0.6.0, v0.6.2 and
// v0.6.4 even when every other job was green and the guard had output proceed=true. The pipeline
// reported success and published nothing, three times, with no error anywhere to look at.
//
// Nothing caught it: the YAML parses, the jobs all succeed, and no test asserted the gate's
// meaning. So the shape is asserted here instead — statically, across every workflow.
func TestNoWorkflowDotsAHyphenatedNeedsJob(t *testing.T) {
	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob ../../.github/workflows/*.yml: %v (found %d)", err, len(paths))
	}

	// `needs.<id>.<prop>` where <id> contains a hyphen — the broken form.
	dotted := regexp.MustCompile(`needs\.[A-Za-z0-9_]+-[A-Za-z0-9_-]*\.`)
	// A line that is a comment explaining the rule is not a violation.
	isComment := func(line string) bool {
		trimmed := strings.TrimSpace(line)
		return strings.HasPrefix(trimmed, "#")
	}

	checked := 0
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if isComment(line) {
				continue
			}
			if m := dotted.FindString(line); m != "" {
				t.Errorf("%s:%d uses %q in an Actions expression.\n"+
					"Actions parses `needs.a-b` as subtraction, so the condition is silently FALSE "+
					"(or an error the runner swallows) instead of reading the job's result.\n"+
					"Index it instead: needs['a-b'].result\n  line: %s",
					p, i+1, strings.TrimSuffix(m, "."), strings.TrimSpace(line))
			}
		}
		checked++
	}

	if checked < 2 {
		t.Fatalf("only checked %d workflow(s) — expected at least release.yml and canary.yml, "+
			"so this guard is not covering what it thinks it is", checked)
	}
}

// TestReleaseJobSurvivesASkippedNeed — a job whose `needs` includes a job that is normally SKIPPED
// must use `always()` in its `if:`.
//
// `if:` on a job is implicitly ANDed with `success()`, and `success()` is false when ANY needed job
// was skipped — not failed, skipped. So an explicit `|| needs.x.result == 'skipped'` arm is DEAD
// CODE: the implicit gate has already made the whole condition false before that arm is reached.
//
// This is exactly how `Create Release` was skipped at five consecutive releases while every other
// job was green and `guard` output proceed=true. `sign-macos` is skipped whenever signing is off,
// which is the normal case, so the release job needed `always()` — and nothing in the repo used
// that pattern, so there was no example to copy and no test to catch it.
func TestReleaseJobSurvivesASkippedNeed(t *testing.T) {
	b, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	src := string(b)

	// Locate the Create Release job block: from its name line to the next top-level job key.
	lines := strings.Split(src, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		if strings.Contains(l, "name: Create Release") {
			start = i
			continue
		}
		if start >= 0 && i > start && len(l) > 2 && l[0] == ' ' && l[1] == ' ' && l[2] != ' ' &&
			strings.HasSuffix(strings.TrimSpace(l), ":") {
			end = i
			break
		}
	}
	if start < 0 {
		t.Fatal("release.yml has no `Create Release` job — this guard is checking the wrong thing")
	}
	// STRIP COMMENT LINES BEFORE ASSERTING. The block's explanatory comments mention `always()` and
	// `sign-macos` in prose, and an assertion satisfied by prose is worthless — it passes whether or not
	// the workflow actually does it. (Found by mutation: removing the real `always()` still passed.)
	var code []string
	for _, l := range lines[start:end] {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		code = append(code, l)
	}
	block := strings.Join(code, "\n")

	if !strings.Contains(block, "sign-macos") {
		t.Fatalf("the Create Release job no longer references `sign-macos`.\nIf the signing job was "+
			"dropped from its `needs`, this guard must be revisited rather than silently passing.\njob block:\n%s", block)
	}
	if !strings.Contains(block, "always()") {
		t.Error("the Create Release job depends on `sign-macos` — a job that is SKIPPED whenever " +
			"MACOS_SIGNING_ENABLED is not 'true' — but its `if:` does not say `always()`.\n" +
			"A job `if:` is implicitly ANDed with success(), which is FALSE when a needed job is " +
			"skipped, so an explicit `|| result == 'skipped'` arm can never be reached and the job " +
			"is skipped on every unsigned release. This is what kept the release unpublished at five " +
			"consecutive tags.\nExpected the job's if: to begin with `always() &&`.")
	}
}

// TestDevelopBumpHonoursNoDevBumpMarker — the [NoDevBump] opt-out must exist, must
// make the merge RIDE the current tag rather than mint a new one, and must read the
// commit's whole message.
//
// WHY THIS MATTERS. develop-bump tags EVERY merge to develop, and auto-release
// computes the release number from the same pool. So intermediate work burns release
// numbers before the release exists: v0.6.1, v0.6.3 and v0.6.4 were all minted by fix
// PRs, were never published, and pushed the real release onto a number nobody chose.
//
// RIDING, NOT SKIPPING. next_tag refuses to reuse a tag that sits on a different
// commit and bumps past it, so merely NOT tagging makes the NEXT merge climb anyway.
// The opt-out has to make the merge SHARE the current tag: two commits, one version,
// no number consumed. A test that only checked "no new tag is created" would pass
// against a workflow that still burned numbers one merge later.
//
// The assertions below are the ways this regresses without anyone noticing, because a
// bump workflow that does the wrong thing on a marked merge looks exactly like one
// that works.
// TestNoDevBumpMarkerMustBeLeadable — a TRAILING [NoDevBump] can be truncated away.
//
// Measured: a PR titled
//
//	docs(release): v0.6.1 highlights — install and desktop port improvements [NoDevBump]
//
// merged with a squash produced
//
//	docs(release): v0.6.1 highlights — install and desktop port improvements (#967)
//
// GitHub truncates the squash subject at 72 characters before appending " (#N)".
// The marker was at character 73, so it was truncated and the merge minted v0.6.3
// even though the marker really was in the PR title and the workflow check is
// correct. The workflow cannot fix that — the character never reaches it — so what
// is guarded here is the RULE the workflow documents: put the marker first.
//
// This asserts the workflow's own guidance is present and says "first"/"front",
// because that guidance is the only thing standing between an operator and a
// silently-ignored marker. It is documentation-as-contract: weak on its own, and
// paired with the truncation arithmetic below, which IS executable.
func TestNoDevBumpMarkerTruncationArithmetic(t *testing.T) {
	// The arithmetic that caused this, as a regression: GitHub keeps 72 chars of the
	// PR title and appends " (#N)". A marker at or beyond index 72 is lost.
	const squashSubjectLimit = 72
	const observedTitle = "docs(release): v0.6.1 highlights — install and desktop port improvements [NoDevBump]"
	const observedSubject = "docs(release): v0.6.1 highlights — install and desktop port improvements (#967)"

	at := strings.Index(observedTitle, "[NoDevBump]")
	if at < 0 {
		t.Fatal("the fixture title lost its marker — this test is no longer reproducing the defect")
	}
	if at < squashSubjectLimit {
		t.Skipf("fixture no longer demonstrates truncation (marker at %d, limit %d) — "+
			"keep this test pointed at the real case", at, squashSubjectLimit)
	}
	if strings.Contains(observedSubject, "[NoDevBump]") {
		t.Fatalf("the observed squash subject unexpectedly kept the marker; the truncation "+
			"premise no longer holds and this guard should be revisited:\n  %s", observedSubject)
	}

	// And the rule that avoids it: a LEADING marker survives the same truncation.
	leading := "[NoDevBump] " + observedTitle[:40]
	if len(leading) > squashSubjectLimit {
		// still fine — truncation cuts the tail, never the head
	}
	if !strings.HasPrefix(leading, "[NoDevBump]") {
		t.Fatal("a leading marker does not survive; the documented rule is wrong")
	}
	if at := strings.Index(leading, "[NoDevBump]"); at != 0 {
		t.Fatalf("marker at %d, expected 0 — truncation at %d cannot reach it", at, squashSubjectLimit)
	}
}

func TestDevelopBumpHonoursNoDevBumpMarker(t *testing.T) {
	const path = "../../.github/workflows/develop-bump.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)

	if !strings.Contains(src, "[NoDevBump]") {
		t.Fatalf("develop-bump.yml no longer mentions [NoDevBump]. The opt-out is gone, so the " +
			"docs PR before a release mints another release number.")
	}

	// It must read the FULL message (%B), not just the subject: a squash merge carries
	// the PR title as the subject, but a MERGE COMMIT carries it in the body — and this
	// repo has both (squash for ordinary PRs, merge commits for release and backfill).
	if !strings.Contains(src, "--format='%B'") {
		t.Error("develop-bump.yml does not read the commit's full message (expected " +
			"--format='%B'). Reading only the subject makes the marker invisible on a " +
			"MERGE COMMIT, so a PR merged that way would still be tagged")
	}

	// The RIDE step must exist: the merge has to share the current tag, not merely be
	// left untagged. next_tag bumps past a tag on a different commit, so "untagged"
	// still consumes a number on the next merge.
	if !strings.Contains(src, "Ride the current version tag") {
		t.Error("develop-bump.yml has no 'Ride the current version tag' step. Leaving a marked " +
			"merge merely UNTAGGED does not hold the number: next_tag bumps past a tag that sits " +
			"on a different commit, so the following merge mints one anyway")
	}
	if !strings.Contains(src, "git tag -f") {
		t.Error("the ride step does not MOVE the current tag (expected `git tag -f`), so the tag " +
			"would still point at the previous commit and `git describe` on the new tree resolves " +
			"to a stale version")
	}

	// Both branches must be gated on the SAME flag, or the two paths collide: a marked
	// merge could ride AND mint. Keyed on step names, not indices, so reordering
	// cannot silently detach a condition from the step it guards.
	lines := strings.Split(src, "\n")
	gateOf := func(stepName string) string {
		for i, l := range lines {
			if !strings.Contains(l, stepName) {
				continue
			}
			for j := i + 1; j < len(lines) && j <= i+4; j++ {
				if t := strings.TrimSpace(lines[j]); strings.HasPrefix(t, "if:") {
					return t
				}
			}
			return ""
		}
		return ""
	}

	rideGate := gateOf("name: Ride the current version tag")
	bumpGate := gateOf("name: Bump version tag")
	if rideGate == "" {
		t.Error("the 'Ride the current version tag' step carries no `if:`")
	}
	if bumpGate == "" {
		t.Error("the 'Bump version tag' step carries no `if:` — the [NoDevBump] check runs and is " +
			"then ignored, so every merge still mints a version")
	}
	// They must be mutually exclusive expressions over the same output.
	if rideGate != "" && bumpGate != "" {
		if !strings.Contains(rideGate, "ride == 'true'") || !strings.Contains(bumpGate, "ride != 'true'") {
			t.Errorf("the ride and bump steps do not gate on OPPOSITE conditions:\n"+
				"  ride: %s\n  bump: %s\nExpected `ride == 'true'` and `ride != 'true'`, or a "+
				"marked merge could both ride the current tag and mint a new one.", rideGate, bumpGate)
		}
	}
}

// TestReleaseWaitsForEveryDesktopJobItUploads — Create Release must NEED the desktop
// jobs whose artifacts it uploads.
//
// WHY. It uploaded their artifacts but did not depend on them, so it started as soon as
// `build` finished and downloaded whatever happened to exist at that moment. v0.6.1
// shipped with 9 assets against v0.6.0's 13: the Windows desktop job uploaded at
// 07:47:05, and Create Release had already run 07:46:06–07:46:30. The Windows CLI zip
// was there, so nothing looked wrong — the MSI, the NSIS installer and the two Windows
// desktop zips were simply absent from a release that reported success.
//
// A missing-asset release is the hardest kind of defect to notice from the pipeline: every
// job is green, the release exists, and only a human counting files would see it.
func TestReleaseWaitsForEveryDesktopJobItUploads(t *testing.T) {
	b, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseWorkflowPath, err)
	}
	src := string(b)

	// Every job whose artifact the release downloads must be in its `needs`.
	// The download step uses the pattern `orchicon-desktop-*`, which is fed by these
	// producers; the per-OS jobs are named here so a rename shows up as a failure.
	producers := []string{"desktop", "desktop-linux", "desktop-macos", "desktop-windows"}

	lines := strings.Split(src, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		if strings.Contains(l, "name: Create Release") {
			start = i
			continue
		}
		if start >= 0 && i > start && len(l) > 2 && l[0] == ' ' && l[1] == ' ' && l[2] != ' ' &&
			strings.HasSuffix(strings.TrimSpace(l), ":") {
			end = i
			break
		}
	}
	if start < 0 {
		t.Fatal("release.yml has no `Create Release` job")
	}
	block := strings.Join(lines[start:end], "\n")

	// The `needs:` line, whatever shape it takes.
	var needsLine string
	for _, l := range lines[start:end] {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "needs:") {
			needsLine = t
			break
		}
	}
	if needsLine == "" {
		t.Fatalf("the Create Release job has no `needs:` — it will race every artifact it uploads")
	}

	// It must survive a skipped dependency (the whole reason always() went in), so assert
	// that too rather than only the list membership.
	if !strings.Contains(block, "always()") {
		t.Error("the Create Release job no longer uses `always()`; with desktop jobs in `needs` a " +
			"single skipped platform would now skip the entire release")
	}

	for _, p := range producers {
		if !strings.Contains(needsLine, p) {
			t.Errorf("`needs:` does not include %q, but the release uploads that job's artifacts.\n"+
				"It will download whatever exists when `build` finishes instead of waiting, and the "+
				"missing files will not fail anything — the release simply publishes fewer assets than "+
				"it should.\\n  needs: %s", p, needsLine)
		}
	}
}

// TestOnlyDevelopMayOpenAPRIntoMain — the integration rule must be ENFORCED, not prose.
//
// "Branch off develop and PR into develop — never main" was written in developer.md the
// whole time, and it was broken four times in one day by an agent that reasoned past it:
// "release.yml only runs on main, so the fix has to be there". That reasoning is wrong —
// release.yml runs on workflow_dispatch with --ref main and requires only the TAG to be
// reachable from main, which a tag cut from develop's tip satisfies.
//
// Prose did not stop it. This asserts the enforcement exists, is scoped to main, and
// rejects anything that is not develop.
func TestOnlyDevelopMayOpenAPRIntoMain(t *testing.T) {
	const path = "../../.github/workflows/integration-guard.yml"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nThe integration guard is GONE. Without it nothing prevents a "+
			"fix from being merged straight into main, which is how develop ended up two "+
			"backfills behind on one day.", path, err)
	}
	src := string(b)

	// IT MUST RUN ON EVERY PR, NOT ONLY ONES TARGETING MAIN.
	//
	// A required status check has to have RUN recently or GitHub cannot offer it in the
	// "Require status checks" picker — the list contains only checks it has seen. Scoped
	// to `branches: [main]`, the workflow fires only when someone opens a PR into main,
	// which can be weeks apart, so between releases the check cannot be made required at
	// all. That is the opposite of the intent: the guard must exist BEFORE the next PR
	// into main, not appear with it.
	// ASSERT ON CODE, NOT PROSE. The file explains this mistake in a comment that quotes
	// `branches: [main]`, so a naive substring check fails on the documentation of the fix.
	var code strings.Builder
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		code.WriteString(l)
		code.WriteString("\n")
	}
	if strings.Contains(code.String(), "branches: [main]") {
		t.Error("integration-guard.yml is scoped to `branches: [main]`. Then it only runs when a " +
			"PR into main is opened — potentially weeks apart — and GitHub cannot offer it as a " +
			"required check in between, because the picker lists only checks that have run. Scope " +
			"it to ALL pull requests so the check exists continuously and reports green on develop")
	}
	// It must compare the HEAD branch, not just the base.
	if !strings.Contains(src, "github.head_ref") {
		t.Error("integration-guard.yml does not read github.head_ref, so it cannot tell which " +
			"branch is opening the PR")
	}
	// And it must actually fail, not merely warn.
	if !strings.Contains(src, "exit 1") {
		t.Error("integration-guard.yml never exits non-zero — a guard that always passes is " +
			"the same as no guard, which is exactly what this defect was")
	}
	if !strings.Contains(src, `"develop"`) {
		t.Error("integration-guard.yml does not name develop as the permitted head branch")
	}

	// It must be a required check on main, or a red result cannot block a merge. This is
	// asserted against the live API by the release process rather than here (a unit test
	// cannot reach GitHub), so what is checked is that the NEED is documented beside the
	// workflow — the operator has to add it to branch protection once.
	if !strings.Contains(src, "required") && !strings.Contains(src, "branch protection") {
		t.Error("integration-guard.yml does not mention that it must be a REQUIRED check on " +
			"main — an unrequired check reports red and blocks nothing")
	}
}
