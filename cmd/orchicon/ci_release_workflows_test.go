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
	// WINDOWS IS OUT OF BOTH, and that is the decision this guards: nothing consumes a Windows artifact
	// (the stack runs in WSL2 there, and the installer installs the Linux binary into the distro), so a
	// platform reappearing in one file without the other is the drift worth catching.
	for _, p := range release {
		if p[0] == "windows" {
			t.Errorf("release.yml builds %v again — Windows artifacts are deliberately not shipped; if that "+
				"decision has been reversed, update canary.yml and the docs in the same change", p)
		}
	}
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
	want := [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
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
