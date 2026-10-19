package main

// webkit_abi_consistency_test.go — THE WEBKITGTK ABI MUST AGREE EVERYWHERE IT IS NAMED.
//
// WebKitGTK ships TWO ABI families, and which one a build links is decided by a
// Wails BUILD TAG:
//
//	#cgo !webkit2_41 pkg-config: webkit2gtk-4.0
//	#cgo  webkit2_41 pkg-config: webkit2gtk-4.1
//
// Five places then have to agree about the answer: the tags the build uses, the
// runtime dependency the package DECLARES, the SONAME the launcher and the
// AppImage DETECT, the SONAMEs the AppImage BUNDLES, and the docs. They are
// spelled differently and the two families are NOT symmetric:
//
//	4.0 -> libwebkit2gtk-4.0.so.37  + libjavascriptcoregtk-4.0.so.18
//	4.1 -> libwebkit2gtk-4.1.so.0   + libjavascriptcoregtk-4.1.so.0
//
// THE DRIFT THIS PINS, all of it found in one sweep:
//
//  1. `libwebkit2gtk-4.1.so.37` was written into the launcher's check, the
//     AppRun check and the AppImage bundle list. No distribution ships it, so the
//     "or the 4.1 series" branch was UNREACHABLE: on a 4.1-only system (Ubuntu
//     24.04+, Fedora 40+, Arch) the launcher reported WebKitGTK missing and
//     refused to start, and the AppImage build copied nothing and then failed its
//     own "no libwebkit2gtk found to bundle" guard.
//  2. `webkit2_41` was put in wails.json, which EVERY `wails build` inherits —
//     including the .deb job on ubuntu-22.04, where libwebkit2gtk-4.1-dev does
//     not exist and libwebkit2gtk-4.0-dev is what the package depends on. That
//     turned a working release into a build failure, while nfpm.yaml went on
//     declaring a 4.0 dependency the build no longer produced.
//  3. `make desktop-build` hardcoded the tag, which is the mirror-image bug for a
//     developer on a 4.0-only host (Ubuntu 22.04, Debian 12).
//
// THE RULE: the ABI is a property of the BUILD HOST, except where an artifact
// deliberately promises one — the .deb promises 4.0 for older distros. So the tag
// lives where the decision is made (the detected host for a local build, the named
// artifact for packaging), never in wails.json, and every declaration must match.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// webkitSONAMEs is the CANONICAL table: ABI family → the SONAMEs that exist for
// it. Anything else is a name no distribution ships.
var webkitSONAMEs = map[string][]string{
	"4.0": {"libwebkit2gtk-4.0.so.37", "libjavascriptcoregtk-4.0.so.18"},
	"4.1": {"libwebkit2gtk-4.1.so.0", "libjavascriptcoregtk-4.1.so.0"},
}

// realSONAME reports whether a SONAME string is one that actually exists.
func realSONAME(so string) bool {
	for _, family := range webkitSONAMEs {
		for _, real := range family {
			if so == real {
				return true
			}
		}
	}
	return false
}

// abiFromTags maps a tag list to the ABI family it makes cgo demand.
func abiFromTags(tags string) string {
	if strings.Contains(tags, "webkit2_41") {
		return "4.1"
	}
	return "4.0"
}

// declaredABI maps nfpm's `depends:` entries to the ABI family they name.
// libwebkit2gtk-4.0-37 is Ubuntu's runtime package for 4.0; libwebkit2gtk-4.1-0
// is the one for 4.1.
func declaredABI(nfpm string) string {
	switch {
	case strings.Contains(nfpm, "libwebkit2gtk-4.1-0"):
		return "4.1"
	case strings.Contains(nfpm, "libwebkit2gtk-4.0-37"):
		return "4.0"
	}
	return ""
}

// TestNoImpossibleWebkitSONAME is the sweep: EVERY SONAME-looking literal in the
// repository must be one that exists.
//
// It is a sweep rather than a per-file assertion because the bad name was written
// in four different places (two checks, one bundle list, two messages) from one
// mistaken mental model — "the families differ by version number" — and fixing
// them one at a time is how the fifth gets missed.
func TestNoImpossibleWebkitSONAME(t *testing.T) {
	root := filepath.Join("..", "..")
	// Files that ship or describe the packaging. Deliberately excludes testdata
	// and the vendored toolchain.
	targets := []string{
		"deploy/desktop/linux/orchicon-desktop",
		"deploy/desktop/linux/orchicon-desktop.desktop",
		"deploy/desktop/appimage/AppRun",
		"deploy/desktop/appimage/build.sh",
		"deploy/desktop/nfpm.yaml",
		"deploy/desktop/README.md",
		"Makefile",
		".github/workflows/release.yml",
		".github/workflows/ci.yml",
		"cmd/orchicon-desktop/wails.json",
	}

	// A SONAME literal looks like lib<name>-4.<minor>.so.<major>.
	soRE := regexp.MustCompile(`lib(?:webkit2gtk|javascriptcoregtk)-4\.\d\.so\.\d+`)

	inspected := 0
	for _, rel := range targets {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		inspected++
		for _, line := range strings.Split(string(b), "\n") {
			// COMMENT LINES ARE SKIPPED: a name in prose is not executed, and these files
			// deliberately RECORD the historical bad name so the next reader does not
			// reintroduce it. The behavioural half of this guard (below) is what proves
			// the checks actually accept a real 4.1.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, so := range soRE.FindAllString(line, -1) {
				if !realSONAME(so) {
					t.Errorf("%s names %q, which no distribution ships.\n"+
						"The families are NOT symmetric:\n"+
						"  4.0 -> libwebkit2gtk-4.0.so.37 + libjavascriptcoregtk-4.0.so.18\n"+
						"  4.1 -> libwebkit2gtk-4.1.so.0  + libjavascriptcoregtk-4.1.so.0\n"+
						"A check written against a nonexistent name silently matches nothing: the "+
						"launcher then reports WebKitGTK missing on a system that has it, and the "+
						"AppImage bundle loop copies nothing and fails its own guard.", rel, so)
				}
			}
		}
	}
	if inspected == 0 {
		t.Fatal("no files were inspected — this test would pass vacuously")
	}
}

// TestPackageDependencyMatchesTheBuildTags — the .deb's declared runtime
// dependency must name the ABI its binary actually LINKS.
//
// This is the pairing that broke: nfpm.yaml declared 4.0 while the tags (via
// wails.json) selected 4.1. A package that depends on a library the binary does
// not use installs cleanly and then fails at the loader.
func TestPackageDependencyMatchesTheBuildTags(t *testing.T) {
	nfpm := mustRead(t, "../../deploy/desktop/nfpm.yaml")
	declared := declaredABI(nfpm)
	if declared == "" {
		t.Fatal("deploy/desktop/nfpm.yaml declares no libwebkit2gtk-<abi> runtime dependency")
	}

	// THE EFFECTIVE TAGS FOR THE .deb BUILD = wails.json (inherited by every
	// `wails build`) ∪ the tags the packaging job passes.
	wailsJSON := mustRead(t, "../../cmd/orchicon-desktop/wails.json")
	projectTags := jsonBuildTags(t, wailsJSON)

	release := mustRead(t, "../../.github/workflows/release.yml")
	jobTags := tagsFromJob(t, release, "desktop-linux:", "wails build")

	built := abiFromTags(projectTags + "," + jobTags)
	if built != declared {
		t.Errorf("the .deb declares WebKitGTK %s (nfpm.yaml) but its build links %s.\n"+
			"  wails.json build:tags   = %q\n"+
			"  desktop-linux job tags  = %q\n"+
			"One of them is wrong, and either way the package installs a dependency that does "+
			"not match the binary or omits the one it needs.", declared, built, projectTags, jobTags)
	}

	// And the ABI tag must NOT be baked into wails.json: it is a HOST property for a
	// local build and an ARTIFACT decision for packaging. Baking it in made one of
	// the two jobs impossible to build.
	if strings.Contains(projectTags, "webkit2_") {
		t.Errorf("wails.json build:tags = %q carries the ABI tag.\n"+
			"EVERY `wails build` inherits this file, and the two desktop jobs need DIFFERENT "+
			"ABIs: the .deb must link 4.0 (nfpm declares libwebkit2gtk-4.0-37 for Ubuntu <= 23.10) "+
			"while a modern host builds 4.1. Wails appends CLI tags, so it cannot subtract — a tag "+
			"here forces both jobs to the same ABI and breaks the other one. Detect it in `make "+
			"desktop-build` and name it in the packaging job.", projectTags)
	}
}

// TestDesktopBuildDetectsTheABI — the local target must ASK the host, not assume.
//
// Hardcoding webkit2_41 is the mirror image of the wails.json bug: correct on a
// modern host, an immediate pkg-config failure on Ubuntu 22.04 / Debian 12.
func TestDesktopBuildDetectsTheABI(t *testing.T) {
	mk := mustRead(t, "../../Makefile")

	if !strings.Contains(mk, "pkg-config --exists webkit2gtk-4.1") {
		t.Error("Makefile's desktop build does not detect the WebKitGTK ABI — it must ask " +
			"pkg-config which of the two the host actually has")
	}
	if strings.Contains(mk, "-tags orchicon_desktop,production,webkit2_41 -ldflags") {
		t.Error("Makefile hardcodes `webkit2_41` in the local desktop build. That fails on a " +
			"4.0-only host (Ubuntu 22.04, Debian 12) where webkit2gtk-4.1's pkg-config file " +
			"does not exist; the tag must come from the detected ABI.")
	}
	// The packaging target is the one place 4.0 IS the decision, so it says so.
	if !strings.Contains(mk, "-tags orchicon_desktop,production -ldflags") {
		t.Error("the Linux PACKAGING target does not state its tags explicitly. It builds the " +
			".deb that declares libwebkit2gtk-4.0-37, so it must pin 4.0 rather than inherit " +
			"whatever the dev loop detected.")
	}
}

// jsonBuildTags extracts build:tags from wails.json without a JSON dependency
// (the file is two fields and a comment-free object; a regex keeps this test
// readable and independent of the struct the app uses).
func jsonBuildTags(t *testing.T, wailsJSON string) string {
	t.Helper()
	m := regexp.MustCompile(`"build:tags"\s*:\s*"([^"]*)"`).FindStringSubmatch(wailsJSON)
	if m == nil {
		t.Fatal("wails.json has no build:tags")
	}
	return m[1]
}

// tagsFromJob returns the -tags argument of the first `wails build` inside the
// named job. Empty when the job passes none (which is itself meaningful here).
func tagsFromJob(t *testing.T, workflow, jobHeader, command string) string {
	t.Helper()
	start := strings.Index(workflow, jobHeader)
	if start < 0 {
		t.Fatalf("release.yml has no %s job", jobHeader)
	}
	// The job block runs to the next two-space key at the same level.
	rest := workflow[start:]
	if next := regexp.MustCompile(`\n  [a-z0-9_-]+:\n`).FindStringIndex(rest[1:]); next != nil {
		rest = rest[:next[0]+1]
	}
	idx := strings.Index(rest, command)
	if idx < 0 {
		t.Fatalf("the %s job runs no %q", jobHeader, command)
	}
	line := rest[idx:]
	if nl := strings.Index(line, "\n"); nl >= 0 {
		line = line[:nl]
	}
	if m := regexp.MustCompile(`-tags\s+([^\\\s]+)`).FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// TestWebkitChecksAcceptEveryRealSONAME is the BEHAVIOURAL half of the sweep: the
// detection pattern the launcher and the AppImage actually run must MATCH a real
// SONAME from either family, and must not match the impossible one.
//
// String-scanning the files (above) cannot prove this — it only proves nobody
// wrote a bad literal. This proves the checks WORK, which is the property that was
// broken: `libwebkit2gtk-4\.[01]\.so\.37` reads as "either family" and matches
// only 4.0, so on a 4.1-only distro the launcher refused to start a system that
// had WebKitGTK installed.
func TestWebkitChecksAcceptEveryRealSONAME(t *testing.T) {
	// THE PATTERNS AS SHIPPED, copied from the two scripts' single source of truth:
	// their `ldconfig -p | grep -qE ...` line. Kept in sync by the sweep above
	// failing if a script gains a literal that is not real.
	shipped := []struct{ file, pattern string }{
		{"../../deploy/desktop/linux/orchicon-desktop", `libwebkit2gtk-4\.0\.so\.37|libwebkit2gtk-4\.1\.so\.0`},
		{"../../deploy/desktop/appimage/AppRun", `libwebkit2gtk-4\.0\.so\.37|libwebkit2gtk-4\.1\.so\.0`},
	}

	for _, sh := range shipped {
		// The pattern in the file must be the one tested here, or this test is
		// fiction. Extract it from the `grep -qE '<pattern>'` in the script.
		src := mustRead(t, sh.file)
		re := regexp.MustCompile(`grep -qE '([^']+)'`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s has no `grep -qE '<pattern>'` — the check was rewritten and this test "+
				"no longer knows what it asserts", sh.file)
		}
		shippedPattern := m[1]

		compiled, err := regexp.Compile(shippedPattern)
		if err != nil {
			t.Fatalf("%s ships an invalid pattern %q: %v", sh.file, shippedPattern, err)
		}

		// ONLY the webkit2gtk SONAMEs: the launcher's job is to detect the missing
		// WEBVIEW before the loader fails. JavaScriptCore is webkit's own dependency
		// and is not something this check should require separately — the bundle list
		// below is where JSC matters.
		for _, family := range webkitSONAMEs {
			for _, so := range family {
				if !strings.HasPrefix(so, "libwebkit2gtk-") {
					continue
				}
				if !compiled.MatchString(so) {
					t.Errorf("%s: the check does not match %q, which EXISTS. On a host that has "+
						"exactly this library the launcher would report WebKitGTK missing and refuse "+
						"to start the app. pattern=%q", sh.file, so, shippedPattern)
				}
			}
		}
		// And the impossible name must not be matched (it would be a name that can
		// never appear, hiding as support).
		if compiled.MatchString("libwebkit2gtk-4.1.so.37") {
			t.Errorf("%s: the check matches libwebkit2gtk-4.1.so.37, which no distribution ships",
				sh.file)
		}
	}
}

// TestAppImageBundlesBothFamiliesCompletely — the bundle list must carry BOTH
// libraries for each ABI family, because webkit links JavaScriptCore.
//
// The list previously asked for `libwebkit2gtk-4.1.so.37`, which does not exist, so
// on a 4.1-only host the copy loop matched nothing, the directory stayed empty, and
// the build died on its own "no libwebkit2gtk found to bundle" guard — an AppImage
// that could not be built exactly where it is most needed.
func TestAppImageBundlesBothFamiliesCompletely(t *testing.T) {
	src := mustRead(t, "../../deploy/desktop/appimage/build.sh")

	// The `for so in ...` list, split into words.
	forRE := regexp.MustCompile(`for so in ([^;]+); do`)
	m := forRE.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("deploy/desktop/appimage/build.sh has no `for so in ...; do` bundle list")
	}
	listed := strings.Fields(m[1])

	for abi, sonames := range webkitSONAMEs {
		for _, so := range sonames {
			found := false
			for _, want := range listed {
				if want == so {
					found = true
				}
			}
			if !found {
				t.Errorf("the AppImage bundle list does not include %q (WebKitGTK %s).\n"+
					"Both libraries are required: webkit links JavaScriptCore, and a bundle "+
					"without it fails at the loader on the very distro the AppImage exists for.\n"+
					"listed: %v", so, abi, listed)
			}
		}
	}
}
