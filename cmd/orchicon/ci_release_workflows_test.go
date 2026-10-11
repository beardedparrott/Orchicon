package main

import (
	"os"
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
